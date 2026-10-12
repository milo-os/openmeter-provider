#!/bin/bash
# Invoice the customer's usage now instead of at the end of the monthly
# billing period: OpenMeter's pending-lines action, with progressive billing
# (enabled on the default profile), bills the usage line up to now. This is
# the same action the BillingAccount finalizer uses before deleting a
# customer.
set -euo pipefail
. ./lib.sh

id=$(customer_id)
[ -n "$id" ] || fail "OpenMeter customer for ${ACCOUNT} not found"

# Usage is billed up to the current second; let the second the last event
# landed in close first.
sleep 2
resp=$(om POST "/billing/invoices/invoice" "{\"customerId\":\"${id}\"}") ||
  fail "invoicing pending lines failed (HTTP $(om_status))" "$resp"
echo "$resp" | jq -c '[.[] | {id, status, total: .totals.total}]'

invoice=$(the_invoice) || fail "expected exactly one invoice for the customer" "$(account_invoices)"
echo "OK: invoice $(echo "$invoice" | jq -r .id) created ($(echo "$invoice" | jq -r .status))"
