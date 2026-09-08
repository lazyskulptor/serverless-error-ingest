package main

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

func TestParseEnvelopeBasic(t *testing.T) {
	body := strings.Join([]string{
		`{"event_id":"a"}`,
		`{"type":"event"}`,
		`{"event_id":"a","level":"error","message":"boom"}`,
		`{"type":"session"}`,
		`{"sid":"s1","init":true}`,
	}, "\n")

	env, err := ParseEnvelope(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if len(env.Items) != 2 {
		t.Fatalf("want 2 items, got %d", len(env.Items))
	}
	if env.Items[0].Header.Type != "event" {
		t.Errorf("item 0 type = %q", env.Items[0].Header.Type)
	}
	if !bytes.Contains(env.Items[0].Payload, []byte("boom")) {
		t.Errorf("item 0 payload = %q", env.Items[0].Payload)
	}
	if env.Items[1].Header.Type != "session" {
		t.Errorf("item 1 type = %q", env.Items[1].Header.Type)
	}
	if !bytes.Contains(env.Items[1].Payload, []byte("s1")) {
		t.Errorf("item 1 payload = %q", env.Items[1].Payload)
	}
}

func TestParseEnvelopeZeroItems(t *testing.T) {
	env, err := ParseEnvelope(strings.NewReader(`{"event_id":"x"}` + "\n"))
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if len(env.Items) != 0 {
		t.Fatalf("want 0 items, got %d", len(env.Items))
	}
}

func TestParseEnvelopeEmptyHeaderIsValid(t *testing.T) {
	env, err := ParseEnvelope(strings.NewReader("{}\n"))
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if env.Header.DSN != "" {
		t.Errorf("dsn = %q", env.Header.DSN)
	}
}

func TestParseEnvelopeLengthPrefixedWithEmbeddedNewline(t *testing.T) {
	// The attachment payload contains a raw '\n' byte; the item header
	// declares its exact byte length, so the parser must NOT truncate at the
	// embedded newline.
	payload := "line1\nline2\nline3"
	body := "{}\n" +
		`{"type":"attachment","length":` + itoa(len(payload)) + "}\n" +
		payload + "\n"

	env, err := ParseEnvelope(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if len(env.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(env.Items))
	}
	if got := string(env.Items[0].Payload); got != payload {
		t.Errorf("payload mismatch:\n got %q\nwant %q", got, payload)
	}
}

func TestParseEnvelopeImplicitLengthSessionItem(t *testing.T) {
	body := "{}\n" +
		`{"type":"session"}` + "\n" +
		`{"sid":"s1","init":true}` + "\n"

	env, err := ParseEnvelope(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if len(env.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(env.Items))
	}
	if env.Items[0].Header.Length != nil {
		t.Errorf("implicit item should have nil length")
	}
	if !bytes.Contains(env.Items[0].Payload, []byte("s1")) {
		t.Errorf("payload = %q", env.Items[0].Payload)
	}
}

func TestParseEnvelopeTrailingNewline(t *testing.T) {
	body := "{}\n" +
		`{"type":"event"}` + "\n" +
		`{"message":"x"}` + "\n" + "\n"
	env, err := ParseEnvelope(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if len(env.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(env.Items))
	}
}

func TestParseEnvelopeMalformedLengthNotFollowedByNewline(t *testing.T) {
	body := "{}\n" +
		`{"type":"attachment","length":4}` + "\n" +
		"abcdX"
	_, err := ParseEnvelope(strings.NewReader(body))
	if err == nil {
		t.Fatal("expected malformed error (payload not followed by newline)")
	}
	if !strings.Contains(err.Error(), "newline") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseEnvelopeTruncatedPayload(t *testing.T) {
	body := "{}\n" +
		`{"type":"attachment","length":100}` + "\n" +
		"short"
	_, err := ParseEnvelope(strings.NewReader(body))
	if err == nil {
		t.Fatal("expected malformed error (truncated payload)")
	}
}

func TestParseEnvelopeCarriageReturnBelongsToLine(t *testing.T) {
	// A '\r' immediately before '\n' is not a newline — it belongs to the
	// payload. The payload here is `{"m":"x"}` followed by '\r'.
	body := "{}\n" +
		`{"type":"event"}` + "\n" +
		"{\"m\":\"x\"}\r\n"
	env, err := ParseEnvelope(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if got := string(env.Items[0].Payload); !strings.HasSuffix(got, "\r") {
		t.Errorf("expected payload to keep trailing \\r, got %q", got)
	}
}

func TestParseEnvelopeUnknownItemTypeAccepted(t *testing.T) {
	body := "{}\n" +
		`{"type":"check_in"}` + "\n" +
		`{"check_in_id":"c1"}` + "\n"
	env, err := ParseEnvelope(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if len(env.Items) != 1 || env.Items[0].Header.Type != "check_in" {
		t.Fatalf("unexpected items: %+v", env.Items)
	}
}

func TestParseEnvelopeRejectsTooManyItems(t *testing.T) {
	var body strings.Builder
	body.WriteString("{}\n")
	for i := 0; i < maxEnvelopeItems+1; i++ {
		body.WriteString("{\"type\":\"event\",\"length\":0}\n\n")
	}
	if _, err := ParseEnvelope(strings.NewReader(body.String())); err == nil {
		t.Fatal("expected item-count error")
	}
}

func TestParseEnvelopeRejectsOversizedDeclaredItem(t *testing.T) {
	body := "{}\n{\"type\":\"attachment\",\"length\":" + itoa(maxItemBytes+1) + "}\n"
	if _, err := ParseEnvelope(strings.NewReader(body)); err == nil {
		t.Fatal("expected item-size error")
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
