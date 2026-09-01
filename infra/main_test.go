package main

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/jsii-runtime-go"
)

func TestRegionalStackUsesInternalLoadBalancer(t *testing.T) {
	app := awscdk.NewApp(nil)
	stack := NewRegionalStack(app, "TestRegional", &RegionalStackProps{
		StackProps: awscdk.StackProps{
			Env: &awscdk.Environment{
				Account: jsii.String("000000000000"),
				Region:  jsii.String(primaryRegion),
			},
		},
		LambdaAssetPath: t.TempDir(),
	})

	template := assertions.Template_FromStack(stack.Stack, nil)
	template.HasResourceProperties(jsii.String("AWS::ElasticLoadBalancingV2::LoadBalancer"), map[string]any{
		"Scheme": "internal",
	})
}

func TestPrimaryStackSynthesizesMRSCGlobalTable(t *testing.T) {
	app := awscdk.NewApp(nil)
	stack := NewRegionalStack(app, "TestPrimary", &RegionalStackProps{
		StackProps: awscdk.StackProps{
			Env: &awscdk.Environment{
				Account: jsii.String("000000000000"),
				Region:  jsii.String(primaryRegion),
			},
		},
		IsPrimary:       true,
		LambdaAssetPath: t.TempDir(),
	})

	template := assertions.Template_FromStack(stack.Stack, nil)
	template.HasResourceProperties(jsii.String("AWS::DynamoDB::GlobalTable"), map[string]any{
		"MultiRegionConsistency": "STRONG",
		"GlobalTableWitnesses": []any{
			map[string]any{"Region": witnessRegion},
		},
	})
}

func TestEdgeEndpointPreservesClientIP(t *testing.T) {
	app := awscdk.NewApp(nil)
	regional := NewRegionalStack(app, "TestRegionalForEdge", &RegionalStackProps{
		StackProps: awscdk.StackProps{
			Env: &awscdk.Environment{
				Account: jsii.String("000000000000"),
				Region:  jsii.String(primaryRegion),
			},
		},
		LambdaAssetPath: t.TempDir(),
	})
	edge := NewEdgeStack(app, "TestEdge", &EdgeStackProps{
		StackProps: awscdk.StackProps{
			Env: &awscdk.Environment{
				Account: jsii.String("000000000000"),
				Region:  jsii.String(primaryRegion),
			},
		},
		PrimaryAlb: regional.Alb,
		SecondaryAlbArn: "arn:aws:elasticloadbalancing:us-east-1:000000000000:" +
			"loadbalancer/app/secondary/0123456789abcdef",
	})

	template := assertions.Template_FromStack(edge, nil)
	template.ResourceCountIs(jsii.String("AWS::GlobalAccelerator::EndpointGroup"), jsii.Number(2))
	template.HasResourceProperties(jsii.String("AWS::GlobalAccelerator::EndpointGroup"), map[string]any{
		"EndpointConfigurations": []any{
			map[string]any{"ClientIPPreservationEnabled": true},
		},
	})
}
