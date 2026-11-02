#!/bin/bash
# Waits for OpenMeter's subscription sync to put the setup fee and the first
# month on the customer's gathering invoice, both billable now, so the
# requests below see them. Both are billable from the subscription's start,
# so "now" is read on every poll: the subscription may start after this
# script does.
. ./lib.sh

id=$(customer_id)

check() {
  local now
  now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  raw=$(om_get "/billing/invoices?customers=${id}&statuses=gathering&expand=lines" |
    jq -c '[.items[]? | {status, lines: [.lines[]? | {id, name, invoiceAt, deletedAt, price: .rateCard.price, total: .totals.total}]}]') || return 1
  lines=$(echo "$raw" | jq -c --arg now "$now" '[.[].lines[] | select(.deletedAt == null and .invoiceAt <= $now and .price.type == "flat")]')
  [ "$(echo "$lines" | jq 'length')" -eq 2 ]
}

if retry 45 check; then
  echo "OK: setup fee and first month waiting: $lines"
  exit 0
fi
fail "the contract's fixed charges never reached the gathering invoice" "${raw:-[]}"
