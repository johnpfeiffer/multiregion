// Multi-region serverless POC:
//
//	Global Accelerator -> ALB (per region) -> Lambda (Go) -> DynamoDB global table
//
// Stack layout:
//
//	RegionalStack  x2  (us-west-2 primary, us-east-1 secondary)
//	EdgeStack      x1  (MUST be us-west-2 -- Global Accelerator's control plane
//	                    only accepts create/update in us-west-2)
//
// Deploy order matters because the edge stack needs the secondary ALB ARN:
//  1. cdk deploy Svc-usw2 Svc-use1
//  2. cdk deploy Svc-edge -c albArnUsEast1=<arn from step 1 output>
package main

import (
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsdynamodb"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	elbtargets "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2targets"
	ga "github.com/aws/aws-cdk-go/awscdk/v2/awsglobalaccelerator"
	gaendpoints "github.com/aws/aws-cdk-go/awscdk/v2/awsglobalacceleratorendpoints"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslambda"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"
)

const (
	// Explicit table name: the secondary region has no TableV2 construct (it only
	// holds a replica), so it cannot call table.TableName(). A fixed name is the
	// simplest way to keep both regions pointing at the same logical table.
	tableName = "mrpoc-items"

	// MRSC requires all three regions to be inside one DynamoDB "region set".
	// The US set is us-east-1 / us-east-2 / us-west-2. us-west-1 is NOT in it.
	primaryRegion   = "us-west-2"
	secondaryRegion = "us-east-1"
	witnessRegion   = "us-east-2" // witness = consensus only, no readable replica

	// "date" is a DynamoDB reserved word. Using it as an attribute name forces
	// ExpressionAttributeNames (#d) in every single expression forever.
	partitionKeyAttr = "id"
	sortKeyAttr      = "occurred_at" // ISO-8601 string -> lexicographic sort == chronological
)

// ---------------------------------------------------------------------------
// Regional stack
// ---------------------------------------------------------------------------

type RegionalStackProps struct {
	awscdk.StackProps
	IsPrimary       bool
	LambdaAssetPath string
}

type RegionalStack struct {
	awscdk.Stack
	Alb elbv2.ApplicationLoadBalancer
}

