# Serverless Sentry Alternative for AWS

An open-source, self-hosted Sentry-compatible error and application log
collector for AWS. Keep existing Sentry SDKs (`@sentry/browser` and others),
change only the DSN, and archive errors and structured SDK events in your own S3
bucket with searchable metadata in DynamoDB. It runs serverlessly on API Gateway
and Lambda, without operating a full Sentry or centralized logging stack.

Use it when you need a lightweight Sentry alternative, serverless error
collector, structured application event archive, or simple log ingestion API
for development, internal tools, and custom observability pipelines. It
replaces Sentry's ingestion layer—not the Sentry dashboard, log search UI,
issue grouping, alerting, performance monitoring, or source-map processing.

## Why this project?

- **Sentry SDK compatible** — existing SDK transport works with a DSN change.
- **Self-hosted in your AWS account** — raw events stay in private S3 and
  metadata stays in DynamoDB.
- **Serverless and low-operations** — no Kubernetes, Kafka, ClickHouse, or
  always-on application servers.
- **OpenTofu deployment** — reproducible API Gateway, Lambda, WAF, storage,
  logging, alarms, DNS, and GitHub OIDC automation.
- **Collection-focused** — a small foundation for teams building their own
  error-processing, analytics, retention, or AI workflows.
- **Useful for application logs** — capture structured logs and exceptions sent
  through Sentry SDKs without deploying a general-purpose log platform.

If you need a complete error-monitoring product with UI and issue workflows,
consider self-hosted Sentry, GlitchTip, Bugsink, Highlight, or SigNoz instead.

## Lightweight log collection: where it fits

Many open-source logging tools are excellent but solve a broader problem:
Loki, OpenSearch, Graylog, Vector, Fluent Bit, and OpenTelemetry Collector
typically collect logs from hosts, containers, files, or multiple backends.
They may also require a separate storage and query stack. This project is
narrower: it accepts Sentry SDK error and structured event traffic directly,
then persists it on managed AWS services with no always-on collector cluster.

Choose this project for Sentry-compatible application error/log ingestion and
an S3 archive. Choose a general log platform when you need arbitrary text logs,
full-text search, dashboards, agents, traces, metrics, or multi-source routing.

## Architecture

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

Components:

- **API Gateway (REST)** — routes only `POST /api/{project_id}/envelope/` and
  `POST /api/{project_id}/store/`, with CORS and WAF rate limiting.
- **Lambda ingest handler (Go)** — DSN public-key validation, exact
  length-prefixed envelope parsing, `gzip`/`deflate`/`br`/`zstd`
  decompression, writes raw payloads to S3 and metadata to DynamoDB.
- **S3** — private, versioned bucket storing every raw envelope/event.
- **DynamoDB** — `projects` table (DSN keys registry) and `events` table
  (metadata/index: project_id + event_id, GSI on timestamp).
- **Dormant query source (Go)** — retained for future authenticated operator
  tooling, but no query Lambda or GET route is deployed.
- **Lambda admin handler (Go)** — project registration + DSN key issuance.

Language choice: Go for all Lambda handlers — fast cold start, exact
byte-level control for length-prefixed envelope parsing, mature
`aws-sdk-go-v2`, stdlib `compress/gzip`/`compress/flate`, `klauspost/compress`
(zstd), `andybalholm/brotli` (br).

## Scope

- **Sentry compatibility**: implement Sentry's ingest HTTP contract. This is
  the acceptance definition — a stock Sentry SDK must work with only a DSN
  change.
- **Collection only**: this project targets a complete replacement of Sentry's
  collection/ingestion layer, not its analysis/UI/alerting product. An
  envelope can contain multiple item types (`event`, `session`, `transaction`,
  `attachment`, `client_report`, `check_in`, ...); every item type is accepted
  and archived so no envelope is ever rejected just because it contains a type
  this project doesn't analyze yet.
- **AI not deployed**: no processor, schedule, or AI credential resources.

### Non-goals (explicit)

- No Sentry UI clone or public read/query endpoint.
- No grouping/dedupe/SourceMap processing in v1 (AI phase).
- No relay/transaction-heavy features (profiles, replays) in v1.

## Repository layout

```
infra/              OpenTofu infrastructure (API Gateway, Lambda, S3, DynamoDB, IAM)
lambda/ingest/      Ingest handler (Sentry wire compatibility)
lambda/admin/       Project registration / DSN key issuance
lambda/query/       Read/query API for ingested event metadata
examples/browser/   @sentry/browser compatibility proof page
docs/               COMPATIBILITY.md, ARCHITECTURE.md, QUERY.md, REGISTRATION.md, AI.md
```

## Getting started

1. Install Go 1.24+, `zip`, OpenTofu 1.6+, and the AWS CLI, then run
   `./scripts/build-lambdas.sh` from the repository root.
2. **Infrastructure**: `cd infra`, copy `terraform.tfvars.example` to
   `terraform.tfvars`, and set a globally unique raw bucket name. Leave
   `domain_name` empty for the direct API Gateway URL, or configure Route 53
   (`dns_provider = "aws"`) or Cloudflare (`dns_provider = "cloudflare"`). Run
   `tofu init && tofu plan && tofu apply`. Cloudflare credentials come from
   `CLOUDFLARE_API_TOKEN`, never from tfvars.
   `allow_destroy_data` defaults to `true` for clean development teardown.
   **Production must set it to `false` before the first apply** or destroy will
   permanently remove archived events and tables.
3. **Register a project**: `cd scripts/register && go run . -project <name>
   -host <api-host>`
   (creates a `projects` table row + DSN public key). See `docs/REGISTRATION.md`.
4. **Send events**: point a Sentry SDK DSN at
   `https://{public_key}@{api_gateway_host}/{project_id}`. See
    `examples/browser/` and `docs/COMPATIBILITY.md`.

For automated deployment, bootstrap GitHub OIDC under
`infra/bootstrap/github-oidc`, configure the protected environment described in
`infra/README.md`, and publish a GitHub Release. CI validates the exact commit
and stores its Lambda artifact; the Release workflow verifies that artifact,
applies a saved OpenTofu plan, and checks POST/S3/DynamoDB persistence.

## License

MIT — see [LICENSE](LICENSE).
