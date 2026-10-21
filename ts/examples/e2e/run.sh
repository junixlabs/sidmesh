#!/usr/bin/env bash
# End-to-end check: the Go interpreter routes a call step to the TS worker.
# Needs a Temporal dev server on 127.0.0.1:7233 and Go binaries built from ../go:
#   (cd ../go && go build -o /tmp/sidmesh-a10/ ./cmd/sidmesh-run ./examples/demo/worker)
# Run from ts/:  pnpm build && examples/e2e/run.sh
set -euo pipefail
BIN="${SIDMESH_BIN:-/tmp/sidmesh-a10}"
PROJECT="$(cd "$(dirname "$0")" && pwd)"
LOG="$(mktemp -d)"
"$BIN/worker" -project "$PROJECT" -records "$LOG/records.jsonl" >"$LOG/go.log" 2>&1 &
GO_PID=$!
node "$PROJECT/worker.ts" >"$LOG/ts.log" 2>&1 &
TS_PID=$!
trap 'kill $GO_PID $TS_PID 2>/dev/null || true' EXIT
sleep 4
RUN="$(date +%s)"
"$BIN/sidmesh-run" start -project "$PROJECT" -definition hello -id "e2e-ts:ann:$RUN" -tenant demo -input '{"name":"Ann"}' -wait
"$BIN/sidmesh-run" start -project "$PROJECT" -definition hello -id "e2e-ts:mallory:$RUN" -tenant demo -input '{"name":"Mallory"}' -wait
echo "ledger: $LOG/records.jsonl"
