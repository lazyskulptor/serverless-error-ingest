package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// keyStore validates a DSN public key against the projects registry.
type keyStore interface {
	ValidateKey(ctx context.Context, projectID, publicKey string) (bool, error)
}

// archiveStore writes raw payloads to S3.
type archiveStore interface {
	PutObject(ctx context.Context, key string, data []byte) error
}

// indexStore writes event metadata rows to DynamoDB.
type indexStore interface {
	PutEvent(ctx context.Context, row eventRow) error
}

// deps bundles the handler's dependencies (injected for tests).
type deps struct {
	keys         keyStore
	arch         archiveStore
	index        indexStore
	limiter      *rateLimiter
	now          func() time.Time
	maxBodyBytes int
	eventTTLDays int
}

const (
	routeEnvelope = "envelope"
	routeStore    = "store"
)

// handleRequest is the ingest entrypoint shared by the Lambda and tests.
func handleRequest(ctx context.Context, req events.APIGatewayProxyRequest, d *deps) (events.APIGatewayProxyResponse, error) {
	now := d.now()

	// 1. Body (API Gateway base64-encodes bodies whose content type is listed
	//    in the REST API binary media types).
	body, err := decodeBody(req)
	if err != nil {
		return respondBadRequest("request body is not valid base64")
	}

	// 1b. Enforce the payload cap before any decompression or parsing.
	if d.maxBodyBytes > 0 && len(body) > d.maxBodyBytes {
		return respondBadRequest("payload exceeds maximum size")
	}

	// 2. Authentication: query sentry_key, X-Sentry-Auth header, or envelope
	//    header `dsn`. No auth in any form -> 403; key present but unknown -> 401.
	auth, err := extractAuthKey(req, body)
	if err != nil {
		// Forms disagree — treat as an invalid key.
		return respondUnauthorized()
	}
	if !auth.Present {
		return respondForbidden()
	}
	if auth.Key == "" {
		return respondUnauthorized()
	}

	projectID := projectIDOf(req, body)
	valid, err := d.keys.ValidateKey(ctx, projectID, auth.Key)
	if err != nil {
		slog.ErrorContext(ctx, "key validation failed", "project_id", projectID, "error", err)
		return respondJSON(http.StatusInternalServerError, "key validation failed", nil)
	}
	if !valid {
		return respondUnauthorized()
	}

	// 3. Per-key rate limiting (abuse prevention) -> 429 + Retry-After.
	if d.limiter != nil {
		allowed, retryAfter := d.limiter.Allow(auth.Key, now)
		if !allowed {
			return respondRateLimited(retryAfter)
		}
	}

	// 4. Decompress per Content-Encoding (gzip/deflate/br/zstd).
	decBody, err := decompressBody(contentEncoding(req), body, int64(d.maxBodyBytes))
	if err != nil {
		slog.ErrorContext(ctx, "decompression failed", "error", err)
		return respondBadRequest(fmt.Sprintf("decompression failed: %v", err))
	}

	// 5. Parse envelope (or single JSON event for /store/). Never reject an
	//    envelope because it contains an item type this project doesn't
	//    analyze — every item is archived as an opaque payload.
	route := routeOf(req)
	var (
		items []Item
		raw   []byte
	)
	if route == routeStore {
		items = []Item{{Header: ItemHeader{Type: "event"}, Payload: decBody}}
		raw = decBody
	} else {
		env, err := ParseEnvelope(bytes.NewReader(decBody))
		if err != nil {
			slog.ErrorContext(ctx, "invalid envelope", "error", err)
			return respondBadRequest(fmt.Sprintf("invalid envelope: %v", err))
		}
		items = env.Items
		raw = decBody
	}

	eventID := eventIDFromItems(items)
	if eventID == "" {
		// No client-supplied id (or it failed validation): derive a stable
		// id from the raw bytes so identical retries collapse onto the same
		// storage keys instead of creating duplicates.
		eventID = deterministicID(raw)
	}

	// 6. Persist: raw envelope + one S3 object and one DynamoDB row per item.
	if err := persist(ctx, d, projectID, eventID, raw, items, now); err != nil {
		slog.ErrorContext(ctx, "persist failed", "project_id", projectID, "event_id", eventID, "error", err)
		return respondJSON(http.StatusInternalServerError, "persistence failed", nil)
	}

	slog.InfoContext(ctx, "ingested",
		"project_id", projectID, "event_id", eventID, "items", len(items),
		"duration_ms", time.Since(now).Milliseconds())
	return respondID(eventID)
}

