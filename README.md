# multi region

Multi-region serverless POC

Global Accelerator -> ALB -> Lambda (Go) -> DynamoDB global table across us-west-2 (primary) and us-east-1 (secondary), with us-east-2 as MRSC witness.


## Layout

- Makefile
- infra/main.go
- lambda/main.go	

