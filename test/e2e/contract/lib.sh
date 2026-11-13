#!/bin/bash
# Configuration and helpers of the contract suite. The OpenMeter and retry
# helpers are the invoice suites' (../invoice/common.sh); this adds the v3
# API, which is the only one that reads a subscription with a unit-converting
# item, and what the contract should bill (schedule.py).

ACCOUNT=e2e-ct-account
ENTITLEMENT=e2e-ct-entitlement
QUERY_POD=openmeter-query-ct

# This directory, so the time-travel suite (../../timetravel) can source
# this file from its own.
CONTRACT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

. "${CONTRACT_DIR}/../invoice/common.sh"

OM3_API="http://openmeter-api.openmeter-system.svc.cluster.local/api/v3/openmeter"

# om3 METHOD PATH [BODY] — om against the v3 API.
om3() {
  OM_API=$OM3_API om "$@"
}

# starts_at — the contract's start, as RFC 3339 in UTC.
starts_at() {
  kubectl get billingentitlement "$ENTITLEMENT" -n "$NS" -o jsonpath='{.spec.contract.startsAt}' |
    sed -E 's/\.[0-9]+Z$/Z/'
}

# subscription_id — the entitlement's OpenMeter subscription, from its
# OpenMeterSubscriptionSynced condition ("OpenMeter subscription <id> on
# plan ...").
subscription_id() {
  kubectl get billingentitlement "$ENTITLEMENT" -n "$NS" -o json |
    jq -r '.status.conditions // [] | map(select(.type == "OpenMeterSubscriptionSynced")) | .[0].message // ""' |
    sed -nE 's/^OpenMeter subscription ([^ ]+) on plan.*/\1/p'
}

# subscription — the entitlement's subscription, read through v3.
subscription() {
  local id
  id=$(subscription_id)
  [ -n "$id" ] || return 1
  om3 GET "/subscriptions/${id}"
}

# expected OFFER COMMAND [ARGS...] — schedule.py's answer for OFFER from the
# contract's start.
expected() {
  local offer=$1
  shift
  kubectl get offer "$offer" -o json | python3 "${CONTRACT_DIR}/schedule.py" "$1" "$(starts_at)" "${@:2}"
}

# usage_names OFFER — the invoice line names of OFFER's usage pricings, as a
# JSON array. OpenMeter's v1 API types every line usage_based, flat fees
# included, so usage lines are told apart by name.
usage_names() {
  kubectl get offer "$1" -o json |
    jq -c '[.spec.servicePricings[] | select(.spec.chargeType == "Usage") | .spec.displayName // .name]'
}

# trim_time — drop fractional seconds from RFC 3339 times in JSON on stdin,
# so times compare as strings.
trim_time() {
  sed -E 's/("[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})\.[0-9]+Z"/\1Z"/g'
}