func NewRegionalStack(scope constructs.Construct, id string, props *RegionalStackProps) *RegionalStack {
	stack := awscdk.NewStack(scope, jsii.String(id), &props.StackProps)
	region := *stack.Region()
	lambdaAssetPath := props.LambdaAssetPath
	if lambdaAssetPath == "" {
		lambdaAssetPath = "../lambda/build"
	}

	// -- Data ----------------------------------------------------------------
	// Only the primary stack declares the table. TableV2 manages replicas via the
	// native AWS::DynamoDB::GlobalTable resource, so the secondary region must NOT
	// declare its own table or the two stacks will fight over the same resource.
	if props.IsPrimary {
		awsdynamodb.NewTableV2(stack, jsii.String("Table"), &awsdynamodb.TablePropsV2{
			TableName:    jsii.String(tableName),
			PartitionKey: &awsdynamodb.Attribute{Name: jsii.String(partitionKeyAttr), Type: awsdynamodb.AttributeType_STRING},
			SortKey:      &awsdynamodb.Attribute{Name: jsii.String(sortKeyAttr), Type: awsdynamodb.AttributeType_STRING},
			Billing:      awsdynamodb.Billing_OnDemand(nil),

			// Multi-Region Strong Consistency: 2 replicas + 1 witness.
			// Trade-offs you are accepting by choosing STRONG:
			//   - no transactions (TransactWriteItems / TransactGetItems)
			//   - no TTL, no local secondary indexes
			//   - concurrent conflicting writes surface as ReplicatedWriteConflictException
			//   - the table must be empty when MRSC is enabled
			// Swap to MultiRegionConsistency_EVENTUAL + drop WitnessRegion for MREC.
			MultiRegionConsistency: awsdynamodb.MultiRegionConsistency_STRONG,
			Replicas: &[]*awsdynamodb.ReplicaTableProps{
				{Region: jsii.String(secondaryRegion)},
			},
			WitnessRegion: jsii.String(witnessRegion),

			PointInTimeRecoverySpecification: &awsdynamodb.PointInTimeRecoverySpecification{
				PointInTimeRecoveryEnabled: jsii.Bool(true),
			},
			RemovalPolicy: awscdk.RemovalPolicy_DESTROY, // POC only
		})
	}

	// -- Network -------------------------------------------------------------
	// The ALB is internal so Global Accelerator is the only public entry point.
	// Lambda targets are invoked over the Lambda control plane, so neither the
	// load balancer nor the function needs NAT-backed internet egress.
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), &awsec2.VpcProps{
		MaxAzs:      jsii.Number(2),
		NatGateways: jsii.Number(0),
		SubnetConfiguration: &[]*awsec2.SubnetConfiguration{
			{Name: jsii.String("application"), SubnetType: awsec2.SubnetType_PRIVATE_ISOLATED, CidrMask: jsii.Number(24)},
		},
	})

	// -- Compute -------------------------------------------------------------
	fn := awslambda.NewFunction(stack, jsii.String("Api"), &awslambda.FunctionProps{
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String(lambdaAssetPath), nil),
		MemorySize:   jsii.Number(512),
		Timeout:      awscdk.Duration_Seconds(jsii.Number(10)),
		Environment: &map[string]*string{
			"TABLE_NAME": jsii.String(tableName),
			"PK_ATTR":    jsii.String(partitionKeyAttr),
			"SK_ATTR":    jsii.String(sortKeyAttr),
			// Keep the data store in the endpoint health signal for this milestone.
			// Shallow-vs-deep health experiments are deferred with failover testing.
			"DEEP_HEALTH": jsii.String("true"),
		},
		LoggingFormat: awslambda.LoggingFormat_JSON,
	})

	awslogs.NewLogRetention(stack, jsii.String("ApiLogRetention"), &awslogs.LogRetentionProps{
		LogGroupName: jsii.String(fmt.Sprintf("/aws/lambda/%s", *fn.FunctionName())),
		Retention:    awslogs.RetentionDays_ONE_WEEK,
	})

	// Grant against the ARN rather than the construct: the secondary stack has no
	// table object, and both regions address the same table name locally.
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Actions: jsii.Strings("dynamodb:PutItem", "dynamodb:GetItem", "dynamodb:Query", "dynamodb:DescribeTable"),
		Resources: jsii.Strings(
			fmt.Sprintf("arn:aws:dynamodb:%s:%s:table/%s", region, *stack.Account(), tableName),
		),
	}))

	// -- Edge (regional) -----------------------------------------------------
	alb := elbv2.NewApplicationLoadBalancer(stack, jsii.String("Alb"), &elbv2.ApplicationLoadBalancerProps{
		Vpc:            vpc,
		InternetFacing: jsii.Bool(false),
	})

	// CRITICAL: health checks are DISABLED by default on Lambda target groups.
	// Global Accelerator does not health check ALB endpoints itself -- it inherits
	// the load balancer's health state. Leave this off and GA will consider the
	// region permanently healthy, and no failover will ever fire.
	tg := elbv2.NewApplicationTargetGroup(stack, jsii.String("Tg"), &elbv2.ApplicationTargetGroupProps{
		TargetType: elbv2.TargetType_LAMBDA,
		Targets: &[]elbv2.IApplicationLoadBalancerTarget{
			elbtargets.NewLambdaTarget(fn),
		},
		HealthCheck: &elbv2.HealthCheck{
			Enabled:                 jsii.Bool(true),
			Path:                    jsii.String("/healthz"),
			Interval:                awscdk.Duration_Seconds(jsii.Number(10)),
			Timeout:                 awscdk.Duration_Seconds(jsii.Number(5)),
			HealthyThresholdCount:   jsii.Number(2),
			UnhealthyThresholdCount: jsii.Number(2),
			HealthyHttpCodes:        jsii.String("200"),
		},
	})

	listener := alb.AddListener(jsii.String("Http"), &elbv2.BaseApplicationListenerProps{
		Port:     jsii.Number(80), // POC. Real: 443 + ACM cert per region.
		Protocol: elbv2.ApplicationProtocol_HTTP,
		Open:     jsii.Bool(true),
	})
	listener.AddTargetGroups(jsii.String("Default"), &elbv2.AddApplicationTargetGroupsProps{
		TargetGroups: &[]elbv2.IApplicationTargetGroup{tg},
	})

	awscdk.NewCfnOutput(stack, jsii.String("AlbArn"), &awscdk.CfnOutputProps{Value: alb.LoadBalancerArn()})
	awscdk.NewCfnOutput(stack, jsii.String("AlbDns"), &awscdk.CfnOutputProps{Value: alb.LoadBalancerDnsName()})

	return &RegionalStack{Stack: stack, Alb: alb}
}

// ---------------------------------------------------------------------------
// Edge stack (Global Accelerator) -- must be us-west-2
// ---------------------------------------------------------------------------

