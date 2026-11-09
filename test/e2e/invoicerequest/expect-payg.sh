#!/bin/bash
# The account has one live subscription, on the pay-as-you-go plan, and
# nothing to pay: a usage plan bills in arrears, so it charges nothing up
# front. An invoice with nothing on it is allowed: when restart-today.sh
# deletes the entitlement, its subscription is canceled and OpenMeter
# invoices the last period's usage, which is none.
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
charged=$(om_get "/billing/invoices?customers=$(customer_id)" |
  jq -c '[.items[]? | select(.status != "gathering" and ((.totals.total // "0") | tonumber) != 0) | {id, status, total: .totals.total}]')
[ "$(echo "$charged" | jq length)" -eq 0 ] || fail "pay as you go was invoiced up front" "$charged"
echo "OK: on pay as you go: $(echo "$subs" | jq -c '.[0] | {id, plan: .plan.key, billingAnchor}')"
