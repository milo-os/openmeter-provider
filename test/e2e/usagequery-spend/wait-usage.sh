#!/bin/bash
# Wait until OpenMeter has all of the suite's usage: its ingest pipeline
# (Kafka -> sink-worker -> ClickHouse) can lag by minutes.
set -euo pipefail
. ./lib.sh
. "$TIMES_FILE"

for _ in $(seq 1 120); do
  out=$(usage_query Organization "$ORG" "$(spend_spec "$START" "$END")") || true
  requests=$(meter "$out" "$REQUESTS" | jq -r '.groupings[0].series[0].total // "0"' 2>/dev/null || echo 0)
  egress=$(meter "$out" "$EGRESS" | jq -r '.groupings[0].series[0].total // "0"' 2>/dev/null || echo 0)
  if num_eq "$requests" 12 && num_eq "$egress" 28; then
    echo "OK: requests 12, egress 28"
    exit 0
  fi
  echo "waiting: requests ${requests}/12, egress ${egress}/28"
  sleep 3
done
fail "the usage never reached OpenMeter" "$out"
