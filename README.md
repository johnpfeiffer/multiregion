# Multi-region serverless POC

This milestone deploys a small writable backend in two AWS Regions:

```text
client -> Global Accelerator TCP/443 -> private ALB HTTPS/443 -> Go Lambda -> DynamoDB MRSC global table
```

- Application Regions: `us-west-2` and `us-east-1`
- DynamoDB witness Region: `us-east-2`
- HTTPS hostname: `www.kittyandbear.com`
- Public entry point: Global Accelerator only
- Current scope: build, deploy, and write data
- Deferred scope: automated failover/failback experiments and production hardening

The table uses DynamoDB multi-Region strong consistency with two writable replicas and one witness. The ALBs are internal and live in isolated subnets; Global Accelerator reaches them through its managed VPC interfaces. Global Accelerator carries TCP/443 to each ALB, where TLS terminates before the ALB invokes Lambda.

## Setup

Install:

- Go 1.25 or newer
- Node.js 22 or 24 and npm
- AWS CLI v2
- Docker Desktop (only for the DynamoDB Local integration test)

The AWS CDK CLI is pinned as a project dependency, so a global `cdk` installation is not required.

Install all project dependencies:

```sh
make setup
```

The Go module checksums and npm lock file are committed. `make setup` uses those files to install the pinned toolchain.

### AWS credentials

AWS credentials are not needed for `make setup`, `make build`, `make test`, `make test-integration`, or the default `make synth`. The integration target supplies dummy credentials because DynamoDB Local requires credential-shaped values but does not authenticate them.

Credentials are required for `make bootstrap`, `make deploy`, `make accel-dns`, and `make accel-ips`. Configure credentials with your normal AWS CLI workflow, for example:

```sh
aws configure sso
aws sso login --profile my-profile
AWS_PROFILE=my-profile aws sts get-caller-identity
```

The deployment identity needs permission to bootstrap and deploy CDK stacks containing IAM, VPC, ALB, Lambda, DynamoDB global table, and Global Accelerator resources. It also needs permission to read CloudFormation stack outputs.

All AWS-facing Make targets honor `AWS_PROFILE`. They discover the account through STS, or you can provide `ACCOUNT` explicitly. If the explicit account does not match the authenticated account, the Makefile stops before deployment.

## Development

The common local workflow is:

```sh
make build
make test
make synth
```

### Test the Lambda with DynamoDB Local

Run the integration test against a real DynamoDB-compatible process in Docker:

```sh
make test-integration
```

This starts the `amazon/dynamodb-local` container, creates an in-memory table, sends a `POST /items` ALB event through the Lambda handler, and reads the item back through `GET /items`. It tests the request mapping and real AWS SDK calls without using AWS or mocks. It does not emulate global tables, MRSC replication, IAM, ALB, or Global Accelerator.

The container stays running so repeated test runs are fast. Inspect or stop it with:

```sh
make local-logs
make local-down
```

To use a different port or table name:

```sh
make test-integration \
  DYNAMODB_ENDPOINT=http://localhost:9000 \
  LOCAL_TABLE=my-local-table
```

If the endpoint port changes, update the host port in `compose.yaml` as well. `DYNAMODB_ENDPOINT` is only honored when explicitly set; deployed Lambdas continue to use the normal regional DynamoDB endpoint.

`make synth` uses the placeholder account `000000000000`, so it does not require AWS access. To synthesize with a specific account:

```sh
make synth SYNTH_ACCOUNT=123456789012
```

To run the toolchain manually:

```sh
cd lambda
go mod download
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -ldflags="-s -w" -tags lambda.norpc -o build/bootstrap .

cd ../infra
go mod download
npm ci
./node_modules/.bin/cdk synth \
  -c account=000000000000 \
  -c certArnUsWest2=arn:aws:acm:us-west-2:067872803572:certificate/d3fe4d63-b8ec-4776-8f6c-6b4374832416 \
  -c certArnUsEast1=arn:aws:acm:us-east-1:067872803572:certificate/cd0d5b68-5dc1-4b41-b138-ad54dc522ba6
```

The local integration path can also be run manually:

```sh
docker compose up -d dynamodb-local
cd lambda
AWS_REGION=us-west-2 \
AWS_ACCESS_KEY_ID=local \
AWS_SECRET_ACCESS_KEY=local \
AWS_EC2_METADATA_DISABLED=true \
TABLE_NAME=mrpoc-items-local \
DYNAMODB_ENDPOINT=http://localhost:8000 \
go test -tags=integration -count=1 .
```

Generated Lambda binaries, CDK assemblies, and npm dependencies are ignored by Git. Clean generated build output with:

```sh
make clean
```

## Deploy

The Makefile defaults to the existing wildcard certificates:

- `us-west-2`: `arn:aws:acm:us-west-2:067872803572:certificate/d3fe4d63-b8ec-4776-8f6c-6b4374832416`
- `us-east-1`: `arn:aws:acm:us-east-1:067872803572:certificate/cd0d5b68-5dc1-4b41-b138-ad54dc522ba6`

Both certificates are issued for `*.kittyandbear.com`, which covers `www.kittyandbear.com`. Override `CERT_ARN_USW2` or `CERT_ARN_USE1` on the `make` command if the certificates are replaced.

