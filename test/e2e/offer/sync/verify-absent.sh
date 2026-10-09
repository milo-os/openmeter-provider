#!/bin/bash
# Verifies no OpenMeter plan exists for an Offer, polling long enough for a
# misbehaving reconciler to have created one.
#
# Usage: verify-absent.sh <offer-uid>
set -euo pipefail
QUERY_POD=openmeter-query
. ../lib.sh

key=$(plan_key "$1")
for _ in $(seq 1 5); do
  count=$(plan_versions "$key" | jq 'length')
  [ "$count" -eq 0 ] || fail "found ${count} plan version(s) for ${key}" "$(plan_versions "$key")"
  sleep 2
done
echo "No plan for ${key}."