type EdgeStackProps struct {
	awscdk.StackProps
	PrimaryAlb      elbv2.IApplicationLoadBalancer
	SecondaryAlbArn string // may be empty on first deploy
}

func NewEdgeStack(scope constructs.Construct, id string, props *EdgeStackProps) awscdk.Stack {
	stack := awscdk.NewStack(scope, jsii.String(id), &props.StackProps)

	accel := ga.NewAccelerator(stack, jsii.String("Accel"), &ga.AcceleratorProps{
		AcceleratorName: jsii.String("mrpoc"),
		Enabled:         jsii.Bool(true),
	})

	listener := accel.AddListener(jsii.String("Http"), &ga.ListenerOptions{
		PortRanges: &[]*ga.PortRange{
			{FromPort: jsii.Number(80), ToPort: jsii.Number(80)},
		},
		// NONE means every new connection is routed independently. SOURCE_IP pins
		// a client to an endpoint, which will make your failover measurements
		// noisier and slower to converge. Keep NONE for the experiment.
		ClientAffinity: ga.ClientAffinity_NONE,
	})

	albOpts := &gaendpoints.ApplicationLoadBalancerEndpointOptions{
		Weight: jsii.Number(128),
		// Internal ALB endpoints always use client IP preservation. Global
		// Accelerator reaches them through managed ENIs in the VPC.
		PreserveClientIp: jsii.Bool(true),
	}

	// Both Regions are eligible for normal routing in this milestone. Deliberate
	// traffic shifting and failover experiments are deferred.
	listener.AddEndpointGroup(jsii.String("UsWest2"), &ga.EndpointGroupOptions{
		Region:                jsii.String(primaryRegion),
		TrafficDialPercentage: jsii.Number(100),
		Endpoints: &[]ga.IEndpoint{
			gaendpoints.NewApplicationLoadBalancerEndpoint(props.PrimaryAlb, albOpts),
		},
	})

	if props.SecondaryAlbArn != "" {
		// The secondary ALB is owned by a stack in another Region. Represent its
		// endpoint directly by ARN so the edge stack does not import or mutate the
		// secondary VPC's security group.
		ga.NewCfnEndpointGroup(stack, jsii.String("UsEast1"), &ga.CfnEndpointGroupProps{
			EndpointGroupRegion:   jsii.String(secondaryRegion),
			ListenerArn:           listener.ListenerArn(),
			TrafficDialPercentage: jsii.Number(100),
			EndpointConfigurations: []interface{}{
				&ga.CfnEndpointGroup_EndpointConfigurationProperty{
					EndpointId:                  jsii.String(props.SecondaryAlbArn),
					ClientIpPreservationEnabled: jsii.Bool(true),
					Weight:                      jsii.Number(128),
				},
			},
		})
	}

	awscdk.NewCfnOutput(stack, jsii.String("AcceleratorDns"), &awscdk.CfnOutputProps{Value: accel.DnsName()})
	return stack
}

// ---------------------------------------------------------------------------

func main() {
	defer jsii.Close()
	app := awscdk.NewApp(nil)

	account := jsii.String(mustCtx(app, "account"))

	primary := NewRegionalStack(app, "Svc-usw2", &RegionalStackProps{
		StackProps: awscdk.StackProps{
			Env:                   &awscdk.Environment{Account: account, Region: jsii.String(primaryRegion)},
			CrossRegionReferences: jsii.Bool(true),
		},
		IsPrimary: true,
	})

	NewRegionalStack(app, "Svc-use1", &RegionalStackProps{
		StackProps: awscdk.StackProps{
			Env:                   &awscdk.Environment{Account: account, Region: jsii.String(secondaryRegion)},
			CrossRegionReferences: jsii.Bool(true),
		},
		IsPrimary: false,
	})

	NewEdgeStack(app, "Svc-edge", &EdgeStackProps{
		StackProps: awscdk.StackProps{
			Env:                   &awscdk.Environment{Account: account, Region: jsii.String(primaryRegion)},
			CrossRegionReferences: jsii.Bool(true),
		},
		PrimaryAlb:      primary.Alb,
		SecondaryAlbArn: ctxOr(app, "albArnUsEast1", ""),
	})

	app.Synth(nil)
}

func ctxOr(app awscdk.App, key, def string) string {
	if v, ok := app.Node().TryGetContext(jsii.String(key)).(string); ok && v != "" {
		return v
	}
	return def
}

func mustCtx(app awscdk.App, key string) string {
	v := ctxOr(app, key, "")
	if v == "" {
		panic(fmt.Sprintf("missing required context: -c %s=<value>", key))
	}
	return v
}
