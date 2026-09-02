# Multi-region serverless POC

This milestone deploys a small writable backend in two AWS Regions:

```text
client -> Global Accelerator TCP/443 -> private ALB HTTPS/443 -> Go Lambda -> DynamoDB MRSC global table
```

- Application Regions: `us-west-2` and `us-east-1`
- DynamoDB witness Region: `us-east-2`
- HTTPS hostname: `www.example.com`
- Public entry point: Global Accelerator only
- Current scope: build, deploy, and write data
- Deferred scope: automated failover/failback experiments and production hardening

The table uses DynamoDB multi-Region strong consistency with two writable replicas and one witness. The ALBs are internal and live in isolated subnets; Global Accelerator reaches them through its managed VPC interfaces. Global Accelerator carries TCP/443 to each ALB, where TLS terminates before the ALB invokes Lambda.

## Setup

Install:

- Go 1.25 or newer
- Node.js 22 or 24 and npm
- AWS CLI v2
- `jq` (used by the API token configuration helper)
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

The deployment identity needs permission to bootstrap and deploy CDK stacks containing IAM, VPC, ALB, Lambda, DynamoDB global table, and Global Accelerator resources. It also needs permission to read CloudFormation stack outputs. The token helper additionally uses `lambda:GetFunctionConfiguration` and `lambda:UpdateFunctionConfiguration`.

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
  -c certArnUsWest2=arn:aws:acm:us-west-2:ACCOUNT_ID:certificate/CERTIFICATE_ID \
  -c certArnUsEast1=arn:aws:acm:us-east-1:ACCOUNT_ID:certificate/CERTIFICATE_ID
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
API_TOKEN=local-integration-token-32-characters \
go test -tags=integration -count=1 .
```

Generated Lambda binaries, CDK assemblies, and npm dependencies are ignored by Git. Clean generated build output with:

```sh
make clean
```

## Deploy

The Makefile supplies the deployment-specific wildcard certificates through these variables:

- `us-west-2`: `CERT_ARN_USW2`
- `us-east-1`: `CERT_ARN_USE1`

For this generalized example, both certificates must cover `www.example.com`; a certificate for `*.example.com` does. Override `CERT_ARN_USW2` or `CERT_ARN_USE1` on the `make` command if the deployment-specific certificates are replaced.

Bootstrap the two Regions that contain CloudFormation stacks. This is normally required once per account and Region:

```sh
AWS_PROFILE=my-profile make bootstrap
```

Deploy the regional stacks first and the Global Accelerator stack second:

```sh
AWS_PROFILE=my-profile make deploy
```

`make deploy` validates the authenticated account and refuses to create the edge stack if it cannot obtain a valid `us-east-1` ALB ARN.

### Configure API authentication

`/items` requires an `Authorization: Bearer <token>` header. `/healthz` remains public so the ALB can perform health checks. The Lambda fails closed with `503` if `API_TOKEN` has not been configured and returns `401` for a missing or incorrect token.

After each deployment, run the helper and enter a token of at least 32 characters at its hidden prompt:

```sh
AWS_PROFILE=my-profile scripts/set-api-token.sh
```

For example, generate a high-entropy token in another terminal with `openssl rand -hex 32`. The script accepts the token as a parameter if needed:

```sh
API_TOKEN="$(openssl rand -hex 32)"
AWS_PROFILE=my-profile scripts/set-api-token.sh "$API_TOKEN"
unset API_TOKEN
```

The prompt form is preferable because it does not put the token in a command argument. The helper discovers both function names from the `Svc-usw2` and `Svc-use1` CloudFormation outputs, preserves the other Lambda environment variables, updates `API_TOKEN`, and waits for both changes to finish. Do not commit a real token.

This is deliberately simple runtime configuration for the current milestone. CDK owns the Lambda environment map, so a later `make deploy` will remove this out-of-band value; rerun the helper after every deployment. For production, move the token to AWS Secrets Manager or replace shared-token authentication with a proper identity system.

Get the Global Accelerator hostname:

```sh
AWS_PROFILE=my-profile make accel-dns
```

Public DNS is managed outside this project. At the domain's authoritative DNS provider, create a CNAME from `www.example.com` to the accelerator hostname returned by `make accel-dns`. If the provider requires address records instead, retrieve Global Accelerator's static addresses with:

```sh
AWS_PROFILE=my-profile make accel-ips
```

Then create an A record for each returned address.

### Smoke test

Write an item through Global Accelerator:

```sh
export API_TOKEN='replace-with-the-deployed-token'
curl -i -X POST "https://www.example.com/items" \
  -H "Authorization: Bearer ${API_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{"id":"demo","seq":1,"payload":"hello"}'