// eventRow is one DynamoDB metadata row per envelope item.
type eventRow struct {
	ProjectID string `dynamodbav:"project_id"`
	EventID   string `dynamodbav:"event_id"`
	Timestamp string `dynamodbav:"timestamp"`
	Level     string `dynamodbav:"level,omitempty"`
	Platform  string `dynamodbav:"platform,omitempty"`
	ItemType  string `dynamodbav:"item_type"`
	Count     int    `dynamodbav:"count"`
	Status    string `dynamodbav:"status"`
	S3Path    string `dynamodbav:"s3_path"`
	// ExpiresAt (epoch seconds) drives the DynamoDB TTL for cost control.
	ExpiresAt int64 `dynamodbav:"expires_at,omitempty"`
}

// persist archives the full raw envelope to S3 and writes one metadata row per
// item (raw payload per item to S3 as well). Only event ids/counts are logged.
//
// Partial-failure semantics (documented in docs/ARCHITECTURE.md):
//   - The envelope archive is authoritative: if it fails, the whole request
//     fails (500) so we never leave orphaned items without their raw source.
//   - Items are best-effort: a failed item is logged and skipped. The request
//     still succeeds (200) as long as at least one item persisted; it only
//     fails if every item failed.
func persist(ctx context.Context, d *deps, projectID, eventID string, raw []byte, items []Item, now time.Time) error {
	// Defense in depth: never let client-supplied input reach a storage key
	// unvalidated.
	if !validEventID(eventID) {
		eventID = deterministicID(raw)
	}

	date := now.UTC().Format("2006-01-02")
	envKey := fmt.Sprintf("projects/%s/%s/%s.envelope", projectID, date, eventID)
	if err := d.arch.PutObject(ctx, envKey, raw); err != nil {
		return fmt.Errorf("archiving envelope: %w", err)
	}

	failed := 0
	for i, item := range items {
		itemType := sanitizeItemType(item.Header.Type)
		itemKey := fmt.Sprintf("projects/%s/%s/%s/items/%d-%s", projectID, date, eventID, i, itemType)
		if err := d.arch.PutObject(ctx, itemKey, item.Payload); err != nil {
			slog.ErrorContext(ctx, "persist item s3 failed",
				"item", i, "project_id", projectID, "event_id", eventID, "error", err)
			failed++
			continue
		}

		row := eventRow{
			ProjectID: projectID,
			EventID:   sortKeyOf(eventID, itemType, i),
			Timestamp: now.UTC().Format(time.RFC3339),
			ItemType:  itemType,
			Count:     1,
			Status:    "received",
			S3Path:    itemKey,
		}
		if d.eventTTLDays > 0 {
			row.ExpiresAt = now.Add(time.Duration(d.eventTTLDays) * 24 * time.Hour).Unix()
		}
		if itemType == "event" {
			meta := parseEventMeta(item.Payload)
			if meta.Timestamp != "" {
				row.Timestamp = meta.Timestamp
			}
			row.Level = meta.Level
			row.Platform = meta.Platform
		}
		if err := d.index.PutEvent(ctx, row); err != nil {
			slog.ErrorContext(ctx, "persist item ddb failed",
				"item", i, "project_id", projectID, "event_id", eventID, "error", err)
			failed++
			continue
		}
	}

	if failed == len(items) && len(items) > 0 {
		return fmt.Errorf("all %d items failed to persist", failed)
	}
	if failed > 0 {
		slog.WarnContext(ctx, "persist partial failure",
			"project_id", projectID, "event_id", eventID, "failed", failed, "total", len(items))
	}
	return nil
}

// eventMeta holds the few event-schema fields normalized for `event` items.
type eventMeta struct {
	EventID   string `json:"event_id"`
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Platform  string `json:"platform"`
}

// parseEventMeta extracts normalization fields from a Sentry event payload.
func parseEventMeta(payload []byte) eventMeta {
	var m eventMeta
	// Ignore parse errors: an opaque/unparseable event payload is still
	// archived; normalization is best-effort.
	_ = json.Unmarshal(payload, &m)
	return m
}

// sortKeyOf builds the DynamoDB sort key. The first item in an envelope owns
// the bare event_id; later items (session/transaction/attachment/...) get a
// suffixed key so one row per item never collides.
func sortKeyOf(eventID, itemType string, seq int) string {
	if seq == 0 {
		return eventID
	}
	return fmt.Sprintf("%s#%s#%d", eventID, itemType, seq)
}

// eventIDFromItems returns the first `event` item's own id when present and
// valid; otherwise "" so the caller falls back to a server-derived id.
func eventIDFromItems(items []Item) string {
	for _, it := range items {
		if it.Header.Type == "event" {
			if id := parseEventMeta(it.Payload).EventID; validEventID(id) {
				return id
			}
		}
	}
	return ""
}

