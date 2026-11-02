#!/bin/bash
# When the contract starts, OpenMeter invoices the setup fee and the first
# month itself, straight away: nobody asks for the first invoice.
. ./lib.sh

id=$(customer_id)

check() {
  invoices=$(om_get "/billing/invoices?customers=${id}&expand=lines" |
    jq -c '[.items[]? | select(.status != "gathering") | {id, status, total: .totals.total, lines: [.lines[]? | {name, total: .totals.total}]}]') || return 1
  [ "$(echo "$invoices" | jq -r 'length == 1 and (.[0].lines | map(.name) | sort) == ["Dedicated server", "Server installation"] and (.[0].lines[] | select(.name == "Server installation") | .total | tonumber) == 1500')" = "true" ]
}

if retry 45 check; then
  echo "OK: OpenMeter invoiced the setup fee and first month itself: $(echo "$invoices" | jq -c '.[0]')"
  exit 0
fi
fail "OpenMeter didn't invoice the contract's first charges on its own" "${invoices:-[]}"
