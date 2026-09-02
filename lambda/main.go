// ALB -> Lambda handler for the multi-region POC.
//
// Design notes that matter for the experiment, not just for the code:
//
//  1. Every response carries X-Served-By: <region>. Without this the failover
//     canary cannot tell which region answered, and "did it fail over" becomes
//     unmeasurable.
//
//  2. Writes carry a monotonic sequence supplied by the client. Scanning the
//     surviving replica for gaps after a kill is how you measure RPO. Do not
//     generate the sequence server-side.
//
//  3. Handler errors become ALB 502s with no body. Every expected condition
//     returns an explicit status code instead.
//
//  4. ALB caps both request and response bodies at 1 MB for Lambda targets.
//     Query results are capped below that on purpose.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

var (
	ddb        *dynamodb.Client
	region     = os.Getenv("AWS_REGION")
	table      = os.Getenv("TABLE_NAME")
	pkAttr     = envOr("PK_ATTR", "id")
	skAttr     = envOr("SK_ATTR", "occurred_at")
	deepHealth = envOr("DEEP_HEALTH", "true") == "true"
	initErr    error
)

// Client is built once per execution environment and reused across warm
// invocations -- the one piece of connection reuse Lambda does give you.
func init() {
	cfg, err := awscfg.LoadDefaultConfig(context.Background())
	if err != nil {
		initErr = err
		return
	}

	var options []func(*dynamodb.Options)
	if endpoint := os.Getenv("DYNAMODB_ENDPOINT"); endpoint != "" {
		options = append(options, func(o *dynamodb.Options) {
			o.BaseEndpoint = aws.String(endpoint)
		})
	}
	ddb = dynamodb.NewFromConfig(cfg, options...)
}

type item struct {
	ID         string `json:"id"`
	OccurredAt string `json:"occurred_at"`
	Seq        int64  `json:"seq"`
	Payload    string `json:"payload,omitempty"`
	WrittenIn  string `json:"written_in"`
}

func handler(ctx context.Context, req events.ALBTargetGroupRequest) (events.ALBTargetGroupResponse, error) {
	switch {
	case req.Path == "/healthz":
		return health(ctx)
	case req.Path == "/items" && req.HTTPMethod == "POST":
		return putItem(ctx, req)
	case req.Path == "/items" && req.HTTPMethod == "GET":
		return queryItems(ctx, req)
	default:
		return respond(404, map[string]string{"error": "not found"})
	}
}

// health is the entire failover signal. Global Accelerator does not probe ALB
// endpoints itself -- it inherits the target group's health state, and the
// target group probes this path.
//
// DEEP_HEALTH=true  -> a data-layer failure takes the region out of rotation.
// DEEP_HEALTH=false -> only a dead process does.
//
// Both are defensible; they are different experiments. Deep checks can flap
// under dependency latency and can take out both regions at once (at which
// point GA fails open and sprays traffic everywhere). Run both, record both.
func health(ctx context.Context) (events.ALBTargetGroupResponse, error) {
	if initErr != nil {
		return respond(503, map[string]string{"status": "init_failed", "region": region})
	}
	if !deepHealth {
		return respond(200, map[string]string{"status": "ok", "depth": "shallow", "region": region})
	}

	// Short, independent budget. A health check must fail fast rather than
	// inherit the caller's timeout and pile up concurrent probes.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	_, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(table),
		Key: map[string]ddbtypes.AttributeValue{
			pkAttr: &ddbtypes.AttributeValueMemberS{Value: "__healthcheck__"},
			skAttr: &ddbtypes.AttributeValueMemberS{Value: "1970-01-01T00:00:00Z"},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		slog.Error("deep health check failed", "region", region, "err", err)
		return respond(503, map[string]string{"status": "degraded", "depth": "deep", "region": region})
	}
	return respond(200, map[string]string{"status": "ok", "depth": "deep", "region": region})
}

