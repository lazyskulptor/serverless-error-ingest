# Infrastructure

OpenTofu-managed AWS infrastructure for the serverless Sentry-compatible
ingest service.

## Resources

| Resource | Purpose |
|---|---|
| API Gateway (REST) | `POST /api/{project_id}/envelope/`, `POST /api/{project_id}/store/`, `GET /api/projects/{project_id}/events`, `OPTIONS` CORS mock on every route |
| Lambda `sentry-ingest-ingest` | Sentry wire-protocol ingest handler (Go) |
| Lambda `sentry-ingest-query` | Read-only event metadata query handler (Go) |
| Lambda `sentry-ingest-processor` | Post-ingest grouping/summarization (Go, EventBridge scheduled) |
| S3 `raw` bucket | Private, versioned, SSE-encrypted archive of raw envelopes/events |
| DynamoDB `-projects` | DSN public-key registry (partition key `project_id`) |
| DynamoDB `-events` | Event metadata index (partition key `project_id`, sort key `event_id`, GSI `TimestampIndex` on `timestamp`) |
| Usage plan + API key | Per-key throttle/burst rate limit + daily quota |
| WAFv2 web ACL | Rate-based rule (per source IP) in front of the stage |
| CloudWatch | Lambda + API alarms, stage access logs (metadata only), X-Ray tracing active on Lambdas |

## Cost control

- S3 lifecycle: raw objects transition to `STANDARD_IA` after
  `raw_standard_ia_days` (default 30) and expire after `raw_expiration_days`
  (default 180).
- DynamoDB TTL: `events` rows expire after `event_ttl_days` (default 90); the
  ingest handler writes `expires_at` on every row.

## Abuse prevention

- 1MB payload cap enforced by API Gateway.
- Usage plan with per-key throttle (`rate_limit` req/s) and daily quota.
  Methods do **not** require the API key because stock Sentry SDKs never send
  one — keyed clients are throttled by the usage plan, and SDK-facing traffic
  is rate-limited inside the ingest handler (returns `429` + `Retry-After`).
- Gateway-level `THROTTLED` / `QUOTA_EXCEEDED` responses are mapped to `429`
  with a `Retry-After` header per the response contract in
  `docs/COMPATIBILITY.md`.

## Building the Lambda artifacts

Each handler is a Go module producing a zip for the `provided.al2023` runtime:

```sh
# ingest
cd lambda/ingest
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bootstrap .
zip function.zip bootstrap
mv function.zip ../../infra/../lambda/ingest/function.zip

# query
cd lambda/query
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bootstrap .
zip function.zip bootstrap

# processor (optional, for AI grouping/summarization)
cd lambda/processor
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bootstrap .
zip function.zip bootstrap
```

(`function.zip` files are git-ignored.)

## Usage

```sh
cp terraform.tfvars.example terraform.tfvars
# edit terraform.tfvars — raw_bucket_name must be globally unique

tofu init
tofu validate
tofu plan
tofu apply
```

## State management (remote backend)

State is stored in an S3 bucket with DynamoDB locking — never locally. The
backend bucket/table cannot be created by the same state they store
(chicken-and-egg), so bootstrap them once per account:

```sh
# 1. Create the state bucket + lock table (only needs AWS CLI, no tofu)
aws s3api create-bucket --bucket sentry-ingest-tfstate --region ap-northeast-2 \
  --create-bucket-configuration LocationConstraint=ap-northeast-2
aws s3api put-bucket-versioning \
  --bucket sentry-ingest-tfstate --versioning-configuration Status=Enabled
aws dynamodb create-table \
  --table-name sentry-ingest-tfstate-lock \
  --attribute-definitions AttributeName=LockID,AttributeType=S \
  --key-schema AttributeName=LockID,KeyType=HASH \
  --billing-mode PAY_PER_REQUEST \
  --region ap-northeast-2

# 2. Initialize with the backend (distinct key per environment)
tofu init \
  -backend-config="bucket=sentry-ingest-tfstate" \
  -backend-config="key=envs/<environment>/terraform.tfstate" \
  -backend-config="region=ap-northeast-2" \
  -backend-config="dynamodb_table=sentry-ingest-tfstate-lock" \
  -backend-config="encrypt=true"
```

Validation-only contexts (CI) use `tofu init -backend=false`, which skips
state access entirely.

## WAF

A `aws_wafv2_web_acl` with a rate-based rule (default 2000 requests per 5
minutes per source IP, `waf_rate_limit`) is associated with the API Gateway
stage as the primary global abuse-prevention control. The in-Lambda token
bucket remains as defense-in-depth for per-key SDK traffic (see
`docs/ARCHITECTURE.md`).

Outputs after apply:

- `api_gateway_url` — base URL
- `envelope_endpoint` / `store_endpoint` — ingest endpoints
- `raw_bucket`, `projects_table`, `events_table` — storage names
- `usage_plan_id`, `api_key_id` — throttling controls

## Registration

After applying, register a project and obtain a DSN public key:

```sh
cd ../scripts/register
go run . -project my-app -region ap-northeast-2
```

See `docs/REGISTRATION.md` for the full DSN format handed to clients.
