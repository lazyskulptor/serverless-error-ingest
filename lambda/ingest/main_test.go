package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// fakeStores implement the keyStore/archiveStore/indexStore interfaces in
// memory so the handler can be exercised without AWS.
type fakeKeyStore struct{ valid map[string]string }

func (f *fakeKeyStore) ValidateKey(_ context.Context, projectID, publicKey string) (bool, error) {
	if f.valid == nil {
		return false, nil
	}
	key, ok := f.valid[projectID]
	return ok && key == publicKey, nil
}

type fakeArchiveStore struct{ objects map[string][]byte }

func (f *fakeArchiveStore) PutObject(_ context.Context, key string, data []byte) error {
	if f.objects == nil {
		f.objects = make(map[string][]byte)
	}
	f.objects[key] = data
	return nil
}

type fakeIndexStore struct{ rows []eventRow }

func (f *fakeIndexStore) PutEvent(_ context.Context, row eventRow) error {
	f.rows = append(f.rows, row)
	return nil
}

func newTestDeps(validKeys map[string]string) (*deps, *fakeArchiveStore, *fakeIndexStore) {
	arch := &fakeArchiveStore{}
	idx := &fakeIndexStore{}
	return &deps{
		keys:    &fakeKeyStore{valid: validKeys},
		arch:    arch,
		index:   idx,
		limiter: newRateLimiter(1000, 1000), // effectively unlimited
		now:     func() time.Time { return time.Date(2026, 8, 6, 1, 0, 0, 0, time.UTC) },
	}, arch, idx
}

func envelopeBody(t *testing.T, header string, items ...string) string {
	t.Helper()
	parts := []string{header}
	parts = append(parts, items...)
	return strings.Join(parts, "\n") + "\n"
}

func TestHandleEnvelopeSuccess(t *testing.T) {
	d, arch, idx := newTestDeps(map[string]string{"proj1": "key1"})
	body := envelopeBody(t,
		`{"event_id":"e1"}`,
		`{"type":"event"}`,
		`{"event_id":"e1","level":"error","platform":"javascript","message":"boom"}`,
		`{"type":"session"}`,
		`{"sid":"s1","init":true}`,
	)

	resp, err := handleRequest(context.Background(), events.APIGatewayProxyRequest{
		Resource:       "/api/{project_id}/envelope",
		Path:           "/api/proj1/envelope",
		PathParameters: map[string]string{"project_id": "proj1"},
		Headers:        map[string]string{"X-Sentry-Auth": "Sentry sentry_version=7, sentry_key=key1"},
		Body:           body,
	}, d)
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	if ct := resp.Headers["Content-Type"]; ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	if resp.Headers["Access-Control-Allow-Origin"] != "*" {
		t.Error("missing CORS header on response")
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &out); err != nil || out.ID != "e1" {
		t.Errorf("response body = %q", resp.Body)
	}

	// Envelope archived + one object per item + one index row per item.
	if len(arch.objects) != 3 {
		t.Errorf("want 3 s3 objects, got %d", len(arch.objects))
	}
	if _, ok := arch.objects["projects/proj1/2026/08/06/e1.envelope"]; !ok {
		t.Errorf("missing envelope object, have %v", keys(arch.objects))
	}
	if len(idx.rows) != 2 {
		t.Fatalf("want 2 index rows, got %d", len(idx.rows))
	}
	if idx.rows[0].EventID != "e1" {
		t.Errorf("row0 event_id = %q", idx.rows[0].EventID)
	}
	if idx.rows[0].ItemType != "event" || idx.rows[0].Level != "error" || idx.rows[0].Platform != "javascript" {
		t.Errorf("row0 = %+v", idx.rows[0])
	}
	if idx.rows[1].ItemType != "session" {
		t.Errorf("row1 = %+v", idx.rows[1])
	}
	// First item owns the bare event_id; second item is suffixed.
	if idx.rows[1].EventID == "e1" {
		t.Errorf("row1 event_id should be suffixed, got %q", idx.rows[1].EventID)
	}
}

func TestHandleStoreSuccess(t *testing.T) {
	d, _, idx := newTestDeps(map[string]string{"proj1": "key1"})
	resp, err := handleRequest(context.Background(), events.APIGatewayProxyRequest{
		Resource:              "/api/{project_id}/store",
		Path:                  "/api/proj1/store",
		PathParameters:        map[string]string{"project_id": "proj1"},
		QueryStringParameters: map[string]string{"sentry_version": "7", "sentry_key": "key1"},
		Body:                  `{"event_id":"a1b2c3d4e5f60718","message":"store event"}`,
	}, d)
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	if !strings.Contains(resp.Body, `"a1b2c3d4e5f60718"`) {
		t.Errorf("body = %q", resp.Body)
	}
	if len(idx.rows) != 1 || idx.rows[0].ItemType != "event" {
		t.Errorf("rows = %+v", idx.rows)
	}
}

