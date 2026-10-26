#!/bin/bash
set -euo pipefail
. ./lib.sh

id=$(customer_id)
[ -n "$id" ] || fail "OpenMeter customer for ${ACCOUNT} not found"

sleep 2
resp=$(om POST "/billing/invoices/invoice" "{\"customerId\":\"${id}\"}") ||
  fail "invoicing pending lines failed (HTTP $(om_status))" "$resp"
echo "$resp" | jq -c '[.[] | {id, status, total: .totals.total}]'

invoice=$(the_invoice) || fail "expected exactly one invoice for the customer" "$(account_invoices)"
echo "OK: invoice $(echo "$invoice" | jq -r .id) created ($(echo "$invoice" | jq -r .status))"
