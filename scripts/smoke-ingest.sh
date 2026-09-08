#!/usr/bin/env bash
set -euo pipefail

: "${PUBLIC_BASE_URL:?PUBLIC_BASE_URL is required}"
: "${RAW_BUCKET:?RAW_BUCKET is required}"
: "${EVENTS_TABLE:?EVENTS_TABLE is required}"
: "${SMOKE_PROJECT_ID:?SMOKE_PROJECT_ID is required}"
: "${SMOKE_PUBLIC_KEY:?SMOKE_PUBLIC_KEY is required}"

if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
  printf '::add-mask::%s\n' "$SMOKE_PUBLIC_KEY"
fi

event_id=$(openssl rand -hex 16)
day=$(date -u +%Y-%m-%d)
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

printf '{"event_id":"%s"}\n{"type":"event"}\n{"event_id":"%s","timestamp":"%sT00:00:00Z","level":"error","platform":"javascript"}' \
  "$event_id" "$event_id" "$day" > "$tmpdir/envelope"

status=$(curl --silent --show-error --output "$tmpdir/response" --write-out '%{http_code}' \
  --request POST --data-binary "@$tmpdir/envelope" \
  --header 'Content-Type: application/x-sentry-envelope' \
  --header "X-Sentry-Auth: Sentry sentry_version=7, sentry_key=$SMOKE_PUBLIC_KEY" \
  "${PUBLIC_BASE_URL%/}/api/${SMOKE_PROJECT_ID}/envelope/")
test "$status" = "200"
python3 -c 'import json,sys; expected=sys.argv[2]; assert json.load(open(sys.argv[1]))["id"] == expected' "$tmpdir/response" "$event_id"

s3_key="projects/$SMOKE_PROJECT_ID/$day/$event_id.envelope"
for _ in {1..12}; do
  if aws s3api head-object --bucket "$RAW_BUCKET" --key "$s3_key" >/dev/null 2>&1; then
    break
  fi
  sleep 5
done
aws s3api head-object --bucket "$RAW_BUCKET" --key "$s3_key" >/dev/null

found=false
for _ in {1..12}; do
  count=$(aws dynamodb query --table-name "$EVENTS_TABLE" \
    --key-condition-expression 'project_id = :p AND begins_with(event_id, :e)' \
    --expression-attribute-values "{\":p\":{\"S\":\"$SMOKE_PROJECT_ID\"},\":e\":{\"S\":\"$event_id\"}}" \
    --select COUNT --query Count --output text)
  if [[ "$count" -ge 1 ]]; then
    found=true
    break
  fi
  sleep 5
done
test "$found" = true
printf 'POST persistence smoke passed for event %s\n' "$event_id"
