# Query API

> Deployment status: dormant source only. The production OpenTofu stack does
> not create this Lambda or any GET route. A separate operator-authentication
> design is required before deployment.

Read-only endpoint listing event metadata ingested by the service. Raw
payload content is **not** returned — only metadata summaries (plus the S3
path where the raw archive lives).

## Endpoint

```
GET /api/projects/{project_id}/events
```

Protected with the same key validation as ingestion: the request must carry
the project's DSN public key either in the `X-Sentry-Auth` header or the
`sentry_key` query parameter. Missing auth → `403`; unknown key → `401`.

## Query parameters

| Param | Description |
|---|---|
| `sentry_key` | DSN public key (query form of auth) |
| `limit` | Max events per page (default 50, max 200) |
| `cursor` | Opaque `next_cursor` from the previous page (pass through unchanged) |
| `from` / `to` | Optional timestamp range (RFC3339). Uses the `TimestampIndex` GSI. |
| `level` | Optional filter (e.g. `error`). Narrows the item-type filter. |
| `type` | Item type to list. **Default `event`** — only event metadata rows are returned unless widened. Use `type=all` to include `session`/`transaction`/`attachment`/etc. rows, or `type=session` for a specific type. |

## Response

```json
{
  "events": [
    {
      "project_id": "my-app",
      "event_id": "e1",
      "timestamp": "2026-08-06T01:00:00Z",
      "level": "error",
      "platform": "javascript",
      "item_type": "event",
      "count": 1,
      "status": "received",
      "s3_path": "projects/my-app/2026/08/06/e1/items/0-event"
    }
  ],
  "next_cursor": "eyJrIjoiZT..." 
}
```

- `next_cursor` is an opaque, base64-encoded DynamoDB `LastEvaluatedKey`.
  Pass it back as the `cursor` parameter to fetch the next page; when it is
  empty the result set is exhausted.
- `item_type` distinguishes metadata rows: `event`, `session`, `transaction`,
  `attachment`, ... (one row per envelope item).
- To read raw content, fetch the object at `s3_path` from the raw bucket
  (the bucket is private; use AWS credentials or a presigned URL).
