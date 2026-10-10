#!/bin/bash
# Delete-convergence verification.
#
# Usage:
#   verify.sh <offer-uid> present
#       Wait for the plan to have two versions — v1 archived, v2 active —
#       and its usage feature to be active.
#   verify.sh <offer-uid> absent
#       Wait for every plan version to be deleted, every feature on the
#       Offer's meter to be archived, and the Offer object to be gone
#       (finalizer released). Archiving features is what lets the meter be
#       deleted later: OpenMeter refuses to delete a meter with active
#       features.
set -euo pipefail
QUERY_POD=openmeter-query-del
. ../lib.sh

key=$(plan_key "$1")
mode="$2"
meter_slug="del_metric"

active_meter_features() {
  om_get "/features?meterSlug=${meter_slug}" | jq -c '.'
}

case "$mode" in
present)
  two_versions() {
    versions=$(plan_versions "$key")
    [ "$(echo "$versions" | jq 'map(select(.status == "active")) | length')" -eq 1 ] &&
      [ "$(echo "$versions" | jq 'length')" -eq 2 ]
  }
  echo "Waiting for plan ${key} to have an archived v1 and an active v2..."
  retry 20 two_versions || fail "plan ${key} did not reach two versions" "$(plan_versions "$key")"
  expect_jq "$versions" '(map(select(.version == 1))[0].status) == "archived"' "v1 must be archived"
  features=$(active_meter_features)
  expect_jq "$features" 'length == 1' "expected exactly one active feature on ${meter_slug}"
  echo "Plan ${key} present with two versions."
  ;;

absent)
  no_versions() {
    [ "$(plan_versions "$key" | jq 'length')" -eq 0 ]
  }
  echo "Waiting for every version of plan ${key} to be deleted..."
  retry 20 no_versions || fail "plan ${key} still has live versions (orphan risk)" "$(plan_versions "$key")"

  no_features() {
    [ "$(active_meter_features | jq 'length')" -eq 0 ]
  }
  echo "Waiting for the Offer's features on ${meter_slug} to be archived..."
  retry 10 no_features || fail "features on ${meter_slug} left active after Offer deletion" "$(active_meter_features)"

  offer_gone() {
    ! kubectl get offer e2e-offer-delete >/dev/null 2>&1
  }
  echo "Waiting for the Offer object to be fully deleted (finalizer released)..."
  retry 20 offer_gone || {
    kubectl get offer e2e-offer-delete -o yaml || true
    fail "Offer e2e-offer-delete still present — finalizer likely wedged"
  }
  echo "Plan deleted, features archived, Offer gone."
  ;;

*)
  echo "verify.sh: unknown mode '$mode' (want present|absent)"
  exit 2
  ;;
esac
