package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

func TestValidEventID(t *testing.T) {
	valid := []string{"e1", "abc123", strings.Repeat("a", 64), "ABCDEF0123456789"}
	for _, s := range valid {
		if !validEventID(s) {
			t.Errorf("validEventID(%q) = false, want true", s)
		}
	}
	invalid := []string{
		"", "../../x", "a/b", "a#b", "a b", "a\x00b",
		"g", "zz", strings.Repeat("a", 65), "abc-123", "abc_123",
	}
	for _, s := range invalid {
		if validEventID(s) {
			t.Errorf("validEventID(%q) = true, want false", s)
		}
	}
}

func TestEventIDSanitizationStorageKeysStayInProject(t *testing.T) {
	// A crafted client event_id must never escape the project's namespace in
	// S3 keys or DynamoDB sort keys.
	evil := []string{"../../x", "a/b", "a#b", "a b", "\x00", strings.Repeat("a", 65), "..%2f..%2fx"}
	for _, eid := range evil {
		d, arch, idx := newTestDeps(map[string]string{"proj1": "key1"})
		// Persist directly with the malicious id (as the handler would after
		// fallback logic failed to catch it — defense in depth).
		raw := []byte(`{"message":"x"}`)
		err := persist(context.Background(), d, "proj1", eid, raw,
			[]Item{{Header: ItemHeader{Type: "event"}, Payload: raw}},
			time.Date(2026, 8, 6, 1, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatalf("persist(%q): %v", eid, err)
		}

		for key := range arch.objects {
			if !strings.HasPrefix(key, "projects/proj1/") {
				t.Errorf("event_id %q escaped project prefix via key %q", eid, key)
			}
		}
		for _, row := range idx.rows {
			if !strings.HasPrefix(row.EventID, "projects") && strings.ContainsAny(row.EventID, "/#\x00") {
				t.Errorf("event_id %q produced unsafe sort key %q", eid, row.EventID)
			}
		}
	}
}

func TestHandleRequestWithMaliciousEventID(t *testing.T) {
	d, arch, _ := newTestDeps(map[string]string{"proj1": "key1"})
	body := envelopeBody(t,
		`{"event_id":"evil"}`,
		`{"type":"event"}`,
		`{"event_id":"../../x","message":"boom"}`,
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
	for key := range arch.objects {
		if !strings.HasPrefix(key, "projects/proj1/") {
			t.Errorf("key escaped project prefix: %q", key)
		}
	}
}

func TestHandleOversizedPayloadBadRequest(t *testing.T) {
	d, _, _ := newTestDeps(map[string]string{"proj1": "key1"})
	d.maxBodyBytes = 10 // tiny cap for the test

	body := envelopeBody(t,
		`{"event_id":"e1"}`,
		`{"type":"event"}`,
		`{"message":"this payload is way bigger than ten bytes"}`,
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
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d (want 400)", resp.StatusCode)
	}
	if !strings.Contains(resp.Body, "maximum size") {
		t.Errorf("body = %q", resp.Body)
	}
}

func TestDeterministicEventIDOnRetry(t *testing.T) {
	// Two identical store requests without a client event_id must collapse
	// onto the exact same S3 objects and DynamoDB rows.
	run := func() (*fakeArchiveStore, *fakeIndexStore) {
		d, arch, idx := newTestDeps(map[string]string{"proj1": "key1"})
		resp, err := handleRequest(context.Background(), events.APIGatewayProxyRequest{
			Resource:              "/api/{project_id}/store",
			Path:                  "/api/proj1/store",
			PathParameters:        map[string]string{"project_id": "proj1"},
			QueryStringParameters: map[string]string{"sentry_key": "key1"},
			Body:                  `{"message":"retry me"}`,
		}, d)
		if err != nil {
			t.Fatalf("handleRequest: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
		}
		if !strings.Contains(resp.Body, `"id"`) {
			t.Errorf("body = %q", resp.Body)
		}
		return arch, idx
	}

	arch1, idx1 := run()
	arch2, idx2 := run()

	for k := range arch1.objects {
		if _, ok := arch2.objects[k]; !ok {
			t.Errorf("first run object %q missing in second run — idempotency broken", k)
		}
	}
	if len(arch1.objects) != len(arch2.objects) {
		t.Errorf("object counts differ: %d vs %d", len(arch1.objects), len(arch2.objects))
	}
	if len(idx1.rows) != 1 || len(idx2.rows) != 1 {
		t.Fatalf("want 1 row each, got %d and %d", len(idx1.rows), len(idx2.rows))
	}
	if idx1.rows[0].EventID != idx2.rows[0].EventID {
		t.Errorf("row event ids differ: %q vs %q", idx1.rows[0].EventID, idx2.rows[0].EventID)
	}
}

// failAfterArchiveStore fails every object put after the first (envelope).
type failAfterArchiveStore struct {
	fakeArchiveStore
	failFrom int
	count    int
}

func (f *failAfterArchiveStore) PutObject(ctx context.Context, key string, data []byte) error {
	f.count++
	if f.count > f.failFrom {
		return errors.New("s3 injected failure")
	}
	return f.fakeArchiveStore.PutObject(ctx, key, data)
}

// failIndexStore fails every PutEvent.
type failIndexStore struct{}

func (f *failIndexStore) PutEvent(ctx context.Context, row eventRow) error {
	return errors.New("ddb injected failure")
}

func TestPersistPartialFailureStillSucceeds(t *testing.T) {
	d := &deps{
		keys:    &fakeKeyStore{valid: map[string]string{"proj1": "key1"}},
		arch:    &failAfterArchiveStore{failFrom: 2}, // envelope + item 0 succeed, item 1 fails
		index:   &fakeIndexStore{},
		limiter: newRateLimiter(1000, 1000),
		now:     func() time.Time { return time.Date(2026, 8, 6, 1, 0, 0, 0, time.UTC) },
	}
	body := envelopeBody(t,
		`{"event_id":"e1"}`,
		`{"type":"event"}`,
		`{"event_id":"e1"}`,
		`{"type":"session"}`,
		`{"sid":"s1"}`,
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
		t.Fatalf("status = %d (want 200 with partial success), body = %s", resp.StatusCode, resp.Body)
	}
}

func TestPersistAllItemsFailReturnsError(t *testing.T) {
	d := &deps{
		keys:    &fakeKeyStore{valid: map[string]string{"proj1": "key1"}},
		arch:    &failAfterArchiveStore{failFrom: 1},
		index:   &failIndexStore{},
		limiter: newRateLimiter(1000, 1000),
		now:     func() time.Time { return time.Date(2026, 8, 6, 1, 0, 0, 0, time.UTC) },
	}
	body := envelopeBody(t,
		`{"event_id":"e1"}`,
		`{"type":"event"}`,
		`{"event_id":"e1"}`,
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
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d (want 500 when every item fails), body = %s", resp.StatusCode, resp.Body)
	}
}

func TestEventIDFromItemsValidates(t *testing.T) {
	if got := eventIDFromItems([]Item{{Header: ItemHeader{Type: "event"}, Payload: []byte(`{"event_id":"e1"}`)}}); got != "e1" {
		t.Errorf("got %q, want e1", got)
	}
	if got := eventIDFromItems([]Item{{Header: ItemHeader{Type: "event"}, Payload: []byte(`{"event_id":"../../x"}`)}}); got != "" {
		t.Errorf("malicious id accepted: %q", got)
	}
	if got := eventIDFromItems([]Item{{Header: ItemHeader{Type: "event"}, Payload: []byte(`{"message":"no id"}`)}}); got != "" {
		t.Errorf("missing id should yield empty, got %q", got)
	}
}

func TestDeterministicIDStable(t *testing.T) {
	raw := []byte(`{"message":"same"}`)
	a := deterministicID(raw)
	b := deterministicID(raw)
	if a != b || len(a) != 32 || !validEventID(a) {
		t.Errorf("deterministicID unstable or invalid: %q %q", a, b)
	}
	if deterministicID([]byte(`{"message":"diff"}`)) == a {
		t.Error("different raw bytes must yield different ids")
	}
}

func BenchmarkValidEventID(b *testing.B) {
	s := "0123456789abcdef0123456789abcdef"
	for i := 0; i < b.N; i++ {
		_ = validEventID(s)
	}
	_ = fmt.Sprint() // keep fmt imported
}
