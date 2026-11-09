#!/bin/bash
# The account moved off pay as you go: its one live subscription is on the
# contract's plan and bills on the contract's start day (today), not the
# 1st. The pay-as-you-go subscription has ended.
. ./lib.sh

today=$(date -u +%Y-%m-%d)
customer=$(kubectl get billingaccount "$ACCOUNT" -n "$NS" -o jsonpath='{.metadata.uid}')
contract=$(plan_key "$(kubectl get offer e2e-ir-offer -o jsonpath='{.metadata.uid}')")
payg=$(plan_key "$(kubectl get offer e2e-ir-payg -o jsonpath='{.metadata.uid}')")

check() {
  all=$(om_get "/customers/${customer}/subscriptions" | jq -c '.items // []') || return 1
  subs=$(echo "$all" | jq -c '[.[] | select(.status != "inactive")]')
  [ "$(echo "$subs" | jq -r --arg d "$today" --arg k "$contract" \
    'length == 1 and .[0].status == "active" and .[0].plan.key == $k and .[0].billingAnchor[0:10] == $d')" = "true" ] || return 1
  [ "$(echo "$all" | jq -r --arg k "$payg" 'map(select(.plan.key == $k)) | length >= 1 and all(.status == "inactive")')" = "true" ]
}

if retry 30 check; then
  echo "OK: moved off pay as you go onto one active contract subscription billing on the contract's day ($today)"
  exit 0
fi
fail "the account isn't on a single active contract subscription anchored on $today, with pay as you go ended" \
  "$(echo "${all:-[]}" | jq -c '[.[] | {id, status, plan: .plan.key, activeFrom, activeTo, billingAnchor}]')"
