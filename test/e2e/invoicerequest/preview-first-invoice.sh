#!/bin/bash
# Before the contract starts, a Preview works out its first invoice from the
# Offer: the setup fee and the first month from the start date. Nothing is
# subscribed or invoiced yet.
. ./lib.sh

tomorrow=$(date -u -d '1 day' +%Y-%m-%d 2>/dev/null || date -u -v+1d +%Y-%m-%d)
request e2e-ir-preview-first Preview >/dev/null
st=$(wait_done e2e-ir-preview-first)

ok=$(echo "$st" | jq -r --arg d "$tomorrow" '
  .phase == "Succeeded" and .currencyCode == "USD" and (.lines | length) == 2
  and (.lines | map(select(.description == "Server installation" and (.amount | tonumber) == 1500)) | length) == 1
  and (.lines | map(select(.description == "Dedicated server" and (.amount | tonumber) == 1000 and .period.start[0:10] == $d)) | length) == 1
  and (.total | tonumber) == 2500
  and ((.conditions[] | select(.type == "Completed") | .message) | contains("contract starts on " + $d))')
[ "$ok" = "true" ] || fail "unexpected first-invoice preview" "$st"

customer=$(kubectl get billingaccount "$ACCOUNT" -n "$NS" -o jsonpath='{.metadata.uid}')
live=$(om_get "/customers/${customer}/subscriptions" | jq '[.items[]? | select(.status != "inactive")] | length')
[ "$live" -eq 0 ] || fail "the contract was subscribed before it starts ($live live subscriptions)"
invoices=$(om_get "/billing/invoices?customers=$(customer_id)" | jq '[.items[]? | select(.status != "gathering")] | length')
[ "$invoices" -eq 0 ] || fail "a preview left $invoices invoice(s)"
echo "OK: first invoice previewed: $(echo "$st" | jq -c '{total, lines: [.lines[] | {description, amount}]}')"