func putItem(ctx context.Context, req events.ALBTargetGroupRequest) (events.ALBTargetGroupResponse, error) {
	var in item
	if err := json.Unmarshal([]byte(req.Body), &in); err != nil {
		return respond(400, map[string]string{"error": "invalid json"})
	}
	if in.ID == "" {
		return respond(400, map[string]string{"error": "id required"})
	}
	if in.OccurredAt == "" {
		in.OccurredAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	in.WrittenIn = region

	_, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(table),
		Item: map[string]ddbtypes.AttributeValue{
			pkAttr:       &ddbtypes.AttributeValueMemberS{Value: in.ID},
			skAttr:       &ddbtypes.AttributeValueMemberS{Value: in.OccurredAt},
			"seq":        &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(in.Seq, 10)},
			"payload":    &ddbtypes.AttributeValueMemberS{Value: in.Payload},
			"written_in": &ddbtypes.AttributeValueMemberS{Value: region},
		},
	})
	if err != nil {
		// Under MRSC, concurrent conflicting writes surface here as
		// ReplicatedWriteConflictException. It is retryable -- 409 tells the
		// canary to retry rather than counting a lost write.
		var conflict *ddbtypes.ReplicatedWriteConflictException
		if errors.As(err, &conflict) {
			return respond(409, map[string]string{"error": "write conflict", "region": region})
		}
		slog.Error("put failed", "region", region, "err", err)
		return respond(502, map[string]string{"error": "write failed", "region": region})
	}
	return respond(201, in)
}

func queryItems(ctx context.Context, req events.ALBTargetGroupRequest) (events.ALBTargetGroupResponse, error) {
	id := req.QueryStringParameters["id"]
	if id == "" {
		return respond(400, map[string]string{"error": "id query param required"})
	}
	limit := int32(100)
	if v := req.QueryStringParameters["limit"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = int32(n)
		}
	}

	out, err := ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(table),
		KeyConditionExpression: aws.String("#pk = :id"),
		// #pk aliasing is habit worth keeping: if you ever rename the sort key to
		// "date", "status", "timestamp" or similar, it is a reserved word and the
		// raw name will fail at runtime, not at deploy time.
		ExpressionAttributeNames:  map[string]string{"#pk": pkAttr},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":id": &ddbtypes.AttributeValueMemberS{Value: id}},
		Limit:                     aws.Int32(limit),
		// Meaningful only under MRSC. Under MREC this is a local strong read and
		// says nothing about the other region.
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		slog.Error("query failed", "region", region, "err", err)
		return respond(502, map[string]string{"error": "read failed", "region": region})
	}

	items := make([]map[string]any, 0, len(out.Items))
	for _, it := range out.Items {
		items = append(items, map[string]any{
			pkAttr:       str(it[pkAttr]),
			skAttr:       str(it[skAttr]),
			"seq":        num(it["seq"]),
			"written_in": str(it["written_in"]),
		})
	}
	return respond(200, map[string]any{"region": region, "count": len(items), "items": items})
}

func respond(code int, body any) (events.ALBTargetGroupResponse, error) {
	b, err := json.Marshal(body)
	if err != nil {
		b = []byte(`{"error":"marshal failed"}`)
		code = 500
	}
	return events.ALBTargetGroupResponse{
		StatusCode:        code,
		StatusDescription: fmt.Sprintf("%d %s", code, statusText(code)),
		Headers: map[string]string{
			"Content-Type":  "application/json",
			"X-Served-By":   region,
			"Cache-Control": "no-store",
		},
		Body:            string(b),
		IsBase64Encoded: false,
	}, nil
}

func statusText(c int) string {
	switch c {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 400:
		return "Bad Request"
	case 404:
		return "Not Found"
	case 409:
		return "Conflict"
	case 502:
		return "Bad Gateway"
	case 503:
		return "Service Unavailable"
	default:
		return "Error"
	}
}

func str(v ddbtypes.AttributeValue) string {
	if s, ok := v.(*ddbtypes.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}

func num(v ddbtypes.AttributeValue) string {
	if n, ok := v.(*ddbtypes.AttributeValueMemberN); ok {
		return n.Value
	}
	return ""
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() { lambda.Start(handler) }
