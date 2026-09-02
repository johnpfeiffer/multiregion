//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestHandlerWritesAndReadsDynamoDBLocal(t *testing.T) {
	if initErr != nil {
		t.Fatalf("initialize DynamoDB client: %v", initErr)
	}
	if table == "" {
		t.Fatal("TABLE_NAME must be set for integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	createLocalTable(t, ctx)

	const (
		id         = "integration-test"
		occurredAt = "2026-09-01T12:00:00Z"
	)
	put, err := handler(ctx, events.ALBTargetGroupRequest{
		HTTPMethod: "POST",
		Path:       "/items",
		Body:       fmt.Sprintf(`{"id":%q,"occurred_at":%q,"seq":42,"payload":"local"}`, id, occurredAt),
	})
	if err != nil {
		t.Fatalf("POST /items: %v", err)
	}
	if put.StatusCode != 201 {
		t.Fatalf("POST /items status = %d, body = %s", put.StatusCode, put.Body)
	}

	get, err := handler(ctx, events.ALBTargetGroupRequest{
		HTTPMethod:            "GET",
		Path:                  "/items",
		QueryStringParameters: map[string]string{"id": id},
	})
	if err != nil {
		t.Fatalf("GET /items: %v", err)
	}
	if get.StatusCode != 200 {
		t.Fatalf("GET /items status = %d, body = %s", get.StatusCode, get.Body)
	}

	var result struct {
		Count int `json:"count"`
		Items []struct {
			ID         string `json:"id"`
			OccurredAt string `json:"occurred_at"`
			Seq        string `json:"seq"`
			WrittenIn  string `json:"written_in"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(get.Body), &result); err != nil {
		t.Fatalf("decode GET /items response: %v", err)
	}
	if result.Count != 1 || len(result.Items) != 1 {
		t.Fatalf("GET /items returned %#v", result)
	}
	got := result.Items[0]
	if got.ID != id || got.OccurredAt != occurredAt || got.Seq != "42" || got.WrittenIn != region {
		t.Fatalf("GET /items item = %#v", got)
	}
}

func createLocalTable(t *testing.T, ctx context.Context) {
	t.Helper()

	input := &dynamodb.CreateTableInput{
		TableName: aws.String(table),
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: aws.String(pkAttr), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String(skAttr), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []ddbtypes.KeySchemaElement{
			{AttributeName: aws.String(pkAttr), KeyType: ddbtypes.KeyTypeHash},
			{AttributeName: aws.String(skAttr), KeyType: ddbtypes.KeyTypeRange},
		},
		BillingMode: ddbtypes.BillingModePayPerRequest,
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err := ddb.CreateTable(ctx, input)
		if err == nil {
			break
		}
		var exists *ddbtypes.ResourceInUseException
		if errors.As(err, &exists) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("create local DynamoDB table: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	waiter := dynamodb.NewTableExistsWaiter(ddb)
	if err := waiter.Wait(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(table)}, 10*time.Second); err != nil {
		t.Fatalf("wait for local DynamoDB table: %v", err)
	}
}
