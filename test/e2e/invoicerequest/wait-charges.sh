#!/bin/bash
# Waits for OpenMeter's subscription sync to put the setup fee and the first
# month on the customer's gathering invoice, both billable now, so the
# requests below see them.
. ./lib.sh

id=$(customer_id)
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)

check() {
  lines=$(om_get "/billing/invoices?customers=${id}&statuses=gathering&expand=lines" |
    jq -c --arg now "$now" '[.items[]?.lines[]? | select(.deletedAt == null and .invoiceAt <= $now and .rateCard.price.type == "flat") | {name, invoiceAt, amount: .rateCard.price.amount}]') || return 1
  [ "$(echo "$lines" | jq 'length')" -eq 2 ]
}

if retry 45 check; then
  echo "OK: setup fee and first month waiting: $lines"
  exit 0
fi
fail "the contract's fixed charges never reached the gathering invoice" "${lines:-[]}"
