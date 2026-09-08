# Architecture

## Overview

The service is a complete replacement for Sentry's **collection/ingestion
layer**: it speaks the same wire protocol so any Sentry SDK can be repointed at
this service via a DSN change, and it durably archives every accepted payload.
Analysis (grouping, summarization, UI, alerting) is out of scope; an AI
grouping pipeline is a later, optional phase.

## Data flow

```
Sentry SDK (any language, DSN pointed at this service)
  → POST /api/{project_id}/envelope/ or /api/{project_id}/store/
  → API Gateway (REST, POST only, size limit ~1MB)
  → Lambda ingest handler
      - X-Sentry-Auth / DSN public key validation
      - Envelope parser (JSON header line + item lines) / store JSON
      - Schema-normalize event JSON (Sentry event schema)
      - Write raw envelope/event to S3 (s3://<bucket>/projects/<project>/YYYY-MM-DD/<event_id>.envelope)
      - Write metadata row to DynamoDB (project, event_id, timestamp, level, platform, count, status)
  → 200 OK {"id": "<event_id>"}
```

## Components

### WAFv2 (global rate-based control)

- `aws_wafv2_web_acl` with a rate-based rule (default 2000 requests per 5
  minutes per source IP, `waf_rate_limit`) is associated with the API Gateway
  stage.
- This is the **primary** global abuse-prevention control; the in-Lambda token
  bucket (per execution environment, per DSN key) is documented
  **defense-in-depth**, not the sole control.

### API Gateway (REST)

- Routes: `POST /api/{project_id}/envelope/` and `POST /api/{project_id}/store/`
  (path parameter `project_id`); `OPTIONS` on every route returns the CORS
  headers from `docs/COMPATIBILITY.md`.
- Payload size limit ~1MB — **enforced in the ingest handler** (API Gateway
  REST has no configurable sub-10MB cap); see "Ingest persistence semantics".
- Stage access logs (request metadata only — never bodies) go to a CloudWatch
  log group; CloudWatch alarms cover Lambda errors and API 4xx/5xx rates.
- WAF and the Lambda limiter provide complementary IP- and DSN-key-based rate
  limiting.
- An optional regional custom domain uses an AWS ACM certificate. OpenTofu can
  place both certificate-validation and API records in either an existing
  Route 53 zone or an existing Cloudflare zone. Cloudflare is DNS-only by
  default; changing DNS provider does not replace Lambda or storage resources.

### Lambda ingest handler (Go)

- Reads the DSN public key from the request (`sentry_key` query param,
  `X-Sentry-Auth` header, or envelope header `dsn`), validates against the
  DynamoDB `projects` table.
- Decompresses the body per `Content-Encoding` (`gzip`, `deflate`, `br`,
  `zstd`) through a bounded reader.
- Parses envelopes using the exact length-prefixed grammar in
  `docs/COMPATIBILITY.md` (byte-exact reads, never naive newline splitting).
- Iterates every item regardless of `type`; archives each item's raw payload to
  S3 (path includes item type) and writes one DynamoDB metadata row per item.
- Only fully parses/normalizes the Sentry event schema for `event` items;
  other item types are archived as opaque payloads.
- Never logs event content — only ids/counts.

### S3

- Private, versioned bucket `log-collector-raw` (configurable).
- Raw objects stored at
  `projects/<project_id>/YYYY-MM-DD/<event_id>/items/<sequence>-<item_type>` and
  the full envelope at
  `projects/<project_id>/YYYY-MM-DD/<event_id>.envelope`. Deployments upgraded
  from older versions may retain historical `YYYY/MM/DD` keys until lifecycle
  expiration; existing objects are not migrated.
- **Storage-key safety**: the client-supplied `event_id` is validated against
  a hex charset (`^[a-fA-F0-9]{1,64}$`) before it is ever used in an S3 key
  or DynamoDB sort key; a value that fails validation is replaced with a
  server-derived id so attacker input can never escape the project namespace.

### Ingest persistence semantics

- **Payload cap**: bodies are rejected with `400` when either compressed or
  decompressed content exceeds `MAX_BODY_BYTES` (default 1 MiB). Envelope
  headers, item count, and individual item payloads are bounded as well.
- **Idempotency**: when an envelope/store payload carries no (valid) client
  `event_id`, the handler derives a stable id from a hash of the raw request
  bytes. Identical SDK retries therefore collapse onto the same S3 objects and
  DynamoDB rows instead of creating duplicates.
- **Partial-failure semantics**: the envelope archive is authoritative — if
  writing it fails, the whole request fails (`500`). Individual items are
  best-effort: a failed item is logged and skipped, and the request succeeds
  (`200`) as long as at least one item persisted; it only fails (`500`) when
  every item failed.

### DynamoDB

- `projects` table: partition key `project_id`; stores issued public key
  (and optional secret key), creation timestamp, status (active/disabled).
- `events` table: partition key `project_id`, sort key `event_id`, GSI on
  `timestamp`; stores metadata only (level, platform, item type, count, status,
  S3 path).
- **TTL**: every row carries `expires_at` (epoch seconds, set by the ingest
  handler, default `EVENT_TTL_DAYS=90`); the table has a TTL enabled on it for
  cost control.

### Cost control

- S3 lifecycle rule on the raw bucket: transition to `STANDARD_IA` after
  `raw_standard_ia_days` (default 30) and expire after `raw_expiration_days`
  (default 180).
- DynamoDB TTL on `events` rows (above).

### Observability

- The ingest Lambda emits structured JSON logs (`log/slog`) to CloudWatch —
  ids/counts only, never payload content.
- API Gateway stage access logs (request metadata only) go to a CloudWatch log
  group with 14-day retention.
- CloudWatch alarms: ingest Lambda errors, API 4xx (threshold) and 5xx.
- Growth/abuse alarms cover S3 object count and size, hourly API request volume,
  ingest Lambda throttles, and DynamoDB write throttles. An existing SNS topic
  may receive ALARM/OK actions; S3 storage metrics update daily.

### Dormant query source

- Query source is retained for future operator tooling, but the production
  stack creates no query Lambda or GET route.

### Lambda admin handler (Go) / registration script

- Creates a `projects` table row and mints a DSN-compatible public key.
- The operator hands the client
  `https://{public_key}@{api_gateway_host}/{project_id}` as the DSN.

## IAM

Least-privilege roles per Lambda handler:

- Ingest: `s3:PutObject` on the raw bucket, `dynamodb:PutItem` on the events
  table, `dynamodb:GetItem` on the projects table.
- Ingest also has `AWSLambdaBasicExecutionRole` for CloudWatch logging.
- API Gateway: account-level CloudWatch logging role (request metadata only).

## State management

Terraform state is stored remotely (S3 bucket + DynamoDB lock table) with a
distinct key per environment; see `infra/README.md` "State management" for the
one-time bootstrap steps.

The direct API Gateway URL remains an output even when a custom hostname is
enabled, providing a DNS-independent verification and rollback endpoint.

## Resilience / abuse prevention

- Missing auth → `403`; unknown key → `401`; malformed payload → `400`;
  oversized payload → `400`.
- Two layers of rate limiting: WAFv2 rate-based rule (global, per source IP)
  and an in-Lambda token bucket (per DSN key and execution environment,
  returning `429` + `Retry-After`).
- Every accepted envelope/item is archived even if its type is not analyzed.
