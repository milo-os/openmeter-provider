#!/bin/bash
# When the contract starts, OpenMeter invoices the setup fee and the first
# month itself, straight away: nobody asks for the first invoice.
. ./lib.sh

id=$(customer_id)

check() {
  invoices=$(om_get "/billing/invoices?customers=${id}&expand=lines" |
    jq -c '[.items[]? | select(.status != "gathering") | {id, status, total: .totals.total, lines: [.lines[]? | {name, total: .totals.total}]}]') || return 1
  # Ending pay as you go can leave a draft for its last hours of usage,
  # often at zero; the contract's charges come on an invoice of their own.
  contract=$(echo "$invoices" | jq -c '[.[] | select(any(.lines[]; .name == "Server installation" or .name == "Dedicated server"))]')
  [ "$(echo "$contract" | jq -r 'length == 1 and (.[0].lines | map(.name) | sort) == ["Dedicated server", "Server installation"] and (.[0].lines[] | select(.name == "Server installation") | .total | tonumber) == 1500')" = "true" ]
}

if retry 45 check; then
  echo "OK: OpenMeter invoiced the setup fee and first month itself: $(echo "$contract" | jq -c '.[0]')"
  echo "    every invoice: $(echo "$invoices" | jq -c '[.[] | {status, total}]')"
  exit 0
fi
fail "OpenMeter didn't invoice the contract's first charges on its own" "${invoices:-[]}"