func TestHandleNoAuthForbidden(t *testing.T) {
	d, _, _ := newTestDeps(map[string]string{"proj1": "key1"})
	resp, err := handleRequest(context.Background(), events.APIGatewayProxyRequest{
		Resource:       "/api/{project_id}/envelope",
		Path:           "/api/proj1/envelope",
		PathParameters: map[string]string{"project_id": "proj1"},
		Body:           envelopeBody(t, `{}`),
	}, d)
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d (want 403), body = %s", resp.StatusCode, resp.Body)
	}
}

func TestHandleUnknownKeyUnauthorized(t *testing.T) {
	d, _, _ := newTestDeps(map[string]string{"proj1": "key1"})
	resp, err := handleRequest(context.Background(), events.APIGatewayProxyRequest{
		Resource:       "/api/{project_id}/envelope",
		Path:           "/api/proj1/envelope",
		PathParameters: map[string]string{"project_id": "proj1"},
		Headers:        map[string]string{"X-Sentry-Auth": "Sentry sentry_version=7, sentry_key=wrong"},
		Body:           envelopeBody(t, `{}`),
	}, d)
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d (want 401), body = %s", resp.StatusCode, resp.Body)
	}
}

func TestHandleMalformedEnvelopeBadRequest(t *testing.T) {
	d, _, _ := newTestDeps(map[string]string{"proj1": "key1"})
	resp, err := handleRequest(context.Background(), events.APIGatewayProxyRequest{
		Resource:       "/api/{project_id}/envelope",
		Path:           "/api/proj1/envelope",
		PathParameters: map[string]string{"project_id": "proj1"},
		Headers:        map[string]string{"X-Sentry-Auth": "Sentry sentry_version=7, sentry_key=key1"},
		Body:           "{}\n{\"type\":\"attachment\",\"length\":10}\nshort",
	}, d)
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d (want 400), body = %s", resp.StatusCode, resp.Body)
	}
	var out struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &out); err != nil || out.Detail == "" {
		t.Errorf("detail missing in body %q", resp.Body)
	}
	if resp.Headers["X-Sentry-Error"] == "" {
		t.Error("missing X-Sentry-Error header on 400")
	}
}

func TestHandleRateLimited(t *testing.T) {
	d, _, _ := newTestDeps(map[string]string{"proj1": "key1"})
	d.limiter = newRateLimiter(0.5, 0) // zero burst: every request denied

	resp, err := handleRequest(context.Background(), events.APIGatewayProxyRequest{
		Resource:       "/api/{project_id}/envelope",
		Path:           "/api/proj1/envelope",
		PathParameters: map[string]string{"project_id": "proj1"},
		Headers:        map[string]string{"X-Sentry-Auth": "Sentry sentry_version=7, sentry_key=key1"},
		Body:           envelopeBody(t, `{}`),
	}, d)
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d (want 429)", resp.StatusCode)
	}
	if resp.Headers["Retry-After"] == "" {
		t.Error("missing Retry-After header on 429")
	}
}

func TestHandleBase64EncodedBody(t *testing.T) {
	d, _, idx := newTestDeps(map[string]string{"proj1": "key1"})
	raw := envelopeBody(t,
		`{"event_id":"b64"}`,
		`{"type":"event"}`,
		`{"event_id":"b64","message":"hi"}`,
	)
	resp, err := handleRequest(context.Background(), events.APIGatewayProxyRequest{
		Resource:        "/api/{project_id}/envelope",
		Path:            "/api/proj1/envelope",
		PathParameters:  map[string]string{"project_id": "proj1"},
		Headers:         map[string]string{"X-Sentry-Auth": "Sentry sentry_version=7, sentry_key=key1"},
		Body:            base64.StdEncoding.EncodeToString([]byte(raw)),
		IsBase64Encoded: true,
	}, d)
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	if !strings.Contains(resp.Body, `"b64"`) {
		t.Errorf("body = %q", resp.Body)
	}
	if len(idx.rows) != 1 {
		t.Errorf("rows = %d", len(idx.rows))
	}
}

func TestHandleEnvelopeDSNAuth(t *testing.T) {
	d, _, _ := newTestDeps(map[string]string{"proj1": "envkey"})
	body := envelopeBody(t,
		`{"dsn":"https://envkey@example.com/proj1"}`,
		`{"type":"event"}`,
		`{"message":"x"}`,
	)
	resp, err := handleRequest(context.Background(), events.APIGatewayProxyRequest{
		Resource:       "/api/{project_id}/envelope",
		Path:           "/api/proj1/envelope",
		PathParameters: map[string]string{"project_id": "proj1"},
		Body:           body,
	}, d)
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