```

Read the item through Global Accelerator:

```sh
curl -i "https://www.example.com/items?id=demo" \
  -H "Authorization: Bearer ${API_TOKEN}"
```

The responses include `X-Served-By`, which identifies the AWS Region that handled each request.

To test before the `www` CNAME has propagated, resolve the HTTPS hostname to one of the accelerator's current IP addresses for this request only:

```sh
ACCELERATOR_DNS="$(AWS_PROFILE=my-profile make accel-dns)"
curl -i "https://www.example.com/items?id=demo" \
  -H "Authorization: Bearer ${API_TOKEN}" \
  --resolve "www.example.com:443:$(dig +short "$ACCELERATOR_DNS" | head -1)"
```

`--resolve` preserves `www.example.com` for TLS certificate validation and HTTP routing while bypassing normal DNS resolution locally.

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
- The authoritative DNS record for `www.example.com` is external to this CDK application.
- The API uses one shared bearer token for `/items`; it does not provide per-user identity, authorization scopes, token expiry, or rate limiting.
- `/healthz` is intentionally unauthenticated for ALB health checks.
- The DynamoDB table has a destroy removal policy. Treat deployed data as disposable.
- Global Accelerator, two ALBs, Lambda invocations, and DynamoDB usage can incur AWS charges.
- Failover and failback controls are intentionally deferred to the next milestone.

See [architecture.md](architecture.md) for the current design and request path.

## References

These are the canonical sources behind the current design and local workflow:

- Global Accelerator: [standard accelerator endpoints](https://docs.aws.amazon.com/global-accelerator/latest/dg/about-endpoints.html), [secure VPC connections](https://docs.aws.amazon.com/global-accelerator/latest/dg/secure-vpc-connections.html), and [private-subnet/client-IP requirements](https://docs.aws.amazon.com/global-accelerator/latest/dg/about-endpoints.sipp-caveats.html). AWS requires an internet gateway attached to a VPC containing a private ALB endpoint, but it does not require public IPs or an internet-gateway route on the private subnets.
- HTTPS and DNS: [ALB HTTPS listeners](https://docs.aws.amazon.com/elasticloadbalancing/latest/application/create-https-listener.html), [regional ACM certificates](https://docs.aws.amazon.com/acm/latest/userguide/acm-overview.html), and [mapping a custom domain to Global Accelerator](https://docs.aws.amazon.com/global-accelerator/latest/dg/dns-addressing-custom-domains.mapping-your-custom-domain.html).
- Private ALB and Lambda: [using Lambda functions as ALB targets](https://docs.aws.amazon.com/elasticloadbalancing/latest/application/lambda-functions.html), including the ALB event/response shape, direct Lambda invocation behavior, health checks, and payload limits.
- Lambda configuration and authentication headers: [Lambda environment variables](https://docs.aws.amazon.com/lambda/latest/dg/configuration-envvars.html), [environment-variable encryption](https://docs.aws.amazon.com/lambda/latest/dg/configuration-envvars-encryption.html), and [ALB request headers in Lambda events](https://docs.aws.amazon.com/elasticloadbalancing/latest/application/lambda-functions.html). AWS recommends Secrets Manager rather than environment variables for credentials such as API keys; the environment-variable approach here is a milestone tradeoff.
- DynamoDB: [global tables and consistency modes](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/GlobalTables.html), [creating MRSC global tables](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/V2globaltables.tutorial.html), and [global-table design guidance](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/bp-global-table-design.html).
- DynamoDB Local: [AWS's Docker and Docker Compose instructions](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/DynamoDBLocal.DownloadingAndRunning.html), [local usage notes and differences](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/DynamoDBLocal.UsageNotes.html), and [AWS SDK for Go v2 custom endpoint configuration](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-endpoints.html).
- AWS CDK: [getting started with the CDK](https://docs.aws.amazon.com/cdk/v2/guide/getting-started.html) and [working with the CDK in Go](https://docs.aws.amazon.com/cdk/v2/guide/work-with-cdk-go.html).
