//go:build integration

// Integration tests exercising the real AWS-SDK-backed store implementations
// (dynamoKeyStore, s3ArchiveStore, dynamoIndexStore) and the full ingest
// handler against LocalStack. Run with:
//
//	docker compose up -d localstack
//	go test -tags=integration ./...
package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const localstackEndpoint = "http://localhost:4566"

func newAWS(t *testing.T) (*dynamodb.Client, *s3.Client) {
	t.Helper()
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	ddb := dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		o.BaseEndpoint = aws.String(localstackEndpoint)
	})
	s3c := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(localstackEndpoint)
		o.UsePathStyle = true // LocalStack requires path-style addressing
	})
	return ddb, s3c
}

func createBucket(t *testing.T, c *s3.Client, name string) {
	t.Helper()
	_, err := c.CreateBucket(context.Background(), &s3.CreateBucketInput{
		Bucket: aws.String(name),
	})
	if err != nil {
		t.Fatalf("create bucket %s: %v", name, err)
	}
}

func createTables(t *testing.T, c *dynamodb.Client, projects, events string) {
	t.Helper()
	ctx := context.Background()

	_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(projects),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("project_id"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("project_id"), KeyType: types.KeyTypeHash},
		},
	})
	if err != nil {
		t.Fatalf("create projects table: %v", err)
	}

	_, err = c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(events),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("project_id"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("event_id"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("timestamp"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("project_id"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("event_id"), KeyType: types.KeyTypeRange},
		},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{
			{
				IndexName: aws.String("TimestampIndex"),
				KeySchema: []types.KeySchemaElement{
					{AttributeName: aws.String("project_id"), KeyType: types.KeyTypeHash},
					{AttributeName: aws.String("timestamp"), KeyType: types.KeyTypeRange},
				},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			},
		},
	})
	if err != nil {
		t.Fatalf("create events table: %v", err)
	}
}

func waitForTables(t *testing.T, c *dynamodb.Client, tables ...string) {
	t.Helper()
	ctx := context.Background()
	for _, name := range tables {
		for i := 0; i < 30; i++ {
			out, err := c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(name)})
			if err == nil && out.Table.TableStatus == types.TableStatusActive {
				break
			}
			time.Sleep(500 * time.Millisecond)
			if i == 29 {
				t.Fatalf("table %s not active", name)
			}
		}
	}
}

func putProject(t *testing.T, c *dynamodb.Client, table, projectID, publicKey string) {
	t.Helper()
	item, err := attributevalue.MarshalMap(map[string]string{
		"project_id": projectID,
		"public_key": publicKey,
		"status":     "active",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_, err = c.PutItem(context.Background(), &dynamodb.PutItemInput{
		TableName: aws.String(table),
		Item:      item,
	})
	if err != nil {
		t.Fatalf("put project: %v", err)
	}
}

func waitForBucket(t *testing.T, c *s3.Client, name string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		_, err := c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(name)})
		if err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("bucket %s not ready", name)
}

func integrationDeps(t *testing.T, ddb *dynamodb.Client, s3c *s3.Client, bucket, projects, events string) *deps {
	t.Helper()
	return &deps{
		keys:  &dynamoKeyStore{db: ddb, table: projects},
		arch:  &s3ArchiveStore{client: s3c, bucket: bucket},
		index: &dynamoIndexStore{db: ddb, table: events},
		// Disable in-process limiting for integration; WAF handles it in prod.
		limiter:      newRateLimiter(100000, 100000),
		now:          time.Now,
		maxBodyBytes: 1 << 20,
		eventTTLDays: 90,
	}
}

