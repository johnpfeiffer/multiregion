SHELL := /bin/bash
.DEFAULT_GOAL := help

PRIMARY        := us-west-2
SECOND         := us-east-1
SYNTH_ACCOUNT  ?= $(if $(strip $(ACCOUNT)),$(strip $(ACCOUNT)),000000000000)
ACTUAL_ACCOUNT  = $(shell aws sts get-caller-identity --region $(PRIMARY) --query Account --output text 2>/dev/null)
AWS_ACCOUNT     = $(if $(strip $(ACCOUNT)),$(strip $(ACCOUNT)),$(ACTUAL_ACCOUNT))
CDK             := ./node_modules/.bin/cdk
JSII_RUNTIME_PACKAGE_CACHE_ROOT ?= $(CURDIR)/.cache/jsii
DYNAMODB_ENDPOINT ?= http://localhost:8000
LOCAL_TABLE       ?= mrpoc-items-local
LOCAL_REGION      ?= us-west-2
DOMAIN_NAME       ?= www.kittyandbear.com
CERT_ARN_USW2     ?= arn:aws:acm:us-west-2:067872803572:certificate/d3fe4d63-b8ec-4776-8f6c-6b4374832416
CERT_ARN_USE1     ?= arn:aws:acm:us-east-1:067872803572:certificate/cd0d5b68-5dc1-4b41-b138-ad54dc522ba6
CERT_CONTEXT       = -c certArnUsWest2=$(CERT_ARN_USW2) -c certArnUsEast1=$(CERT_ARN_USE1)
export JSII_RUNTIME_PACKAGE_CACHE_ROOT

.PHONY: help setup deps lambda-deps infra-deps node-deps build test fmt synth \
	local-up local-down local-logs test-integration check-aws bootstrap deploy \
	deploy-regions deploy-edge accel-dns accel-ips clean

help:
	@echo "Local:   make setup | make build | make test | make synth"
	@echo "Dynamo:  make local-up | make test-integration | make local-down"
	@echo "AWS:     make bootstrap | make deploy | make accel-dns | make accel-ips"
	@echo "Auth:    scripts/set-api-token.sh [API_TOKEN]"
	@echo "Options: AWS_PROFILE=<profile> ACCOUNT=<12-digit-account-id>"
	@echo "HTTPS:   $(DOMAIN_NAME) (override CERT_ARN_USW2 / CERT_ARN_USE1 if needed)"

setup: deps

deps: lambda-deps infra-deps node-deps

lambda-deps:
	cd lambda && go mod download

infra-deps:
	cd infra && go mod download

node-deps: infra/node_modules/.bin/cdk

infra/node_modules/.bin/cdk: infra/package.json infra/package-lock.json
	cd infra && npm ci

# provided.al2023 expects a binary literally named "bootstrap".
build: lambda-deps
	mkdir -p lambda/build
	cd lambda && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
		go build -trimpath -ldflags="-s -w" -tags lambda.norpc -o build/bootstrap .

test: lambda-deps infra-deps
	cd lambda && go test ./...
	cd infra && go test .

local-up:
	docker compose up -d dynamodb-local

local-down:
	docker compose down

local-logs:
	docker compose logs -f dynamodb-local

test-integration: local-up lambda-deps
	cd lambda && \
		AWS_REGION=$(LOCAL_REGION) \
		AWS_ACCESS_KEY_ID=local \
		AWS_SECRET_ACCESS_KEY=local \
		AWS_EC2_METADATA_DISABLED=true \
		TABLE_NAME=$(LOCAL_TABLE) \
		DYNAMODB_ENDPOINT=$(DYNAMODB_ENDPOINT) \
		API_TOKEN=local-integration-token-32-characters \
		go test -tags=integration -count=1 .

fmt:
	gofmt -w infra/main.go infra/main_test.go lambda/main.go lambda/main_integration_test.go

# Synthesis is credential-free by default. Override SYNTH_ACCOUNT if desired.
synth: build infra-deps node-deps
	cd infra && $(CDK) synth -c account=$(SYNTH_ACCOUNT) $(CERT_CONTEXT)

check-aws:
	@command -v aws >/dev/null || { echo "AWS CLI v2 is required for this target"; exit 1; }
	@test -n "$(ACTUAL_ACCOUNT)" || { echo "No working AWS credentials; configure or log in, then retry"; exit 1; }
	@test -z "$(ACCOUNT)" || test "$(ACCOUNT)" = "$(ACTUAL_ACCOUNT)" || { \
		echo "ACCOUNT=$(ACCOUNT) does not match the authenticated account $(ACTUAL_ACCOUNT)"; exit 1; }

bootstrap: check-aws node-deps
	cd infra && $(CDK) bootstrap aws://$(AWS_ACCOUNT)/$(PRIMARY) aws://$(AWS_ACCOUNT)/$(SECOND) \
		-c account=$(AWS_ACCOUNT) $(CERT_CONTEXT)

deploy-regions: check-aws build infra-deps node-deps
	cd infra && $(CDK) deploy Svc-usw2 Svc-use1 -c account=$(AWS_ACCOUNT) $(CERT_CONTEXT) --require-approval never

# Two phases are intentional: the edge stack needs the secondary ALB ARN.
deploy-edge: check-aws build infra-deps node-deps
	@set -euo pipefail; \
	if ! alb_arn="$$(aws cloudformation describe-stacks \
		--stack-name Svc-use1 --region $(SECOND) \
		--query "Stacks[0].Outputs[?OutputKey=='AlbArn'].OutputValue" --output text)"; then \
		echo "Could not read Svc-use1; deploy the regional stacks first"; exit 1; \
	fi; \
	expected_prefix="arn:aws:elasticloadbalancing:$(SECOND):$(AWS_ACCOUNT):loadbalancer/app/"; \
	if [[ -z "$$alb_arn" || "$$alb_arn" != "$$expected_prefix"* ]]; then \
		echo "Svc-use1 returned an invalid ALB ARN: $$alb_arn"; exit 1; \
	fi; \
	cd infra; \
	$(CDK) deploy Svc-edge -c account=$(AWS_ACCOUNT) $(CERT_CONTEXT) \
		-c albArnUsEast1="$$alb_arn" --require-approval never

deploy:
	$(MAKE) deploy-regions
	$(MAKE) deploy-edge
	@echo "Deployment complete. Run scripts/set-api-token.sh to set API_TOKEN on both regional Lambdas."

accel-dns: check-aws
	@aws globalaccelerator list-accelerators --region $(PRIMARY) \
		--query "Accelerators[?Name=='mrpoc'].DnsName" --output text

accel-ips: check-aws
	@aws globalaccelerator list-accelerators --region $(PRIMARY) \
		--query "Accelerators[?Name=='mrpoc'].IpSets[].IpAddresses[]" --output text

clean:
	rm -rf lambda/build infra/cdk.out
