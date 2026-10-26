#!/bin/bash
set -euo pipefail
. ./lib.sh

id=$(customer_id)
[ -n "$id" ] || fail "OpenMeter customer for ${ACCOUNT} not found"

for line in "${LINES[@]}"; do
  IFS='|' read -r _ slug name want _ _ _ _ <<<"$line"
  check() {
    body=$(om GET "/meters/${slug}/query?filterCustomerId=${id}") || return 1
    got=$(echo "$body" | jq -r '[.data[]?.value] | add // 0')
    num_eq "$got" "$want"
  }
  retry 120 check || fail "${name}: usage did not reach ${want} on ${slug} (last value ${got:-none})" "${body:-}"
  echo "OK: ${name}: ${want} units recorded on ${slug}"
done
