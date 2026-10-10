#!/bin/bash
# Verifies no OpenMeter plan and no feature exist for the rejected Offer:
# validation must happen before anything is written to OpenMeter.
#
# Usage: verify-absent.sh <offer-uid>
set -euo pipefail
QUERY_POD=openmeter-query-invalid
. ../lib.sh

key=$(plan_key "$1")
for _ in $(seq 1 5); do
  versions=$(plan_versions "$key")
  expect_jq "$versions" 'length == 0' "a plan was created for an Offer that cannot be represented"
  features=$(om_get "/features?meterSlug=invalid_metric")
  expect_jq "$features" 'length == 0' "features were created for an Offer that cannot be represented"
  sleep 2
done
echo "Rejected Offer left no plan or feature in OpenMeter."
