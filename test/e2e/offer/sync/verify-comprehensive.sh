#!/bin/bash
# Verifies the GA Offer in offer.yaml converged to a published OpenMeter plan
# carrying every pricing shape, with usage features that partition the
# meter's usage (no double billing between matched rates and the catch-all).
#
# Usage: verify-comprehensive.sh <offer-uid> <want-plan-name>
set -euo pipefail
QUERY_POD=openmeter-query
. ../lib.sh

key=$(plan_key "$1")
want_name="$2"

has_named_active_plan() {
  plan=$(active_plan "$key")
  [ -n "$plan" ] && [ "$(echo "$plan" | jq -r '.name')" = "$want_name" ]
}
echo "Waiting for active plan ${key} named '${want_name}'..."
retry 15 has_named_active_plan || fail "no active plan ${key} named '${want_name}'" "$(plan_versions "$key")"

# A draft plan cannot be subscribed to: it must be published.
expect_jq "$plan" '.status == "active" and .version == 1' "plan must be published as version 1"
expect_jq "$plan" '.metadata["openmeter.miloapis.com/spec-hash"] | length > 0' "plan must carry its spec hash"
expect_jq "$plan" '.phases | length == 1' "expected 1 phase"
expect_jq "$plan" '.phases[0].rateCards | length == 5' "expected 5 rate cards (3 usage, 2 flat)"

echo "Validating usage rate cards and features..."
cards=$(usage_cards "$plan" | jq -s -c '.')
expect_jq "$cards" 'length == 3' "expected 3 usage rate cards"
expect_jq "$cards" 'all(.feature != null)' "every usage rate card must reference an existing feature"
expect_jq "$cards" 'all(.feature.meterSlug == "e2e_metric")' "features must be bound to meter e2e_metric"
expect_jq "$cards" 'all(.key == .featureKey and .feature.key == .featureKey)' "rate card key must equal its feature key"
expect_jq "$cards" 'all(.billingCadence == "P1M")' "usage cards must bill monthly"
expect_jq "$cards" 'all(.metadata["miloapis.com/pricing-unit"] == "vcpu")' "usage cards must carry the pricing unit"

us=$(echo "$cards" | jq -c '.[] | select(.feature.advancedMeterGroupByFilters.region["$eq"] == "us-east")')
[ -n "$us" ] || fail "no usage card filtered on region=us-east" "$cards"
expect_jq "$us" '.price.type == "unit" and (.price.amount | tonumber) == 0.1' "us-east must be a 0.10 unit price"

eu=$(echo "$cards" | jq -c '.[] | select(.feature.advancedMeterGroupByFilters.region["$eq"] == "eu-west")')
[ -n "$eu" ] || fail "no usage card filtered on region=eu-west" "$cards"
expect_jq "$eu" '.price.type == "tiered" and .price.mode == "graduated"' "eu-west must be graduated tiers"
expect_jq "$eu" '(.price.tiers[0].upToAmount | tonumber) == 100 and (.price.tiers[0].unitPrice.amount | tonumber) == 0.12' "eu-west tier 0"
expect_jq "$eu" '.price.tiers[1].upToAmount == null and (.price.tiers[1].unitPrice.amount | tonumber) == 0.08' "eu-west tier 1 must be open-ended"

# The catch-all must EXCLUDE the matched regions; an unfiltered catch-all
# would bill us-east and eu-west usage a second time.
catch=$(echo "$cards" | jq -c '.[] | select(.feature.advancedMeterGroupByFilters.region["$nin"] != null)')
[ -n "$catch" ] || fail "catch-all rate card must filter region \$nin the matched values" "$cards"
expect_jq "$catch" '(.feature.advancedMeterGroupByFilters.region["$nin"] | sort) == ["eu-west", "us-east"]' "catch-all must exclude exactly eu-west and us-east"
expect_jq "$catch" '(.price.amount | tonumber) == 0.05' "catch-all must be a 0.05 unit price"

echo "Validating flat-fee rate cards..."
rec=$(flat_card "$plan" recurring-base-fee)
[ -n "$rec" ] || fail "recurring-base-fee rate card missing" "$plan"
expect_jq "$rec" '.billingCadence == "P1M" and (.price.amount | tonumber) == 50' "recurring fee must be 50.00 monthly"
expect_jq "$rec" '.metadata["miloapis.com/service-ref"] == "platform.miloapis.com"' "recurring fee service-ref"

one=$(flat_card "$plan" one-time-setup)
[ -n "$one" ] || fail "one-time-setup rate card missing" "$plan"
expect_jq "$one" '.billingCadence == null and (.price.amount | tonumber) == 100' "one-time fee must be 100.00 with no cadence"
expect_jq "$one" '.metadata["miloapis.com/trigger"] == "BillingAccountActivation"' "one-time fee trigger"

echo "All OpenMeter validations passed."
