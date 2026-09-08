# Infrastructure

OpenTofu-managed AWS infrastructure for the serverless Sentry-compatible
ingest service.

## Resources

| Resource | Purpose |
|---|---|
| API Gateway (REST) | `POST /api/{project_id}/envelope/`, `POST /api/{project_id}/store/`, and `OPTIONS` CORS mock |
| Lambda `sentry-ingest-ingest` | Sentry wire-protocol ingest handler (Go) |
| S3 `raw` bucket | Private, versioned, SSE-encrypted archive of raw envelopes/events |
| DynamoDB `-projects` | DSN public-key registry (partition key `project_id`) |
| DynamoDB `-events` | Event metadata index (partition key `project_id`, sort key `event_id`, GSI `TimestampIndex` on `timestamp`) |
| WAFv2 web ACL | Rate-based rule (per source IP) in front of the stage |
| CloudWatch | Lambda, API, storage and throttle alarms; stage access logs contain metadata only |

## Cost control

- S3 lifecycle: raw objects transition to `STANDARD_IA` after
  `raw_standard_ia_days` (default 30) and expire after `raw_expiration_days`
  (default 180).
- DynamoDB TTL: `events` rows expire after `event_ttl_days` (default 90); the
  ingest handler writes `expires_at` on every row.

## Growth and abuse alarms

CloudWatch alarms are created for raw S3 object count (100,000), S3 Standard
storage (5 GiB), hourly API requests (10,000), any ingest Lambda throttle, and
any DynamoDB write throttle. The thresholds are initial small-service defaults,
not AWS universal recommendations, and can be changed in `terraform.tfvars`.
S3 storage metrics are published daily, so those two alarms are not real-time.

Set `alarm_sns_topic_arn` to an existing operations SNS topic to receive ALARM
and OK notifications. OpenTofu intentionally does not create an email
subscription or assume which account user is an administrator. With an empty
topic ARN, alarms still exist in CloudWatch but send no notification. Configure
an AWS Budget separately for cost-based notification because CloudWatch object
and request alarms do not predict the bill.

## Abuse prevention

- A 1 MiB compressed and decompressed payload cap is enforced by the ingest
  handler, including bounded gzip/deflate/br/zstd decoding.
- SDK-facing traffic is rate-limited by WAF per source IP and inside the ingest
  handler per DSN public key (returns `429` + `Retry-After`).
- Gateway-level `THROTTLED` / `QUOTA_EXCEEDED` responses are mapped to `429`
  with a `Retry-After` header per the response contract in
  `docs/COMPATIBILITY.md`.

## Building the Lambda artifacts

From the repository root, build the deployable ingest handler with the same command
used by CI:

```sh
./scripts/build-lambdas.sh
```

(`function.zip` files are git-ignored.)

## Usage

```sh
cp terraform.tfvars.example terraform.tfvars
# edit terraform.tfvars — raw_bucket_name must be globally unique; choose the
# DNS configuration below without placing credentials in this file

tofu init
tofu validate
tofu plan
tofu apply
```

## Custom domain and DNS provider

The service always runs on AWS. Set `domain_name` to enable an ACM certificate
and an API Gateway regional custom domain. Leave it empty for the direct invoke
URL.

Route 53 example:

```hcl
domain_name    = "errors.example.com"
dns_provider   = "aws"
route53_zone_id = "Z1234567890"
```

Cloudflare example:

```hcl
domain_name          = "errors.example.com"
dns_provider         = "cloudflare"
cloudflare_zone_id   = "0123456789abcdef0123456789abcdef"
cloudflare_proxied   = false
```

Export `CLOUDFLARE_API_TOKEN` with DNS Edit access to the selected zone before
running OpenTofu. DNS-only mode is the supported default; inspect and test API
Gateway behavior before enabling the Cloudflare proxy. AWS credentials use the
standard AWS provider chain.

The query source remains in the repository for future operator-authentication
work, but no query Lambda or GET route is deployed.

For existing records, import them before apply or remove them deliberately;
never allow OpenTofu to overwrite an unmanaged production record without first
reviewing the plan. Reduce TTL before migration, wait for ACM validation, then
verify `dig errors.example.com`, the TLS issuer/SAN, and a test envelope.

To move providers, keep `domain_name` unchanged, configure the destination
zone, inspect the plan to ensure Lambda/S3/DynamoDB are unchanged, and apply.
Rollback by restoring the prior DNS record or by using `api_gateway_url`, which
is always retained as a direct endpoint.

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
- `custom_domain_url` — custom URL or null
- `public_base_url` — effective URL for clients and DSNs
- `envelope_endpoint` / `store_endpoint` — ingest endpoints
- `raw_bucket`, `projects_table`, `events_table` — storage names
- WAF and ingest-handler limits provide throttling controls for stock SDK traffic

## Registration

After applying, register a project and obtain a DSN public key:

```sh
cd ../scripts/register
go run . -project my-app -region ap-northeast-2
```

See `docs/REGISTRATION.md` for the full DSN format handed to clients.

## GitHub Actions deployment

The `Deploy production` workflow uses GitHub OIDC; never configure repository
AWS access-key secrets. Bootstrap the roles once from
`infra/bootstrap/github-oidc`, then create a protected GitHub environment named
`production` with required reviewers.

Configure these repository/environment variables:

- `AWS_REGION`, `AWS_ACCOUNT_ID`
- `AWS_PLAN_ROLE_ARN`, `AWS_APPLY_ROLE_ARN`
- `TF_STATE_BUCKET`, `TF_STATE_LOCK_TABLE`, `TF_STATE_KEY`
- `RAW_BUCKET_NAME`, `EVENTS_TABLE`, `PUBLIC_BASE_URL`
- `DOMAIN_NAME`, `DNS_PROVIDER`, `ROUTE53_ZONE_ID`, `CLOUDFLARE_ZONE_ID`

For Cloudflare DNS, add protected environment secret
`CLOUDFLARE_API_TOKEN`; it is never exposed to pull requests. Add protected
secrets `SMOKE_PROJECT_ID` and `SMOKE_PUBLIC_KEY` after registering a dedicated
smoke project once. Registration is deliberately not part of deployment because
it generates a new key.

Same-repository pull requests assume the plan-only role and plan against remote
state. Fork pull requests receive only the offline CI checks. A `master` push or
manual dispatch waits for environment approval, creates a saved plan, applies
that exact artifact, and verifies POST, S3, and DynamoDB persistence.

Require the CI and deploy-plan checks in branch protection. Workflow
concurrency and the DynamoDB state lock prevent overlapping applies.

### Rollback

Revert the offending commit through a reviewed pull request and let the workflow
apply its saved rollback plan. Do not rerun project registration. During a DNS
incident, use the retained `api_gateway_url`. If apply fails, inspect the remote
state and AWS resources before retrying; never delete the raw S3 bucket or event
tables as a generic recovery step.
