#!/bin/bash
# The account has one live subscription, on the pay-as-you-go plan, and no
# invoice: a usage plan bills in arrears.
. ./lib.sh

customer=$(kubectl get billingaccount "$ACCOUNT" -n "$NS" -o jsonpath='{.metadata.uid}')
payg=$(plan_key "$(kubectl get offer e2e-ir-payg -o jsonpath='{.metadata.uid}')")

check() {
  subs=$(om_get "/customers/${customer}/subscriptions" | jq -c '[.items // [] | .[] | select(.status != "inactive")]') || return 1
  [ "$(echo "$subs" | jq -r --arg k "$payg" 'length == 1 and .[0].status == "active" and .[0].plan.key == $k')" = "true" ]
}

if ! retry 30 check; then
  fail "no single active subscription on the pay-as-you-go plan" \
    "$(echo "${subs:-[]}" | jq -c '[.[] | {id, status, plan: .plan.key, activeFrom, activeTo}]')"
fi
invoices=$(om_get "/billing/invoices?customers=$(customer_id)" | jq '[.items[]? | select(.status != "gathering")] | length')
[ "$invoices" -eq 0 ] || fail "pay as you go was invoiced up front ($invoices invoice(s))"
echo "OK: on pay as you go: $(echo "$subs" | jq -c '.[0] | {id, plan: .plan.key, billingAnchor}')"
