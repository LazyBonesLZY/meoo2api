#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [ -n "$(gofmt -l cmd internal)" ]; then
  echo 'Go formatting differs; run gofmt on the reported files.' >&2
  gofmt -l cmd internal
  exit 1
fi
go vet ./...
go test -race -count=1 -timeout=90s ./...
if command -v node >/dev/null; then
  node --check internal/webui/console.js
  node --test internal/webui/console.test.cjs
fi
./scripts/build.sh
