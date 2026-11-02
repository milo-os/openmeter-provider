#!/bin/bash
# An Issue invoices exactly the previewed charges, and names the Invoice.
. ./lib.sh

request e2e-ir-issue Issue >/dev/null
st=$(wait_done e2e-ir-issue)
preview=$(kubectl get invoicerequest e2e-ir-preview -n "$NS" -o json | jq -c '.status')

ok=$(jq -nr --argjson st "$st" --argjson p "$preview" '
  $st.phase == "Succeeded" and ($st.invoiceRef.name // "" | startswith("e2e-ir-account-"))
  and ($st.total | tonumber) == ($p.total | tonumber)
  and ($st.lines | map(.description) | sort) == ($p.lines | map(.description) | sort)')
[ "$ok" = "true" ] || fail "the issued invoice doesn't match the preview" "$(jq -nc --argjson st "$st" --argjson p "$preview" '{issued: $st, preview: $p}')"

id=$(customer_id)
invoices=$(om_get "/billing/invoices?customers=${id}&expand=lines" |
  jq -c '[.items[]? | select(.status != "gathering") | {id, status, total: .totals.total, lines: [.lines[]? | .name] | sort}]')
[ "$(echo "$invoices" | jq -r 'length == 1 and .[0].lines == ["Dedicated server", "Server installation"]')" = "true" ] ||
  fail "OpenMeter doesn't have one invoice with the two fixed charges" "$invoices"
echo "OK: issued $(echo "$invoices" | jq -c '.[0]') as $(echo "$st" | jq -r '.invoiceRef.name')"