// validEventID reports whether s is a safe, Sentry-style event id: 1-64 hex
// characters. Only such ids are ever used in S3 keys or DynamoDB sort keys;
// anything else is attacker-controlled input and must never reach a key.
func validEventID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// deterministicID derives a stable event id from the raw request bytes so an
// identical retried request collapses onto the same storage keys instead of
// creating duplicates.
func deterministicID(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

func sanitizeItemType(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return "unknown"
	}
	return strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(t)
}

// --- request helpers ---

func decodeBody(req events.APIGatewayProxyRequest) ([]byte, error) {
	if req.IsBase64Encoded {
		return base64.StdEncoding.DecodeString(req.Body)
	}
	return []byte(req.Body), nil
}

func routeOf(req events.APIGatewayProxyRequest) string {
	switch {
	case strings.HasSuffix(req.Resource, "/store"), strings.Contains(req.Path, "/store/"):
		return routeStore
	default:
		return routeEnvelope
	}
}

func contentEncoding(req events.APIGatewayProxyRequest) string {
	if v := req.Headers["Content-Encoding"]; v != "" {
		return v
	}
	if v := req.Headers["content-encoding"]; v != "" {
		return v
	}
	return ""
}

// projectIDOf resolves the project id: the path parameter is authoritative;
// fall back to the envelope header DSN when absent.
func projectIDOf(req events.APIGatewayProxyRequest, body []byte) string {
	if req.PathParameters != nil {
		if pid := req.PathParameters["project_id"]; pid != "" {
			return pid
		}
	}
	return dsnProjectID(body)
}

// dsnProjectID extracts the {PROJECT_ID} segment from an envelope header DSN.
func dsnProjectID(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	line := body
	if idx := bytes.IndexByte(body, '\n'); idx >= 0 {
		line = body[:idx]
	}
	h, err := unmarshalEnvelopeHeaderLine(line)
	if err != nil || h.DSN == "" {
		return ""
	}
	dsn := h.DSN
	if idx := strings.Index(dsn, "://"); idx >= 0 {
		dsn = dsn[idx+3:]
	}
	if at := strings.IndexByte(dsn, '@'); at >= 0 {
		dsn = dsn[at+1:]
	}
	if slash := strings.LastIndexByte(dsn, '/'); slash >= 0 {
		return dsn[slash+1:]
	}
	return ""
}

// --- response helpers (all responses carry CORS headers) ---

func corsHeaders() map[string]string {
	return map[string]string{
		"Access-Control-Allow-Origin": "*",
	}
}

func respondJSON(status int, message string, extra map[string]string) (events.APIGatewayProxyResponse, error) {
	headers := corsHeaders()
	for k, v := range extra {
		headers[k] = v
	}
	body := ""
	if status != http.StatusOK {
		detail := message
		if detail == "" {
			detail = http.StatusText(status)
		}
		payload, _ := json.Marshal(map[string]any{"detail": detail})
		body = string(payload)
	}
	return events.APIGatewayProxyResponse{
		StatusCode: status,
		Headers:    headers,
		Body:       body,
	}, nil
}

func respondID(eventID string) (events.APIGatewayProxyResponse, error) {
	headers := corsHeaders()
	headers["Content-Type"] = "application/json"
	payload, _ := json.Marshal(map[string]string{"id": eventID})
	return events.APIGatewayProxyResponse{
		StatusCode: http.StatusOK,
		Headers:    headers,
		Body:       string(payload),
	}, nil
}

func respondBadRequest(message string) (events.APIGatewayProxyResponse, error) {
	headers := corsHeaders()
	headers["X-Sentry-Error"] = message
	return respondJSONWithHeaders(http.StatusBadRequest, message, headers)
}

func respondUnauthorized() (events.APIGatewayProxyResponse, error) {
	return respondJSONWithHeaders(http.StatusUnauthorized, "invalid sentry public key", corsHeaders())
}

func respondForbidden() (events.APIGatewayProxyResponse, error) {
	return respondJSONWithHeaders(http.StatusForbidden, "missing authentication", corsHeaders())
}

func respondRateLimited(retryAfter time.Duration) (events.APIGatewayProxyResponse, error) {
	headers := corsHeaders()
	headers["Retry-After"] = fmt.Sprintf("%d", int(retryAfter.Seconds()))
	return respondJSONWithHeaders(http.StatusTooManyRequests, "rate limit exceeded", headers)
}

func respondJSONWithHeaders(status int, message string, headers map[string]string) (events.APIGatewayProxyResponse, error) {
	payload, _ := json.Marshal(map[string]any{"detail": message})
	return events.APIGatewayProxyResponse{
		StatusCode: status,
		Headers:    headers,
		Body:       string(payload),
	}, nil
}
