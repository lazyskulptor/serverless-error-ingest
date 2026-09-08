# Serverless Sentry-Compatible Error Ingest

Open-source, serverless error/event collection service that accepts the same
wire protocol as Sentry. Existing Sentry SDKs (`@sentry/browser` and friends)
can point their DSN at this project and send events with **only a DSN change**.

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
