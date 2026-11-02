#!/bin/bash
# The contract's subscription is active and bills on the contract's start
# day (today), not the 1st.
. ./lib.sh

today=$(date -u +%Y-%m-%d)
customer=$(kubectl get billingaccount "$ACCOUNT" -n "$NS" -o jsonpath='{.metadata.uid}')

check() {
  subs=$(om_get "/customers/${customer}/subscriptions" | jq -c '[.items // [] | .[] | select(.status != "inactive")]') || return 1
  [ "$(echo "$subs" | jq -r --arg d "$today" 'length == 1 and .[0].status == "active" and .[0].billingAnchor[0:10] == $d')" = "true" ]
}

if retry 30 check; then
  echo "OK: one active subscription billing on the contract's day ($today)"
  exit 0
fi
fail "no single active subscription anchored on $today" \
  "$(echo "${subs:-[]}" | jq -c '[.[] | {id, status, activeFrom, billingAnchor}]')"
