package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type fakeQueryStore struct {
	rows     []eventMeta
	next     string
	lastArgs []string // projectID, from, to, level, itemType, limit, cursor
}

func (f *fakeQueryStore) QueryEvents(_ context.Context, projectID, from, to, level, itemType string, limit int32, cursor string) ([]eventMeta, string, error) {
	f.lastArgs = []string{projectID, from, to, level, itemType, string(rune(limit)), cursor}
	return f.rows, f.next, nil
}

type fakeKeys struct{ valid map[string]string }

func (f *fakeKeys) ValidateKey(_ context.Context, projectID, publicKey string) (bool, error) {
	if f.valid == nil {
		return false, nil
	}
	key, ok := f.valid[projectID]
	return ok && key == publicKey, nil
}

func newTestDeps(valid map[string]string, rows []eventMeta, next string) (*deps, *fakeQueryStore) {
	s := &fakeQueryStore{rows: rows, next: next}
	return &deps{store: s, keys: &fakeKeys{valid: valid}}, s
}

func TestQuerySuccess(t *testing.T) {
	rows := []eventMeta{
		{ProjectID: "proj1", EventID: "e1", Timestamp: "2026-08-06T01:00:00Z", Level: "error", Platform: "javascript", ItemType: "event", Count: 1, Status: "received"},
	}
	d, store := newTestDeps(map[string]string{"proj1": "key1"}, rows, "next-token")

	resp, err := handleRequest(context.Background(), events.APIGatewayProxyRequest{
		Resource:              "/api/projects/{project_id}/events",
		Path:                  "/api/projects/proj1/events",
		PathParameters:        map[string]string{"project_id": "proj1"},
		QueryStringParameters: map[string]string{"sentry_key": "key1", "limit": "10"},
	}, d)
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	var out struct {
		Events     []eventMeta `json:"events"`
		NextCursor string      `json:"next_cursor"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Events) != 1 || out.Events[0].EventID != "e1" {
		t.Errorf("events = %+v", out.Events)
	}
	if out.NextCursor != "next-token" {
		t.Errorf("next_cursor = %q", out.NextCursor)
	}
	if store.lastArgs[0] != "proj1" {
		t.Errorf("project arg = %q", store.lastArgs[0])
	}
}

func TestQueryNoAuthForbidden(t *testing.T) {
	d, _ := newTestDeps(map[string]string{"proj1": "key1"}, nil, "")
	resp, err := handleRequest(context.Background(), events.APIGatewayProxyRequest{
		Resource:       "/api/projects/{project_id}/events",
		Path:           "/api/projects/proj1/events",
		PathParameters: map[string]string{"project_id": "proj1"},
	}, d)
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d (want 403), body = %s", resp.StatusCode, resp.Body)
	}
}

func TestQueryUnknownKeyUnauthorized(t *testing.T) {
	d, _ := newTestDeps(map[string]string{"proj1": "key1"}, nil, "")
	resp, err := handleRequest(context.Background(), events.APIGatewayProxyRequest{
		Resource:              "/api/projects/{project_id}/events",
		Path:                  "/api/projects/proj1/events",
		PathParameters:        map[string]string{"project_id": "proj1"},
		QueryStringParameters: map[string]string{"sentry_key": "wrong"},
	}, d)
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d (want 401), body = %s", resp.StatusCode, resp.Body)
	}
}

func TestQueryHeaderAuth(t *testing.T) {
	d, _ := newTestDeps(map[string]string{"proj1": "key1"}, nil, "")
	resp, err := handleRequest(context.Background(), events.APIGatewayProxyRequest{
		Resource:       "/api/projects/{project_id}/events",
		Path:           "/api/projects/proj1/events",
		PathParameters: map[string]string{"project_id": "proj1"},
		Headers:        map[string]string{"X-Sentry-Auth": "Sentry sentry_version=7, sentry_key=key1"},
	}, d)
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
}

func TestQueryPassesFiltersAndCursor(t *testing.T) {
	d, store := newTestDeps(map[string]string{"proj1": "key1"}, nil, "")
	_, err := handleRequest(context.Background(), events.APIGatewayProxyRequest{
		Resource:       "/api/projects/{project_id}/events",
		Path:           "/api/projects/proj1/events",
		PathParameters: map[string]string{"project_id": "proj1"},
		QueryStringParameters: map[string]string{
			"sentry_key": "key1",
			"from":       "2026-08-01T00:00:00Z",
			"to":         "2026-08-31T00:00:00Z",
			"level":      "error",
			"cursor":     "abc",
			"limit":      "200",
		},
	}, d)
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if store.lastArgs[1] != "2026-08-01T00:00:00Z" || store.lastArgs[2] != "2026-08-31T00:00:00Z" || store.lastArgs[3] != "error" {
		t.Errorf("filters not passed: %v", store.lastArgs)
	}
	if store.lastArgs[6] != "abc" {
		t.Errorf("cursor = %q", store.lastArgs[6])
	}
}

func TestQueryDefaultItemTypePassedEmpty(t *testing.T) {
	// Without a `type` param the handler forwards "" so the store defaults to
	// event rows.
	d, store := newTestDeps(map[string]string{"proj1": "key1"}, nil, "")
	_, err := handleRequest(context.Background(), events.APIGatewayProxyRequest{
		Resource:              "/api/projects/{project_id}/events",
		Path:                  "/api/projects/proj1/events",
		PathParameters:        map[string]string{"project_id": "proj1"},
		QueryStringParameters: map[string]string{"sentry_key": "key1"},
	}, d)
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if store.lastArgs[4] != "" {
		t.Errorf("itemType = %q, want empty (store applies default)", store.lastArgs[4])
	}
}

func TestQueryTypeParamForwarded(t *testing.T) {
	d, store := newTestDeps(map[string]string{"proj1": "key1"}, nil, "")
	_, err := handleRequest(context.Background(), events.APIGatewayProxyRequest{
		Resource:              "/api/projects/{project_id}/events",
		Path:                  "/api/projects/proj1/events",
		PathParameters:        map[string]string{"project_id": "proj1"},
		QueryStringParameters: map[string]string{"sentry_key": "key1", "type": "session"},
	}, d)
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if store.lastArgs[4] != "session" {
		t.Errorf("itemType = %q, want session", store.lastArgs[4])
	}
}

func TestQueryInvalidLimit(t *testing.T) {
	d, _ := newTestDeps(map[string]string{"proj1": "key1"}, nil, "")
	resp, err := handleRequest(context.Background(), events.APIGatewayProxyRequest{
		Resource:              "/api/projects/{project_id}/events",
		Path:                  "/api/projects/proj1/events",
		PathParameters:        map[string]string{"project_id": "proj1"},
		QueryStringParameters: map[string]string{"sentry_key": "key1", "limit": "abc"},
	}, d)
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d (want 400)", resp.StatusCode)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	// Verify the opaque cursor encode/decode is reversible for the events
	// table key schema (project_id + event_id).
	lek := map[string]types.AttributeValue{
		"project_id": &types.AttributeValueMemberS{Value: "proj1"},
		"event_id":   &types.AttributeValueMemberS{Value: "e1"},
	}
	cursor, err := encodeCursor(lek)
	if err != nil {
		t.Fatalf("encodeCursor: %v", err)
	}
	if cursor == "" {
		t.Fatal("empty cursor")
	}
	decoded, err := decodeCursor(cursor)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if len(decoded) != 2 || decoded["project_id"].(*types.AttributeValueMemberS).Value != "proj1" {
		t.Errorf("decoded = %+v", decoded)
	}
}

func TestDecodeCursorInvalid(t *testing.T) {
	if _, err := decodeCursor("!!!not-base64!!!"); err == nil {
		t.Fatal("expected error for invalid cursor")
	}
}