Bootstrap the two Regions that contain CloudFormation stacks. This is normally required once per account and Region:

```sh
AWS_PROFILE=my-profile make bootstrap
```

Deploy the regional stacks first and the Global Accelerator stack second:

```sh
AWS_PROFILE=my-profile make deploy
```

`make deploy` validates the authenticated account and refuses to create the edge stack if it cannot obtain a valid `us-east-1` ALB ARN.

Get the Global Accelerator hostname:

```sh
AWS_PROFILE=my-profile make accel-dns
```

The active AWS account does not contain a Route 53 hosted zone for `kittyandbear.com`, so public DNS is not changed by this project. At the domain's authoritative DNS provider, create a CNAME from `www.kittyandbear.com` to the accelerator hostname returned by `make accel-dns`. If the provider requires address records instead, retrieve Global Accelerator's static addresses with:

```sh
AWS_PROFILE=my-profile make accel-ips
```

Then create an A record for each returned address.

### Smoke test

Write an item through Global Accelerator:

```sh
curl -i -X POST "https://www.kittyandbear.com/items" \
  -H 'Content-Type: application/json' \
  -d '{"id":"demo","seq":1,"payload":"hello"}'
```

Read the item through Global Accelerator:

```sh
curl -i "https://www.kittyandbear.com/items?id=demo"
```

The responses include `X-Served-By`, which identifies the AWS Region that handled each request.

To test before the `www` CNAME has propagated, resolve the HTTPS hostname to one of the accelerator's current IP addresses for this request only:

```sh
curl -i "https://www.kittyandbear.com/items?id=demo" \
  --resolve "www.kittyandbear.com:443:$(dig +short ac7649bf9cd8b2519.awsglobalaccelerator.com | head -1)"
```

`--resolve` preserves `www.kittyandbear.com` for TLS certificate validation and HTTP routing while bypassing normal DNS resolution locally.

Verify the stored items directly against the `us-west-2` DynamoDB replica:

```sh
AWS_PROFILE=my-profile aws dynamodb scan \
  --table-name mrpoc-items \
  --region us-west-2 \
  --consistent-read
```

For a specific partition key, use `query` instead of scanning the whole table:

```sh
AWS_PROFILE=my-profile aws dynamodb query \
  --table-name mrpoc-items \
  --region us-west-2 \
  --key-condition-expression "#id = :id" \
  --expression-attribute-names '{"#id":"id"}' \
  --expression-attribute-values '{":id":{"S":"demo"}}' \
  --consistent-read
```

Change `--region` to `us-east-1` to inspect the other replica.

## Notes

- Public requests use HTTPS, with TLS 1.2 or 1.3 terminated independently at each regional ALB.
- The authoritative DNS record for `www.kittyandbear.com` is external to this CDK application.
- The API is intentionally minimal and unauthenticated.
- The DynamoDB table has a destroy removal policy. Treat deployed data as disposable.
- Global Accelerator, two ALBs, Lambda invocations, and DynamoDB usage can incur AWS charges.
- Failover and failback controls are intentionally deferred to the next milestone.

See [architecture.md](architecture.md) for the current design and request path.

## References

These are the canonical sources behind the current design and local workflow:

- Global Accelerator: [standard accelerator endpoints](https://docs.aws.amazon.com/global-accelerator/latest/dg/about-endpoints.html), [secure VPC connections](https://docs.aws.amazon.com/global-accelerator/latest/dg/secure-vpc-connections.html), and [private-subnet/client-IP requirements](https://docs.aws.amazon.com/global-accelerator/latest/dg/about-endpoints.sipp-caveats.html). AWS requires an internet gateway attached to a VPC containing a private ALB endpoint, but it does not require public IPs or an internet-gateway route on the private subnets.
- HTTPS and DNS: [ALB HTTPS listeners](https://docs.aws.amazon.com/elasticloadbalancing/latest/application/create-https-listener.html), [regional ACM certificates](https://docs.aws.amazon.com/acm/latest/userguide/acm-overview.html), and [mapping a custom domain to Global Accelerator](https://docs.aws.amazon.com/global-accelerator/latest/dg/dns-addressing-custom-domains.mapping-your-custom-domain.html).
- Private ALB and Lambda: [using Lambda functions as ALB targets](https://docs.aws.amazon.com/elasticloadbalancing/latest/application/lambda-functions.html), including the ALB event/response shape, direct Lambda invocation behavior, health checks, and payload limits.
- DynamoDB: [global tables and consistency modes](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/GlobalTables.html), [creating MRSC global tables](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/V2globaltables.tutorial.html), and [global-table design guidance](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/bp-global-table-design.html).
- DynamoDB Local: [AWS's Docker and Docker Compose instructions](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/DynamoDBLocal.DownloadingAndRunning.html), [local usage notes and differences](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/DynamoDBLocal.UsageNotes.html), and [AWS SDK for Go v2 custom endpoint configuration](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-endpoints.html).
- AWS CDK: [getting started with the CDK](https://docs.aws.amazon.com/cdk/v2/guide/getting-started.html) and [working with the CDK in Go](https://docs.aws.amazon.com/cdk/v2/guide/work-with-cdk-go.html).