func TestIntegrationIngestEnvelopeAndStore(t *testing.T) {
	ddb, s3c := newAWS(t)

	bucket := fmt.Sprintf("ingest-it-bucket-%d", time.Now().UnixNano())
	projects := fmt.Sprintf("ingest-it-projects-%d", time.Now().UnixNano())
	eventsTable := fmt.Sprintf("ingest-it-events-%d", time.Now().UnixNano())

	createBucket(t, s3c, bucket)
	createTables(t, ddb, projects, eventsTable)
	waitForBucket(t, s3c, bucket)
	waitForTables(t, ddb, projects, eventsTable)

	const (
		projectID = "it-proj"
		publicKey = "0123456789abcdef0123456789abcdef"
	)
	putProject(t, ddb, projects, projectID, publicKey)

	d := integrationDeps(t, ddb, s3c, bucket, projects, eventsTable)
	ctx := context.Background()

	envelope := envelopeBody(t,
		`{"event_id":"e1"}`,
		`{"type":"event"}`,
		`{"event_id":"e1","level":"error","platform":"javascript","message":"it boom"}`,
		`{"type":"session"}`,
		`{"sid":"s1","init":true}`,
	)
	resp, err := handleRequest(ctx, events.APIGatewayProxyRequest{
		Resource:       "/api/{project_id}/envelope",
		Path:           "/api/" + projectID + "/envelope",
		PathParameters: map[string]string{"project_id": projectID},
		Headers:        map[string]string{"X-Sentry-Auth": "Sentry sentry_version=7, sentry_key=" + publicKey},
		Body:           envelope,
	}, d)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Body, `"e1"`) {
		t.Fatalf("envelope status=%d body=%s", resp.StatusCode, resp.Body)
	}

	// Store route.
	resp, err = handleRequest(ctx, events.APIGatewayProxyRequest{
		Resource:              "/api/{project_id}/store",
		Path:                  "/api/" + projectID + "/store",
		PathParameters:        map[string]string{"project_id": projectID},
		QueryStringParameters: map[string]string{"sentry_key": publicKey},
		Body:                  `{"event_id":"a1b2c3d4e5f60718","message":"store it"}`,
	}, d)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("store status=%d body=%s", resp.StatusCode, resp.Body)
	}

	// S3 objects actually exist (real SDK call shapes).
	out, err := s3c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatalf("list objects: %v", err)
	}
	if len(out.Contents) < 4 { // envelope + 2 items + store item
		t.Fatalf("want >=4 s3 objects, got %d", len(out.Contents))
	}

	// DynamoDB rows actually exist.
	queryOut, err := ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(eventsTable),
		KeyConditionExpression: aws.String("project_id = :pid"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pid": &types.AttributeValueMemberS{Value: projectID},
		},
	})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if len(queryOut.Items) < 3 { // event + session + store event
		t.Fatalf("want >=3 ddb rows, got %d", len(queryOut.Items))
	}
}

func TestIntegrationIngestAuthAndErrors(t *testing.T) {
	ddb, s3c := newAWS(t)

	bucket := fmt.Sprintf("ingest-auth-bucket-%d", time.Now().UnixNano())
	projects := fmt.Sprintf("ingest-auth-projects-%d", time.Now().UnixNano())
	eventsTable := fmt.Sprintf("ingest-auth-events-%d", time.Now().UnixNano())

	createBucket(t, s3c, bucket)
	createTables(t, ddb, projects, eventsTable)
	waitForBucket(t, s3c, bucket)
	waitForTables(t, ddb, projects, eventsTable)

	putProject(t, ddb, projects, "auth-proj", "abcdef0123456789abcdef0123456789")
	d := integrationDeps(t, ddb, s3c, bucket, projects, eventsTable)
	ctx := context.Background()

	base := events.APIGatewayProxyRequest{
		Resource:       "/api/{project_id}/envelope",
		Path:           "/api/auth-proj/envelope",
		PathParameters: map[string]string{"project_id": "auth-proj"},
		Body:           envelopeBody(t, `{"event_id":"x"}`),
	}

	// No auth -> 403.
	req := base
	resp, _ := handleRequest(ctx, req, d)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no auth status=%d (want 403)", resp.StatusCode)
	}

	// Unknown key -> 401.
	req = base
	req.Headers = map[string]string{"X-Sentry-Auth": "Sentry sentry_version=7, sentry_key=deadbeef"}
	resp, _ = handleRequest(ctx, req, d)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown key status=%d (want 401)", resp.StatusCode)
	}

	// Malformed envelope -> 400.
	req = base
	req.Headers = map[string]string{"X-Sentry-Auth": "Sentry sentry_version=7, sentry_key=abcdef0123456789abcdef0123456789"}
	req.Body = "{}\n{\"type\":\"attachment\",\"length\":10}\nshort"
	resp, _ = handleRequest(ctx, req, d)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed status=%d (want 400)", resp.StatusCode)
	}
}
