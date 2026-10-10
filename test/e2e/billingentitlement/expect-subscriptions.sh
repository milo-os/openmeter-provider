#!/bin/bash
# expect-subscriptions.sh JQ_FILTER DESCRIPTION
#
# Polls the OpenMeter subscriptions of e2e-be-account's customer until
# JQ_FILTER evaluates to true. The filter runs over the customer's
# subscriptions (newest first, ended ones included) with these variables:
#
#   $a, $b  plan keys of e2e-be-offer-a and e2e-be-offer-b
#   $be     metadata value identifying e2e-be-entitlement
#
# Helper functions available to the filter:
#   live         subscriptions that are not inactive
#   on($k; $v)   select subscriptions on plan $k at version $v
set -euo pipefail
QUERY_POD=openmeter-query-be
# shellcheck source=../offer/lib.sh
. ../offer/lib.sh

filter="$1"
description="$2"
ns=openmeter-system

customer=$(kubectl get billingaccount e2e-be-account -n "$ns" -o jsonpath='{.metadata.uid}')
a=$(plan_key "$(kubectl get offer e2e-be-offer-a -o jsonpath='{.metadata.uid}')")
b=$(plan_key "$(kubectl get offer e2e-be-offer-b -o jsonpath='{.metadata.uid}')")
be="${ns}/e2e-be-entitlement"

prelude='def live: map(select(.status != "inactive")); def on($k; $v): map(select(.plan.key == $k and .plan.version == $v));'

check() {
  subs=$(om_get "/customers/${customer}/subscriptions" | jq -c '.items // []') || return 1
  [ "$(echo "$subs" | jq -r --arg a "$a" --arg b "$b" --arg be "$be" "${prelude} ${filter}")" = "true" ]
}

if retry 30 check; then
  echo "OK: ${description}"
  exit 0
fi
fail "${description} (filter: ${filter})" \
  "$(echo "${subs:-[]}" | jq -c '[.[] | {id, status, plan: "\(.plan.key)@\(.plan.version)", activeFrom, activeTo, metadata}]')"
