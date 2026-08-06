//go:build integration

// Integration tests for the query store against LocalStack.
// Run with:
//
//	docker compose up -d localstack
//	cd lambda/query && go test -tags=integration ./...
package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const localstackEndpoint = "http://localhost:4566"

// testRow mirrors the ingest eventRow schema for seeding LocalStack.
type testRow struct {
	ProjectID string `dynamodbav:"project_id"`
	EventID   string `dynamodbav:"event_id"`
	Timestamp string `dynamodbav:"timestamp,omitempty"`
	Level     string `dynamodbav:"level,omitempty"`
	Platform  string `dynamodbav:"platform,omitempty"`
	ItemType  string `dynamodbav:"item_type"`
	Status    string `dynamodbav:"status"`
}

func newDynamo(t *testing.T) *dynamodb.Client {
	t.Helper()
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		o.BaseEndpoint = aws.String(localstackEndpoint)
	})
}

func TestIntegrationQueryEvents(t *testing.T) {
	ddb := newDynamo(t)
	ctx := context.Background()

	table := fmt.Sprintf("query-it-events-%d", time.Now().UnixNano())
	_, err := ddb.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(table),
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
		t.Fatalf("create table: %v", err)
	}
	for i := 0; i < 30; i++ {
		out, err := ddb.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(table)})
		if err == nil && out.Table.TableStatus == types.TableStatusActive {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	rows := []testRow{
		{ProjectID: "p1", EventID: "e1", Timestamp: "2026-08-01T00:00:00Z", Level: "error", Platform: "js", ItemType: "event", Status: "received"},
		{ProjectID: "p1", EventID: "e2", Timestamp: "2026-08-02T00:00:00Z", Level: "info", Platform: "js", ItemType: "event", Status: "received"},
		{ProjectID: "p1", EventID: "e3", Timestamp: "2026-08-03T00:00:00Z", ItemType: "session", Status: "received"},
	}
	for _, r := range rows {
		item, err := attributevalue.MarshalMap(r)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(table), Item: item}); err != nil {
			t.Fatal(err)
		}
	}

	store := &dynamoQueryStore{db: ddb, table: table}

	// Default (empty itemType -> event) must exclude the session row.
	got, next, err := store.QueryEvents(ctx, "p1", "", "", "", "", 10, "")
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("default query want 2 event rows, got %d: %+v", len(got), got)
	}
	if next != "" {
		t.Errorf("next_cursor = %q, want empty", next)
	}

	// type=all includes every row.
	got, _, err = store.QueryEvents(ctx, "p1", "", "", "", "all", 10, "")
	if err != nil {
		t.Fatalf("QueryEvents all: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("type=all want 3 rows, got %d", len(got))
	}

	// level filter narrows.
	got, _, err = store.QueryEvents(ctx, "p1", "", "", "info", "", 10, "")
	if err != nil {
		t.Fatalf("QueryEvents level: %v", err)
	}
	if len(got) != 1 || got[0].EventID != "e2" {
		t.Fatalf("level=info want e2 only, got %+v", got)
	}

	// Timestamp range uses the GSI.
	got, _, err = store.QueryEvents(ctx, "p1", "2026-08-01T00:00:00Z", "2026-08-02T23:59:59Z", "", "", 10, "")
	if err != nil {
		t.Fatalf("QueryEvents range: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("range query want 2 rows, got %d: %+v", len(got), got)
	}
}

func TestIntegrationQueryCursorPagination(t *testing.T) {
	ddb := newDynamo(t)
	ctx := context.Background()

	table := fmt.Sprintf("query-it-page-%d", time.Now().UnixNano())
	_, err := ddb.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(table),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("project_id"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("event_id"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("project_id"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("event_id"), KeyType: types.KeyTypeRange},
		},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	for i := 0; i < 30; i++ {
		out, err := ddb.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(table)})
		if err == nil && out.Table.TableStatus == types.TableStatusActive {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	for i := 0; i < 7; i++ {
		row := testRow{ProjectID: "p2", EventID: fmt.Sprintf("e%02d", i), ItemType: "event", Status: "received"}
		item, _ := attributevalue.MarshalMap(row)
		if _, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(table), Item: item}); err != nil {
			t.Fatal(err)
		}
	}

	store := &dynamoQueryStore{db: ddb, table: table}

	page1, next, err := store.QueryEvents(ctx, "p2", "", "", "", "", 3, "")
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 3 || next == "" {
		t.Fatalf("page1: %d rows, next=%q (want 3 + cursor)", len(page1), next)
	}

	page2, next2, err := store.QueryEvents(ctx, "p2", "", "", "", "", 3, next)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 3 || next2 == "" {
		t.Fatalf("page2: %d rows, next=%q (want 3 + cursor)", len(page2), next2)
	}

	page3, next3, err := store.QueryEvents(ctx, "p2", "", "", "", "", 3, next2)
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if len(page3) != 1 || next3 != "" {
		t.Fatalf("page3: %d rows, next=%q (want 1 + empty cursor)", len(page3), next3)
	}
}
