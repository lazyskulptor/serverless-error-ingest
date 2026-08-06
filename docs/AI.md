# AI Grouping / Summarization (optional MVP)

Post-ingest pipeline that batches new events, groups similar crashes into one
issue, and writes a short summary back to the DynamoDB metadata. Grouping is
**deterministic and offline**; AI summarization is an opt-in enhancement.

## How it works

1. **Trigger**: EventBridge schedule (default `rate(5 minutes)`) invokes the
   `sentry-ingest-processor` Lambda.
2. **Scan**: rows in `sentry-ingest-events` with `item_type = "event"` and
   `status = "received"`.
3. **Load**: the raw event payload is fetched from S3 (`s3_path`).
4. **Fingerprint**: `computeIssueID` hashes the normalized exception
   fingerprint — platform + first exception type/value + top stacktrace frame
   (file/function/line). Identical crashes → same `issue_id`; distinct crashes
   → different `issue_id`. Message-only events are fingerprinted on the
   message. This is deterministic and requires no AI.
5. **Summary**: `Type: value` (or the message) by default. When AI is enabled,
   only the **extracted fingerprint summary** (exception type, value, first
   frames — never the raw payload) is sent to an OpenAI-compatible endpoint.
6. **Update**: the row is updated with `issue_id`, `summary`, and
   `status = "grouped"` (guarded by `attribute_not_exists(issue_id)` so a row
   is processed once).

## Data-leak policy

- The AI model only ever receives the extracted fingerprint (exception type,
  value, up to 5 frame locations) — **not** the raw event payload.
- AI is fully opt-in: without configuration the processor runs deterministic
  grouping/summarization only.
- The AI key is read from SSM at deploy time and injected as a Lambda
  environment variable. It is never stored in source control or tfvars.

## Configuration (infra variables)

| Variable | Default | Purpose |
|---|---|---|
| `processor_schedule` | `rate(5 minutes)` | EventBridge schedule expression |
| `ai_enabled` | `false` | Master switch for AI summarization |
| `ai_api_key_ssm_path` | `""` | SSM parameter holding the AI key (e.g. `/sentry-ingest/ai-key`) |
| `ai_endpoint` | `""` | OpenAI-compatible chat completions URL |
| `ai_model` | `""` | Model name (defaults to `gpt-4o-mini` in the handler) |

```sh
# store the key in SSM (parameter must be plaintext; Lambda reads it at deploy)
aws ssm put-parameter \
  --name /sentry-ingest/ai-key \
  --value "sk-..." \
  --type SecureString

# apply with AI enabled
tofu apply \
  -var ai_enabled=true \
  -var ai_api_key_ssm_path=/sentry-ingest/ai-key \
  -var ai_endpoint=https://api.openai.com/v1/chat/completions \
  -var ai_model=gpt-4o-mini
```

## Verification

Deploy, then send two fixture events with identical exception type/value/frames:

1. Both events persist via the ingest endpoint.
2. Wait for the next scheduled run (or trigger the processor Lambda manually).
3. Query the events table: both rows now share the same `issue_id`,
   `status = "grouped"`, and a non-empty `summary`.
4. A third event with a different exception must get a different `issue_id`.
