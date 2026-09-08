# Project Registration / DSN Key Issuance

Before any ingestion can be tested, a project must exist in the `projects`
DynamoDB table with a minted DSN public key. Registration is a small Go CLI
(`scripts/register`) that writes directly to the table using your AWS
credentials — no admin Lambda route is needed.

## Table schema

`projects` table (partition key `project_id`):

| Attribute | Type | Notes |
|---|---|---|
| `project_id` | S | partition key |
| `public_key` | S | DSN public key (32 hex chars) |
| `secret_key` | S | optional, legacy/deprecated — only set with `-secret` |
| `created_at` | S | RFC3339 timestamp |
| `status` | S | `active` or `disabled` |

## Usage

```sh
cd scripts/register
go run . -project my-app -host <api-gateway-host>
```

Flags:

- `-project` (required) — project id, e.g. `my-app`
- `-host` (required) — host used to render the DSN. Use the hostname from
  `tofu output -raw public_base_url`; this may be API Gateway, Route 53, or
  Cloudflare-managed DNS.
- `-region` (default `ap-northeast-2`)
- `-table` (default `sentry-ingest-projects`)
- `-secret` — also mint a legacy secret key (optional, deprecated)

Requires AWS credentials with `dynamodb:PutItem` on the projects table (the
default CLI profile works if it has access).

## DSN handed to clients

The script prints a DSN in Sentry's exact format
(`docs/COMPATIBILITY.md` §1):

```
DSN = '{PROTOCOL}://{PUBLIC_KEY}@{HOST}/{PROJECT_ID}'
```

For the example above:

```
https://<public-key>@abc123.execute-api.ap-northeast-2.amazonaws.com/my-app
```

A Sentry SDK initialized with this DSN (plus a stage path if deployed under
one) sends events to the envelope endpoint
`/api/{project_id}/envelope/` with **no other configuration change**.

Custom domains map at the API root and do not include the API Gateway stage in
their DSN path. Direct invoke URLs retain the configured stage path.
