#!/bin/bash
# Wait until OpenMeter reports every line's units for the account's customer
# on its meter. Invoicing before all of it is queryable would bill only part
# of it.
set -euo pipefail
. ./lib.sh

id=$(customer_id)
[ -n "$id" ] || fail "OpenMeter customer for ${ACCOUNT} not found"

for line in "${LINES[@]}"; do
  IFS='|' read -r _ slug name want _ <<<"$line"
  check() {
    body=$(om GET "/meters/${slug}/query?filterCustomerId=${id}") || return 1
    got=$(echo "$body" | jq -r '[.data[]?.value] | add // 0')
    num_eq "$got" "$want"
  }
  # The ingest pipeline (Kafka -> sink-worker -> ClickHouse) can lag by
  # minutes on a small cluster; see ../submission/verify-usage.sh.
  retry 120 check || fail "${name}: usage did not reach ${want} on ${slug} (last value ${got:-none})" "${body:-}"
  echo "OK: ${name}: ${want} units recorded on ${slug}"
done
