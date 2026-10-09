#!/bin/bash
# Verifies that an Offer referencing meterName metrics.miloapis.com/cpu-usage
# (whose MeterDefinition metadata.name differs: cpu-usage-meter) converges to
# a published plan whose features are bound to the meter slug derived from
# spec.meterName — NOT from metadata.name.
#
# Regression guard for F1: when the slug was derived from metadata.name,
# features pointed at a non-existent meter and the Offer never converged.
#
# Usage: verify.sh <offer-uid>
set -euo pipefail
QUERY_POD=openmeter-query-slug
. ../lib.sh

key=$(plan_key "$1")
want_slug="metrics_miloapis_com_cpu_usage"

has_active_plan() {
  plan=$(active_plan "$key")
  [ -n "$plan" ]
}
echo "Waiting for active plan ${key}..."
retry 20 has_active_plan || fail "F1 REGRESSION? plan never published" "$(plan_versions "$key")"

expect_jq "$plan" '.name == "E2E Slug Mismatch Offer"' "plan name must come from the display-name annotation"
expect_jq "$plan" '.phases[0].rateCards | length == 2' "expected 2 rate cards (region-matched + catch-all)"

cards=$(usage_cards "$plan" | jq -s -c '.')
expect_jq "$cards" 'length == 2 and all(.feature != null)' "both usage rate cards must reference existing features"
expect_jq "$cards" "all(.feature.meterSlug == \"${want_slug}\")" "F1 REGRESSION: features must be bound to ${want_slug}"
expect_jq "$cards" "all(.featureKey | startswith(\"${want_slug}_\"))" "feature keys must be derived from the meterName slug"

matched=$(echo "$cards" | jq -c '.[] | select(.feature.advancedMeterGroupByFilters.region["$eq"] == "us-east")')
[ -n "$matched" ] || fail "no feature filtered on region=us-east" "$cards"
catch=$(echo "$cards" | jq -c '.[] | select(.feature.advancedMeterGroupByFilters.region["$nin"] == ["us-east"])')
[ -n "$catch" ] || fail "catch-all feature must filter region \$nin [us-east]" "$cards"

echo "Slug-mismatch sync verified: features bound to ${want_slug}, plan published."
