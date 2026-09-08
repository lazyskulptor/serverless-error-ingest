#!/usr/bin/env bash
set -euo pipefail

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

command -v go >/dev/null || { echo "go is required" >&2; exit 1; }
command -v zip >/dev/null || { echo "zip is required" >&2; exit 1; }

for module in ingest; do
  dir="$ROOT/lambda/$module"
  test -f "$dir/go.mod" || { echo "missing $dir/go.mod" >&2; exit 1; }
  (
    cd "$dir"
    rm -f bootstrap function.zip
    GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-buildid=" -o bootstrap .
    TZ=UTC touch -t 198001010000 bootstrap
    zip -q -X function.zip bootstrap
    rm -f bootstrap
  )
done
