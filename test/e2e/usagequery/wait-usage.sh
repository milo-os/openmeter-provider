#!/bin/bash
# Wait until the ingested usage is queryable through the usage API: the
# ingest pipeline (Kafka -> sink-worker -> ClickHouse) can lag by minutes on
# a small cluster. Uses the platform scope, which can read both
# organizations.
set -euo pipefail
. ./lib.sh

query=$(spec '.billingAccounts = [
  {namespace: "organization-e2e-uq-a", name: "e2e-uq-a-main"},
  {namespace: "organization-e2e-uq-b", name: "e2e-uq-b-main"}
]')

check() {
  body=$(usage_query "" "" "$query") || return 1
  num_eq "$(total "$body" "$CPU_METER")" 122 && num_eq "$(total "$body" "$PEAK_METER")" 999
}
retry 120 check || fail "usage did not reach the usage API (want ${CPU_METER}=122, ${PEAK_METER}=999; HTTP $(usage_status))" "${body:-}"
echo "OK: all usage is queryable"
