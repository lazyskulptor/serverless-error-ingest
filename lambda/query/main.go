// Package main implements the read-only event metadata query Lambda handler.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const defaultLimit = 50
const maxLimit = 200

// queryStore abstracts DynamoDB access so the handler is unit-testable.
type queryStore interface {
	// QueryEvents returns metadata rows for a project. When from/to are
	// non-empty the TimestampIndex range is used. itemType filters on
	// item_type; the empty string means the default ("event"), and "all"
	// disables the filter. Cursor is an opaque, base64-encoded DynamoDB
	// ExclusiveStartKey. Returns rows, next cursor (empty when exhausted),
	// and error.
	QueryEvents(ctx context.Context, projectID string, from, to, level, itemType string, limit int32, cursor string) ([]eventMeta, string, error)
}

// eventMeta is the public metadata summary for one event row.
// dynamodbav tags are required for attributevalue.UnmarshalMap to map
// underscore attribute names correctly.
type eventMeta struct {
	ProjectID string `json:"project_id" dynamodbav:"project_id"`
	EventID   string `json:"event_id" dynamodbav:"event_id"`
	Timestamp string `json:"timestamp" dynamodbav:"timestamp"`
	Level     string `json:"level,omitempty" dynamodbav:"level,omitempty"`
	Platform  string `json:"platform,omitempty" dynamodbav:"platform,omitempty"`
	ItemType  string `json:"item_type" dynamodbav:"item_type"`
	Count     int    `json:"count" dynamodbav:"count"`
	Status    string `json:"status" dynamodbav:"status"`
	S3Path    string `json:"s3_path" dynamodbav:"s3_path"`
}

// deps bundles the query handler's dependencies (injected for tests).
type deps struct {
	store queryStore
	keys  keyLookup
}

// keyLookup validates a DSN public key against the projects registry.
type keyLookup interface {
	ValidateKey(ctx context.Context, projectID, publicKey string) (bool, error)
}

func handleRequest(ctx context.Context, req events.APIGatewayProxyRequest, d *deps) (events.APIGatewayProxyResponse, error) {
	projectID := req.PathParameters["project_id"]
	if projectID == "" {
		return respond(http.StatusBadRequest, "missing project_id", nil)
	}

	// Same key validation as ingestion: header form or query-string form.
	key := sentryKey(req)
	if key == "" {
		return respond(http.StatusForbidden, "missing authentication", nil)
	}
	valid, err := d.keys.ValidateKey(ctx, projectID, key)
	if err != nil {
		slog.ErrorContext(ctx, "key validation failed", "project_id", projectID, "error", err)
		return respond(http.StatusInternalServerError, "key validation failed", nil)
	}
	if !valid {
		return respond(http.StatusUnauthorized, "invalid sentry public key", nil)
	}

	from := req.QueryStringParameters["from"]
	to := req.QueryStringParameters["to"]
	level := req.QueryStringParameters["level"]
	itemType := req.QueryStringParameters["type"]
	cursor := req.QueryStringParameters["cursor"]

	limit := defaultLimit
	if v := req.QueryStringParameters["limit"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return respond(http.StatusBadRequest, "invalid limit", nil)
		}
		if n > maxLimit {
			n = maxLimit
		}
		limit = n
	}

	rows, nextCursor, err := d.store.QueryEvents(ctx, projectID, from, to, level, itemType, int32(limit), cursor)
	if err != nil {
		slog.ErrorContext(ctx, "query failed", "project_id", projectID, "error", err)
		return respond(http.StatusInternalServerError, "query failed", nil)
	}

	body, _ := json.Marshal(map[string]any{
		"events":      rows,
		"next_cursor": nextCursor,
	})
	headers := map[string]string{
		"Content-Type":                "application/json",
		"Access-Control-Allow-Origin": "*",
	}
	return events.APIGatewayProxyResponse{StatusCode: http.StatusOK, Headers: headers, Body: string(body)}, nil
}

func respond(status int, message string, headers map[string]string) (events.APIGatewayProxyResponse, error) {
	if headers == nil {
		headers = map[string]string{}
	}
	headers["Access-Control-Allow-Origin"] = "*"
	body, _ := json.Marshal(map[string]any{"detail": message})
	return events.APIGatewayProxyResponse{StatusCode: status, Headers: headers, Body: string(body)}, nil
}

func sentryKey(req events.APIGatewayProxyRequest) string {
	if v := req.Headers["X-Sentry-Auth"]; v != "" {
		if k := sentryKeyFromAuthHeader(v); k != "" {
			return k
		}
	}
	if v := req.Headers["x-sentry-auth"]; v != "" {
		if k := sentryKeyFromAuthHeader(v); k != "" {
			return k
		}
	}
	if req.QueryStringParameters != nil {
		if k := req.QueryStringParameters["sentry_key"]; k != "" {
			return k
		}
	}
	return ""
}

func sentryKeyFromAuthHeader(value string) string {
	rest := value
	if idx := strings.IndexByte(rest, ' '); idx >= 0 {
		rest = rest[idx+1:]
	}
	for _, part := range strings.Split(rest, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && strings.TrimSpace(kv[0]) == "sentry_key" {
			return strings.TrimSpace(kv[1])
		}
	}
	return ""
}

// --- AWS-backed implementations ---

type dynamoQueryStore struct {
	db    *dynamodb.Client
	table string
}

