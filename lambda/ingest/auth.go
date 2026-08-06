package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// authInfo describes the authentication present on a request.
//
// Present is true when ANY auth form exists (X-Sentry-Auth header, query
// `sentry_key`, or envelope header `dsn`). Key is the extracted public key,
// which may be empty when an auth form exists but carries no usable key.
type authInfo struct {
	Present bool
	Key     string
}

// extractAuthKey pulls the DSN public key from the request. It checks the
// header form, the query-string form, and — when body is the raw envelope
// body — the envelope header `dsn` form. If multiple forms are present, they
// must agree; a disagreement yields an error (caller responds 401).
func extractAuthKey(req events.APIGatewayProxyRequest, body []byte) (authInfo, error) {
	var info authInfo

	headerKey, headerPresent := sentryKeyFromHeader(req.Headers)
	if headerPresent {
		info.Present = true
		info.Key = headerKey
	}

	queryKey, queryPresent := sentryKeyFromQuery(req)
	if queryPresent {
		info.Present = true
		if info.Key != "" && queryKey != "" && info.Key != queryKey {
			return authInfo{}, fmt.Errorf("sentry_key disagreement between header and query")
		}
		if queryKey != "" {
			info.Key = queryKey
		}
	}

	if !info.Present {
		// Envelope-only third form: the envelope header may carry the full DSN.
		if dsnKey, ok := dsnFromEnvelopeHeader(body); ok {
			info.Present = true
			info.Key = dsnKey
		}
	}

	return info, nil
}

// sentryKeyFromHeader parses the X-Sentry-Auth header form:
//
//	X-Sentry-Auth: Sentry sentry_version=7, sentry_client=<name>/<version>,
//	              sentry_key=<public_key>[, sentry_secret=<secret_key>]
func sentryKeyFromHeader(headers map[string]string) (key string, present bool) {
	value := firstHeader(headers, "X-Sentry-Auth", "x-sentry-auth")
	if value == "" {
		return "", false
	}
	params := parseAuthHeaderParams(value)
	return params["sentry_key"], true
}

// parseAuthHeaderParams splits "Sentry k=v, k2=v2" into a map.
func parseAuthHeaderParams(value string) map[string]string {
	params := make(map[string]string)
	// The value begins with the auth scheme name ("Sentry ..."); everything
	// after the first space is a comma-separated list of k=v pairs.
	rest := value
	if idx := strings.IndexByte(rest, ' '); idx >= 0 {
		rest = rest[idx+1:]
	}
	for _, part := range strings.Split(rest, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		params[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
	}
	return params
}

// sentryKeyFromQuery checks the query-string form:
//
//	?sentry_version=7&sentry_key=<public_key>[&sentry_secret=<secret_key>]
func sentryKeyFromQuery(req events.APIGatewayProxyRequest) (key string, present bool) {
	// QueryStringParameters is authoritative when present; fall back to the
	// multi-value map for clients that repeat parameters.
	if req.QueryStringParameters != nil {
		if k := req.QueryStringParameters["sentry_key"]; k != "" {
			return k, true
		}
	}
	if req.MultiValueQueryStringParameters != nil {
		if vals := req.MultiValueQueryStringParameters["sentry_key"]; len(vals) > 0 && vals[0] != "" {
			return vals[0], true
		}
	}
	return "", false
}

// dsnFromEnvelopeHeader peeks at an envelope body's header line and, if it
// carries a "dsn" field, returns the public key parsed from that DSN.
func dsnFromEnvelopeHeader(body []byte) (key string, ok bool) {
	if len(body) == 0 {
		return "", false
	}
	// The envelope header is the first line of the body.
	line := body
	if idx := indexByte(body, '\n'); idx >= 0 {
		line = body[:idx]
	}
	header, err := unmarshalEnvelopeHeaderLine(line)
	if err != nil {
		return "", false
	}
	if header.DSN == "" {
		return "", false
	}
	key, err = publicKeyFromDSN(header.DSN)
	if err != nil {
		return "", false
	}
	return key, true
}

// unmarshalEnvelopeHeaderLine parses one envelope header line.
func unmarshalEnvelopeHeaderLine(line []byte) (EnvelopeHeader, error) {
	var h EnvelopeHeader
	err := json.Unmarshal(line, &h)
	return h, err
}

// publicKeyFromDSN parses the DSN formula:
//
//	DSN = '{PROTOCOL}://{PUBLIC_KEY}[:{SECRET_KEY}]@{HOST}{PATH}/{PROJECT_ID}'
//
// {SECRET_KEY} is optional; {PROJECT_ID} is always a string.
func publicKeyFromDSN(dsn string) (string, error) {
	// Strip the protocol: PROTOCOL://
	rest := dsn
	if idx := strings.Index(rest, "://"); idx >= 0 {
		rest = rest[idx+3:]
	}
	// Split userinfo from host: USERINFO@HOST...
	at := strings.IndexByte(rest, '@')
	if at < 0 {
		return "", fmt.Errorf("DSN has no userinfo separator '@'")
	}
	userinfo := rest[:at]
	if userinfo == "" {
		return "", fmt.Errorf("DSN userinfo is empty")
	}
	// userinfo = PUBLIC_KEY[:SECRET_KEY]
	if idx := strings.IndexByte(userinfo, ':'); idx >= 0 {
		userinfo = userinfo[:idx]
	}
	if userinfo == "" {
		return "", fmt.Errorf("DSN public key is empty")
	}
	return userinfo, nil
}

func firstHeader(headers map[string]string, names ...string) string {
	for _, n := range names {
		if v, ok := headers[n]; ok {
			return v
		}
	}
	return ""
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
