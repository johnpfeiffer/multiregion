ACCOUNT ?= $(shell aws sts get-caller-identity --query Account --output text)
PRIMARY  := us-west-2
SECOND   := us-east-1

.PHONY: build deploy deploy-regions deploy-edge bootstrap accel-dns dial-down dial-up clean

# provided.al2023 expects a binary literally named "bootstrap".
# CGO off + arm64 -> ~10-20ms init, which is why Go needs no SnapStart equivalent.
build:
	cd lambda && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
		go build -ldflags="-s -w" -tags lambda.norpc -o build/bootstrap .

bootstrap:
	cd infra && cdk bootstrap aws://$(ACCOUNT)/$(PRIMARY) aws://$(ACCOUNT)/$(SECOND) aws://$(ACCOUNT)/us-east-2

deploy-regions: build
	cd infra && cdk deploy Svc-usw2 Svc-use1 -c account=$(ACCOUNT) --require-approval never

# Two-phase on purpose: the edge stack lives in us-west-2 (Global Accelerator's
# control plane requirement) and needs the us-east-1 ALB ARN as a plain string.
deploy-edge:
	$(eval ALB_USE1 := $(shell aws cloudformation describe-stacks \
		--stack-name Svc-use1 --region $(SECOND) \
		--query "Stacks[0].Outputs[?OutputKey=='AlbArn'].OutputValue" --output text))
	cd infra && cdk deploy Svc-edge -c account=$(ACCOUNT) -c albArnUsEast1=$(ALB_USE1) --require-approval never

deploy: deploy-regions deploy-edge

accel-dns:
	@aws globalaccelerator list-accelerators --region us-west-2 \
		--query "Accelerators[?Name=='mrpoc'].DnsName" --output text

# Graduated: the dial applies only to the raffic GA had already routed to that group, so run the canary from >1 region.
dial-down:
	@aws globalaccelerator update-endpoint-group --region us-west-2 \
		--endpoint-group-arn $(EG_ARN) --traffic-dial-percentage 0

dial-up:
	@aws globalaccelerator update-endpoint-group --region us-west-2 \
		--endpoint-group-arn $(EG_ARN) --traffic-dial-percentage 100

clean:
	rm -rf lambda/build infra/cdk.out

