package main

import (
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func TestSentryKeyFromHeader(t *testing.T) {
	key, present := sentryKeyFromHeader(map[string]string{
		"X-Sentry-Auth": "Sentry sentry_version=7, sentry_client=test/1.0, sentry_key=abc123",
	})
	if !present {
		t.Fatal("expected header auth present")
	}
	if key != "abc123" {
		t.Errorf("key = %q", key)
	}
}

func TestSentryKeyFromHeaderLowercase(t *testing.T) {
	key, present := sentryKeyFromHeader(map[string]string{
		"x-sentry-auth": "Sentry sentry_version=7, sentry_key=abc123",
	})
	if !present || key != "abc123" {
		t.Errorf("present=%v key=%q", present, key)
	}
}

func TestSentryKeyFromHeaderWithSecret(t *testing.T) {
	key, present := sentryKeyFromHeader(map[string]string{
		"X-Sentry-Auth": "Sentry sentry_version=7, sentry_client=c/1, sentry_key=k1, sentry_secret=s1",
	})
	if !present || key != "k1" {
		t.Errorf("present=%v key=%q", present, key)
	}
}

func TestSentryKeyFromQuery(t *testing.T) {
	key, present := sentryKeyFromQuery(events.APIGatewayProxyRequest{
		QueryStringParameters: map[string]string{"sentry_version": "7", "sentry_key": "qkey"},
	})
	if !present || key != "qkey" {
		t.Errorf("present=%v key=%q", present, key)
	}
}

func TestPublicKeyFromDSN(t *testing.T) {
	cases := []struct {
		dsn  string
		want string
	}{
		{"https://pubkey@example.com/1", "pubkey"},
		{"https://pubkey:secret@example.com/1", "pubkey"},
		{"https://pubkey@example.com/1/2/3", "pubkey"},
	}
	for _, c := range cases {
		got, err := publicKeyFromDSN(c.dsn)
		if err != nil {
			t.Errorf("publicKeyFromDSN(%q): %v", c.dsn, err)
			continue
		}
		if got != c.want {
			t.Errorf("publicKeyFromDSN(%q) = %q, want %q", c.dsn, got, c.want)
		}
	}
}

func TestPublicKeyFromDSNInvalid(t *testing.T) {
	for _, dsn := range []string{"", "not-a-dsn", "https://example.com/1", "https://@example.com/1"} {
		if _, err := publicKeyFromDSN(dsn); err == nil {
			t.Errorf("expected error for DSN %q", dsn)
		}
	}
}

func TestExtractAuthKeyHeaderForm(t *testing.T) {
	info, err := extractAuthKey(events.APIGatewayProxyRequest{
		Headers: map[string]string{"X-Sentry-Auth": "Sentry sentry_version=7, sentry_key=h1"},
	}, nil)
	if err != nil {
		t.Fatalf("extractAuthKey: %v", err)
	}
	if !info.Present || info.Key != "h1" {
		t.Errorf("present=%v key=%q", info.Present, info.Key)
	}
}

func TestExtractAuthKeyQueryForm(t *testing.T) {
	info, err := extractAuthKey(events.APIGatewayProxyRequest{
		QueryStringParameters: map[string]string{"sentry_key": "q1"},
	}, nil)
	if err != nil {
		t.Fatalf("extractAuthKey: %v", err)
	}
	if !info.Present || info.Key != "q1" {
		t.Errorf("present=%v key=%q", info.Present, info.Key)
	}
}

func TestExtractAuthKeyAgreement(t *testing.T) {
	info, err := extractAuthKey(events.APIGatewayProxyRequest{
		Headers:               map[string]string{"X-Sentry-Auth": "Sentry sentry_version=7, sentry_key=same"},
		QueryStringParameters: map[string]string{"sentry_key": "same"},
	}, nil)
	if err != nil {
		t.Fatalf("extractAuthKey: %v", err)
	}
	if !info.Present || info.Key != "same" {
		t.Errorf("present=%v key=%q", info.Present, info.Key)
	}
}

func TestExtractAuthKeyDisagreement(t *testing.T) {
	_, err := extractAuthKey(events.APIGatewayProxyRequest{
		Headers:               map[string]string{"X-Sentry-Auth": "Sentry sentry_version=7, sentry_key=a"},
		QueryStringParameters: map[string]string{"sentry_key": "b"},
	}, nil)
	if err == nil {
		t.Fatal("expected disagreement error")
	}
}

func TestExtractAuthKeyEnvelopeDSNForm(t *testing.T) {
	body := []byte(`{"dsn":"https://envkey@example.com/1"}` + "\n" + `{"type":"event"}` + "\n" + `{}` + "\n")
	info, err := extractAuthKey(events.APIGatewayProxyRequest{}, body)
	if err != nil {
		t.Fatalf("extractAuthKey: %v", err)
	}
	if !info.Present || info.Key != "envkey" {
		t.Errorf("present=%v key=%q", info.Present, info.Key)
	}
}

func TestExtractAuthKeyNone(t *testing.T) {
	info, err := extractAuthKey(events.APIGatewayProxyRequest{}, nil)
	if err != nil {
		t.Fatalf("extractAuthKey: %v", err)
	}
	if info.Present {
		t.Error("expected no auth present")
	}
}

func TestDsnProjectID(t *testing.T) {
	body := []byte(`{"dsn":"https://k@example.com/my-project"}` + "\n")
	if got := dsnProjectID(body); got != "my-project" {
		t.Errorf("dsnProjectID = %q", got)
	}
}
