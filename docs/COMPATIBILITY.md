# Sentry Wire Protocol — Exact Contract

This document transcribes the wire contract this service implements, verbatim
from the project plan (which was itself sourced from
[develop.sentry.dev](https://develop.sentry.dev/sdk/foundations/envelopes/) and
the related transport docs). Do not re-derive these values from memory.

Sources of truth:
- https://develop.sentry.dev/sdk/foundations/envelopes/
- https://develop.sentry.dev/sdk/foundations/envelopes/envelope-items/
- https://develop.sentry.dev/sdk/foundations/transport/
- https://develop.sentry.dev/sdk/foundations/transport/authentication/
- https://develop.sentry.dev/sdk/foundations/transport/compression/
- https://develop.sentry.dev/sdk/foundations/transport/rate-limiting/

## 1. DSN parsing (exact formula)

```
DSN            = '{PROTOCOL}://{PUBLIC_KEY}:{SECRET_KEY}@{HOST}{PATH}/{PROJECT_ID}'
BASE_URI       = '{PROTOCOL}://{HOST}{PATH}'
ENDPOINT URL   = '{BASE_URI}/api/{PROJECT_ID}/{ENDPOINT}/'
```

- `{SECRET_KEY}` is optional and deprecated; parsing MUST NOT require it.
- `{PROJECT_ID}` is always a string (even if numeric-looking).
- Example: DSN `https://<public-key>@sentry.example.com/1` → base URI
  `https://sentry.example.com` → store endpoint
  `https://sentry.example.com/api/1/store/`.
- In-scope endpoints for this project: `/envelope/` and `/store/` only.
  Out of scope (do not implement, return 404): `/minidump/`, `/unreal/`,
  `/playstation/`, `/security/`.

## 2. Authentication (exact fields)

Two equally valid forms; both may be present and must agree if so:

- **Header form**:
  `X-Sentry-Auth: Sentry sentry_version=7, sentry_client=<name>/<version>, sentry_key=<public_key>[, sentry_secret=<secret_key>]`
- **Query-string form**:
  `?sentry_version=7&sentry_key=<public_key>[&sentry_secret=<secret_key>]`
- `sentry_key` is required in both forms. `sentry_version` required (current
  value `7`). `sentry_secret` optional/legacy — accept if present, never
  require it.
- Envelope-only third form: the envelope header itself may carry `"dsn"` with
  the full DSN string as an alternative to header/query auth.
- **If no authentication is present in any form, reject with `403 Forbidden`**
  (this exact status is specified for the "completely missing" case). If a key
  is present but not found in the `projects` registry, reject with
  `401 Unauthorized` (reasonable default; not pinned by an explicit status code
  in the fetched spec text — re-verify against a live Sentry error response
  before treating this as final).

## 3. Content-Type handling

- Accept `application/x-sentry-envelope` (default if the header is missing)
  for `/envelope/`.
- Also accept `text/plain`, `multipart/form-data`, and
  `application/x-www-form-urlencoded` on `/envelope/` and treat them
  identically to `application/x-sentry-envelope` — these exist specifically so
  browser SDKs can avoid a CORS preflight by using a CORS-safelisted content
  type. Do not reject a request solely because of one of these content types.

## 4. Envelope grammar (exact, do not approximate with naive newline-split)

```
Envelope = Headers { "\n" Item } [ "\n" ] ;
Item     = Headers "\n" Payload ;
Payload  = { * } ;
```

- Newlines are `\n` only (ASCII 10). A `\r` immediately before `\n` is NOT a
  newline — it belongs to the preceding payload/line.
- Headers are always exactly one line of compact JSON, always followed by `\n`
  or EOF. Empty headers `{}` are valid.
- An envelope may have zero items (just the header line) — this is valid and
  should be archived, not rejected.
- Item header `length` (int) is the exact **byte length** of the following
  payload. **When `length` is present, read exactly that many bytes for the
  payload regardless of any `\n` bytes inside it** (payloads such as
  attachments may contain raw newlines). After reading `length` bytes, the next
  byte must be `\n` or EOF; anything else means the envelope is malformed.
- **When `length` is absent**, the payload is implicitly terminated by the next
  `\n` (i.e. scan for the next newline as the payload boundary). This case is
  common for `session` items.
- Unknown item `type` values MUST be accepted, retained, and
  forwarded/archived — never rejected for having an unrecognized type.
- Reserved types that must never be emitted by a writer but may need graceful
  handling if seen: `security`, `unreal_report`, `form_data`.
- Parsing algorithm (pseudocode):
  ```
  read one line -> envelope_header (JSON)
  while bytes remain:
    read one line -> item_header (JSON)
    if item_header.length is present:
      payload = read_exact_bytes(item_header.length)
      expect next byte == '\n' or EOF, else malformed
    else:
      payload = read_until_next_newline_or_EOF()
    record (item_header, payload)
  ```

## 5. Response contract (exact)

- **Success (both `/envelope/` and `/store/`)**: `200 OK`,
  `Content-Type: application/json`, body `{"id": "<event_id>"}`. Do NOT use
  `202` — the authoritative spec only documents `200` as the success status
  SDKs check for.
- **Malformed request** (e.g. failed to parse/decompress body): `400 Bad
  Request`, optional `X-Sentry-Error` header with a precise message, JSON body
  `{"detail": "<message>", "causes": ["<optional>", ...]}`.
- **No authentication provided at all**: `403 Forbidden`.
- **Key present but invalid/unknown**: `401 Unauthorized` (project's default
  choice; re-verify exact code against a live Sentry response if strict parity
  is required later).
- **Rate limited**: `429 Too Many Requests` with a `Retry-After: <seconds>`
  header at minimum (SDKs MUST honor this). Optionally also emit
  `X-Sentry-Rate-Limits: <retry_after>:<categories>:<scope>:<reason_code>`
  (categories may be empty to mean "all"); this header may also appear on `200`
  responses to preemptively signal upcoming limits. Implementing only
  `Retry-After` on `429` is sufficient for SDK compliance; the detailed header
  is an enhancement.

## 6. Compression (Content-Encoding)

- Sentry accepts `gzip`, `deflate`, `br` (Brotli), and `zstd` as
  `Content-Encoding` values. Decompress before parsing.
- MVP priority: implement `gzip` and `deflate` first (these are what browser
  SDKs can produce via the standard `CompressionStream` API); document
  `br`/`zstd` as accepted-but-not-yet-implemented if deferred, rather than
  silently mishandling them as uncompressed bytes.

## 7. CORS headers (for the `OPTIONS` preflight and actual response)

Permitted request headers per spec (confirmed list; one entry in the source
list was not legible during fetch — re-check the Authentication doc if a client
reports a CORS header rejection): `x-sentry-auth`, `x-requested-with`,
`x-forwarded-for`, `origin`, `accept`, `authentication`, `authorization`,
`content-encoding`, `transfer-encoding`, plus `content-type` (always required
to be readable per general HTTP header handling; verify this is on Sentry's own
list if you need byte-exact parity).

- `Access-Control-Allow-Headers` should include at least:
  `Content-Type, X-Sentry-Auth, X-Requested-With, X-Forwarded-For, Origin,
  Accept, Authentication, Authorization, Content-Encoding, Transfer-Encoding`.
- `Access-Control-Allow-Methods: POST, OPTIONS`.
- Many real browser-SDK requests will use `text/plain` content type with no
  custom header and will not trigger a preflight at all (see §3); still
  implement `OPTIONS` handling as a safety net for clients that do use
  header-based auth or non-safelisted content types.

## 8. DSN mapping for SDKs

A stock Sentry SDK is pointed at this service by constructing its DSN per §1:

```
DSN = '{PROTOCOL}://{PUBLIC_KEY}@{HOST}{PATH}/{PROJECT_ID}'
```

The SDK derives the endpoint `{BASE_URI}/api/{PROJECT_ID}/envelope/` from that
DSN. Practically:

- `{PUBLIC_KEY}` — the `public_key` minted by `scripts/register`
- `{HOST}{PATH}` — the API Gateway base URL (from `tofu output api_gateway_url`,
  including any non-root stage path)
- `{PROJECT_ID}` — the registered project id

## 9. SDK verification log

Results of proving a stock Sentry SDK sends events with only a DSN change
(updated after each live verification):

| SDK | Version | Envelope status/body | Store status/body | Verified at |
|---|---|---|---|---|
| `@sentry/browser` (CDN bundle) | 7.119.2 | pending | pending | — |

## 10. Hardening changelog

Production-hardening changes (this section does not alter the wire contract
above — §1–§9 remain authoritative):

- **Payload cap**: bodies over `MAX_BODY_BYTES` (default 1 MiB) are rejected
  with `400 {"detail": "payload exceeds maximum size"}` before any
  decompression or parsing (the contract's "~1MB limit" enforcement point).
- **Storage-key safety**: client-supplied `event_id` values are validated as
  hex before use in S3 keys / DynamoDB sort keys; invalid values are replaced
  with a server-derived id.
- **Idempotent retries**: requests without a (valid) client event id derive a
  stable id from a hash of the raw body, so identical retries do not create
  duplicates.
- **Rate limiting layers**: WAFv2 rate-based rule (global, per source IP) +
  API Gateway usage plan (keyed clients) + in-Lambda token bucket (per DSN
  key) — `429` + `Retry-After` behavior unchanged.
- **Observability**: structured JSON logs (ids/counts only), X-Ray tracing,
  API Gateway access logs (metadata only), CloudWatch alarms.
- **Cost control**: S3 lifecycle (STANDARD_IA then expiry) and DynamoDB TTL on
  metadata rows.
- **Query default**: `GET /events` lists `event` rows by default; use
  `type=all` (or a specific type) to widen.

