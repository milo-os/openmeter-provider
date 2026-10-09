#!/bin/bash
# Verifies that changing a GA Offer's display name published a new plan
# version (active plans are immutable in OpenMeter) and archived the old one.
#
# Usage: verify-renamed.sh <offer-uid> <want-plan-name>
set -euo pipefail
QUERY_POD=openmeter-query
. ../lib.sh

key=$(plan_key "$1")
want_name="$2"

renamed() {
  plan=$(active_plan "$key")
  [ -n "$plan" ] && [ "$(echo "$plan" | jq -r '.name')" = "$want_name" ]
}
echo "Waiting for plan ${key} to be republished as '${want_name}'..."
retry 15 renamed || fail "active plan never renamed to '${want_name}'" "$(plan_versions "$key")"

versions=$(plan_versions "$key")
expect_jq "$versions" 'length == 2' "expected exactly 2 plan versions"
expect_jq "$versions" 'map(select(.status == "active")) | length == 1' "exactly one version must be active"
expect_jq "$versions" '(map(select(.status == "active"))[0].version) == 2' "the new version must be 2"
expect_jq "$versions" '(map(select(.version == 1))[0].status) == "archived"' "version 1 must be archived"
expect_jq "$versions" 'map(.phases[0].rateCards | map(.key) | sort) | .[0] == .[1]' "rate cards must be unchanged across versions"

echo "Plan ${key} republished as version 2; version 1 archived."
