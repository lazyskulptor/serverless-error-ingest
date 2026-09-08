# Infrastructure

OpenTofu deploys API Gateway, Lambda, WAF, private S3 storage, DynamoDB, access
logs, and alarms.

## Local deployment

Requirements: Go 1.24+, `zip`, OpenTofu 1.6+, AWS CLI, and AWS credentials.

```sh
./scripts/build-lambdas.sh
cd infra
cp terraform.tfvars.example terraform.tfvars
# Set a globally unique raw_bucket_name. Production must also set
# allow_destroy_data=false before the first apply.
tofu init
tofu plan
tofu apply
```

Outputs include the direct `api_gateway_url`, effective `public_base_url`,
storage names, and ingest endpoints. Register a project afterward:

```sh
cd ../scripts/register
go run . -project my-app -host <api-host>
```

## DNS

Leave `domain_name` empty for the direct API URL. For Route 53 set
`dns_provider = "aws"` and `route53_zone_id`. For Cloudflare set
`dns_provider = "cloudflare"`, `cloudflare_zone_id`, and export
`CLOUDFLARE_API_TOKEN`. Start with `cloudflare_proxied = false`.

The API Gateway custom-domain target is only a CNAME target. Test without DNS
using `api_gateway_url`, including its stage path.

## State

Use a versioned S3 bucket and a DynamoDB lock table created outside this stack:

```sh
tofu init -reconfigure \
  -backend-config="bucket=<state-bucket>" \
  -backend-config="key=<environment>/terraform.tfstate" \
  -backend-config="region=<region>" \
  -backend-config="dynamodb_table=<lock-table>" \
  -backend-config="encrypt=true"
```

CI uses `tofu init -backend=false`.

## GitHub Release deployment

Bootstrap OIDC from `infra/bootstrap/github-oidc`, then create a protected
GitHub environment (default: `production`). Never store AWS access keys in
GitHub.

Required variables:

- `AWS_REGION`, `AWS_ACCOUNT_ID`, `AWS_APPLY_ROLE_ARN`
- `TF_STATE_BUCKET`, `TF_STATE_KEY`, `TF_STATE_LOCK_TABLE`
- `RAW_BUCKET_NAME`

Optional variables mirror `variables.tf`: `DEPLOY_ENVIRONMENT`, `NAME_PREFIX`,
`API_STAGE`, DNS values, retention values, WAF limit, alarm topic, and
`ALLOW_DESTROY_DATA`.
Cloudflare needs `CLOUDFLARE_API_TOKEN`.

`ALLOW_DESTROY_DATA` defaults to true through OpenTofu. **Every production
GitHub environment must set it to `false` before its first deployment.**

CI validates source and uploads `lambda-<commit-sha>` for 30 days. A published
Release deploys only the first successful push CI artifact for its exact SHA,
verifies its checksum, applies a saved plan, then checks POST, S3, and DynamoDB.

First deployment only: manually dispatch a published tag with
`skip_smoke=true`, register a smoke project, and add `SMOKE_PROJECT_ID` and
`SMOKE_PUBLIC_KEY`. Normal Releases must run smoke.

## Operations

- Raw objects move to Standard-IA after 30 days and expire after 180 by default.
- Event metadata expires after 90 days by default.
- WAF limits source IPs; Lambda also limits DSN keys.
- Alarms cover API/Lambda errors, throttles, request volume, and storage growth.
  Set `alarm_sns_topic_arn` for notifications and configure AWS Budgets
  separately.
- New keys use `projects/<project>/YYYY-MM-DD/<event>.envelope`; older
  `YYYY/MM/DD` keys may remain until expiry.

## Recovery

After a partial apply, compare `tofu state list` with AWS and import existing
resources instead of deleting data:

```sh
tofu import aws_dynamodb_table.projects <table>
tofu import aws_dynamodb_table.events <table>
tofu import aws_iam_role.ingest <role>
```

API Gateway account logging is account/region-wide and must have one owner.
Always review the next plan. Never delete the raw bucket or event tables as a
generic fix.

## Destroy

The development default, `allow_destroy_data = true`, makes `tofu destroy`
delete versioned S3 objects and both DynamoDB tables with the remaining runtime
resources. The separately bootstrapped GitHub OIDC provider and apply role are
not part of this state and require a separate destroy if they are not shared.

**Production must set `allow_destroy_data = false` before its first apply.** A
later configuration change cannot restore data after destroy has started.

Rollback by reverting through review and publishing a new Release. During DNS
incidents use `api_gateway_url`; do not rerun project registration. In
production, keep `allow_destroy_data = false` during recovery and rollback.