func (s *dynamoQueryStore) QueryEvents(ctx context.Context, projectID, from, to, level, itemType string, limit int32, cursor string) ([]eventMeta, string, error) {
	keyCond := "project_id = :pid"
	exprVals := map[string]types.AttributeValue{
		":pid": &types.AttributeValueMemberS{Value: projectID},
	}
	attrNames := map[string]string{}

	useIndex := from != "" || to != ""
	if useIndex {
		// "timestamp" is a DynamoDB reserved keyword — always alias it.
		attrNames["#ts"] = "timestamp"
		if from != "" && to != "" {
			keyCond += " AND #ts BETWEEN :from AND :to"
			exprVals[":from"] = &types.AttributeValueMemberS{Value: from}
			exprVals[":to"] = &types.AttributeValueMemberS{Value: to}
		} else {
			// A single bound is not expressible as BETWEEN; fall back to >= or <=.
			// The GSI requires the full key condition on project_id + timestamp,
			// so a single bound still uses the index with one side open.
			if from != "" {
				keyCond += " AND #ts >= :from"
				exprVals[":from"] = &types.AttributeValueMemberS{Value: from}
			}
			if to != "" {
				keyCond += " AND #ts <= :to"
				exprVals[":to"] = &types.AttributeValueMemberS{Value: to}
			}
		}
	}

	input := &dynamodb.QueryInput{
		TableName:                 aws.String(s.table),
		KeyConditionExpression:    aws.String(keyCond),
		ExpressionAttributeValues: exprVals,
		Limit:                     aws.Int32(limit),
	}
	if useIndex {
		input.IndexName = aws.String("TimestampIndex")
	}

	// Default to event rows unless the caller explicitly widens with type=all
	// or opts into another item type. level narrows the item_type filter.
	if itemType == "" {
		itemType = "event"
	}
	filters := []string{}
	if itemType != "all" {
		filters = append(filters, "#it = :it")
		attrNames["#it"] = "item_type"
		exprVals[":it"] = &types.AttributeValueMemberS{Value: itemType}
	}
	if level != "" {
		filters = append(filters, "#lvl = :lvl")
		attrNames["#lvl"] = "level" // "level" is a DynamoDB reserved keyword
		exprVals[":lvl"] = &types.AttributeValueMemberS{Value: level}
	}
	if len(filters) > 0 {
		input.FilterExpression = aws.String(strings.Join(filters, " AND "))
		input.ExpressionAttributeNames = attrNames
	}
	if cursor != "" {
		lek, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("invalid cursor: %w", err)
		}
		input.ExclusiveStartKey = lek
	}

	out, err := s.db.Query(ctx, input)
	if err != nil {
		return nil, "", fmt.Errorf("dynamodb query: %w", err)
	}

	rows := make([]eventMeta, 0, len(out.Items))
	for _, item := range out.Items {
		var row eventMeta
		if err := attributevalue.UnmarshalMap(item, &row); err != nil {
			return nil, "", fmt.Errorf("unmarshal event row: %w", err)
		}
		rows = append(rows, row)
	}

	next := ""
	if len(out.LastEvaluatedKey) > 0 {
		next, err = encodeCursor(out.LastEvaluatedKey)
		if err != nil {
			return nil, "", fmt.Errorf("encode cursor: %w", err)
		}
	}
	return rows, next, nil
}

// encodeCursor/base64-encodes a DynamoDB LastEvaluatedKey for pass-through as
// an opaque next_cursor.
func encodeCursor(key map[string]types.AttributeValue) (string, error) {
	// Convert to a JSON-safe intermediate so the opaque value is stable and
	// reversible.
	type kv struct {
		K string `json:"k"`
		V string `json:"v"`
	}
	parts := make([]kv, 0, len(key))
	for k, v := range key {
		s, ok := v.(*types.AttributeValueMemberS)
		if !ok {
			return "", fmt.Errorf("unsupported key attribute type for %q", k)
		}
		parts = append(parts, kv{K: k, V: s.Value})
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeCursor(cursor string) (map[string]types.AttributeValue, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, errors.New("cursor is not valid base64")
	}
	var parts []struct {
		K string `json:"k"`
		V string `json:"v"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, errors.New("cursor is malformed")
	}
	key := make(map[string]types.AttributeValue, len(parts))
	for _, p := range parts {
		key[p.K] = &types.AttributeValueMemberS{Value: p.V}
	}
	return key, nil
}

type dynamoKeyLookup struct {
	db    *dynamodb.Client
	table string
}

func (s *dynamoKeyLookup) ValidateKey(ctx context.Context, projectID, publicKey string) (bool, error) {
	if projectID == "" || publicKey == "" {
		return false, nil
	}
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"project_id": &types.AttributeValueMemberS{Value: projectID},
		},
	})
	if err != nil {
		return false, fmt.Errorf("get project: %w", err)
	}
	if len(out.Item) == 0 {
		return false, nil
	}
	var row struct {
		PublicKey string `dynamodbav:"public_key"`
		Status    string `dynamodbav:"status"`
	}
	if err := attributevalue.UnmarshalMap(out.Item, &row); err != nil {
		return false, fmt.Errorf("unmarshal project: %w", err)
	}
	return row.PublicKey == publicKey && row.Status == "active", nil
}

func newDefaultDeps(ctx context.Context) (*deps, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	eventsTable := os.Getenv("EVENTS_TABLE")
	projectsTable := os.Getenv("PROJECTS_TABLE")
	if eventsTable == "" || projectsTable == "" {
		return nil, errors.New("EVENTS_TABLE and PROJECTS_TABLE must be set")
	}
	return &deps{
		store: &dynamoQueryStore{db: dynamodb.NewFromConfig(cfg), table: eventsTable},
		keys:  &dynamoKeyLookup{db: dynamodb.NewFromConfig(cfg), table: projectsTable},
	}, nil
}
