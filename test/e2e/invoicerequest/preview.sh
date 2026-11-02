#!/bin/bash
# A Preview prices the setup fee and first month, and creates nothing.
. ./lib.sh

request e2e-ir-preview Preview >/dev/null
st=$(wait_done e2e-ir-preview)

ok=$(echo "$st" | jq -r '
  .phase == "Succeeded" and .currencyCode == "USD" and (.lines | length) == 2 and .invoiceRef == null
  and (.lines | map(select(.description == "Server installation" and (.amount | tonumber) == 1500)) | length) == 1
  and (.lines | map(select(.description == "Dedicated server" and (.amount | tonumber) > 0 and (.amount | tonumber) <= 1000 and .period != null)) | length) == 1
  and (.total | tonumber) == (.lines | map(.amount | tonumber) | add)')
[ "$ok" = "true" ] || fail "unexpected preview" "$st"

invoices=$(om_get "/billing/invoices?customers=$(customer_id)" | jq '[.items[]? | select(.status != "gathering")] | length')
[ "$invoices" -eq 0 ] || fail "a preview created $invoices invoice(s)"
echo "OK: previewed $(echo "$st" | jq -c '{total, lines: [.lines[] | {description, amount}]}')"
