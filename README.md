# Multi-region serverless POC

This milestone deploys a small writable backend in two AWS Regions:

```text
client -> Global Accelerator -> private ALB -> Go Lambda -> DynamoDB MRSC global table
```

- Application Regions: `us-west-2` and `us-east-1`
- DynamoDB witness Region: `us-east-2`
- Public entry point: Global Accelerator only
- Current scope: build, deploy, and write data
- Deferred scope: automated failover/failback experiments and production hardening

The table uses DynamoDB multi-Region strong consistency with two writable replicas and one witness. The ALBs are internal and live in isolated subnets; Global Accelerator reaches them through its managed VPC interfaces.

## Setup

Install:

- Go 1.25 or newer
- Node.js 22 or 24 and npm
- AWS CLI v2

The AWS CDK CLI is pinned as a project dependency, so a global `cdk` installation is not required.

Install all project dependencies:

```sh
make setup
```

The Go module checksums and npm lock file are committed. `make setup` uses those files to install the pinned toolchain.

### AWS credentials

AWS credentials are not needed for `make setup`, `make build`, `make test`, or the default `make synth`.

Credentials are required for `make bootstrap`, `make deploy`, and `make accel-dns`. Configure credentials with your normal AWS CLI workflow, for example:

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
./node_modules/.bin/cdk synth -c account=000000000000
```

Generated Lambda binaries, CDK assemblies, and npm dependencies are ignored by Git. Clean generated build output with:

```sh
make clean
```

## Deploy

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

Write an item through Global Accelerator:

```sh
curl -X POST "http://GLOBAL_ACCELERATOR_DNS/items" \
  -H 'Content-Type: application/json' \
  -d '{"id":"demo","seq":1,"payload":"hello"}'
```

The response includes `X-Served-By`, which identifies the AWS Region that handled the request.

## Notes

- The POC currently uses HTTP. Do not send sensitive data.
- The API is intentionally minimal and unauthenticated.
- The DynamoDB table has a destroy removal policy. Treat deployed data as disposable.
- Global Accelerator, two ALBs, Lambda invocations, and DynamoDB usage can incur AWS charges.
- Failover and failback controls are intentionally deferred to the next milestone.

See [architecture.md](architecture.md) for the current design and request path.
