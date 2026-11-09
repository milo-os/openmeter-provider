#!/bin/bash
# Later months need nobody: once the first invoice is out, OpenMeter already
# holds the next month's fee, starting when the current billing period ends,
# waiting to be invoiced in advance. The setup fee isn't among it.
. ./lib.sh

check() {
  sub=$(live_subscription) || return 1
  period=$(billing_period "$(echo "$sub" | jq -r '.id')") || return 1
  lines=$(invoice_lines) || return 1
  [ "$(echo "$lines" | jq -r --arg next "$(echo "$period" | jq -r '.to[0:10]')" '
    (map(select(.status == "gathering" and .name == "Dedicated server" and .from[0:10] == $next and (.total | tonumber) == 1000)) | length) == 1
    and (map(select(.status == "gathering" and .name == "Server installation")) | length) == 0')" = "true" ]
}

if retry 30 check; then
  echo "OK: the next month's fee waits from $(echo "$period" | jq -r '.to'): $(echo "$lines" | jq -c 'map(select(.status == "gathering"))')"
  exit 0
fi
fail "OpenMeter doesn't hold next month's fee from the end of the billing period" \
  "$(jq -n --argjson period "${period:-null}" --argjson lines "${lines:-null}" '{period: $period, lines: $lines}')"
