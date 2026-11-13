#!/bin/bash
# The account is on its contract: the entitlement records when the contract
# started, and the one fee whose only period ends with the first billing
# period (Onboarding) has its removal scheduled for then, with an event
# saying so. The fees with more periods to run are left alone for now.
set -euo pipefail
. ./lib.sh

start=$(starts_at)
onboarding_ends=$(expected e2e-ct-offer items | jq -r '.[] | select(.name == "Onboarding") | .endsAt')

started=$(kubectl get billingentitlement "$ENTITLEMENT" -n "$NS" -o jsonpath='{.metadata.annotations.openmeter\.miloapis\.com/contractStartedAt}')
[ "$started" = "$start" ] || fail "contractStartedAt is \"${started}\", want ${start}"
echo "OK: the contract started ${start}"

# This entitlement's events only: one of the same name from an earlier run
# keeps its events for an hour.
uid=$(kubectl get billingentitlement "$ENTITLEMENT" -n "$NS" -o jsonpath='{.metadata.uid}')

check() {
  events=$(kubectl get events -n "$NS" -o json --field-selector "involvedObject.uid=${uid},reason=FeeEndScheduled" |
    jq -c '[.items[].message]')
  [ "$(echo "$events" | jq --arg ends "$onboarding_ends" 'map(select(startswith("Onboarding") and contains($ends))) | length')" = "1" ] &&
    [ "$(echo "$events" | jq 'length')" = "1" ]
}
retry 20 check || fail "want one FeeEndScheduled event, for Onboarding ending ${onboarding_ends}" "${events:-[]}"
echo "OK: Onboarding's removal is scheduled for ${onboarding_ends}"
