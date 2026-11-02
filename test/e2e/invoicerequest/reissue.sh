#!/bin/bash
# The charges were invoiced, so a second Issue finds nothing to invoice and
# creates no invoice.
. ./lib.sh

request e2e-ir-reissue Issue >/dev/null
st=$(wait_done e2e-ir-reissue)
[ "$(echo "$st" | jq -r '.phase == "Failed" and (.conditions[] | select(.type == "Completed") | .reason) == "NothingToInvoice"')" = "true" ] ||
  fail "a second Issue didn't report NothingToInvoice" "$st"

count=$(om_get "/billing/invoices?customers=$(customer_id)" | jq '[.items[]? | select(.status != "gathering")] | length')
[ "$count" -eq 1 ] || fail "a second Issue left $count invoices"
echo "OK: nothing left to invoice"
