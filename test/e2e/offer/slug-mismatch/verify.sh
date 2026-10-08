#!/bin/bash
# Verifies that an Offer referencing meterName metrics.miloapis.com/cpu-usage
# (whose metadata.name differs: cpu-usage-meter) converges to an OpenMeter Plan
# whose feature key and feature-to-meter association are derived from
# spec.meterName — NOT from metadata.name.
#
# This is the regression guard for the F1 bug: when the reconciler derived the
# meter slug from metadata.name the feature key became cpu_usage_meter_* ,
# feature creation pointed at a non-existent meter slug, and the Offer never
# converged.
set -euo pipefail

offer_key="${1//-/_}"
plan_url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/plans?key=${offer_key}"
feature_url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/features"

# Slug must be derived from spec.meterName.
want_slug="metrics_miloapis_com_cpu_usage"
want_default_key="${want_slug}_default"
want_region_key="${want_slug}_region_us_east"

echo "Waiting for plan ${offer_key} to converge..."
for i in $(seq 1 20); do
  body=$(kubectl exec -n openmeter-system openmeter-query-slug -- wget -q -O - "$plan_url" 2>/dev/null || true)
  count=$(echo "$body" | jq -r '.items | length' 2>/dev/null || echo "0")
  [ "$count" -gt 0 ] && break
  sleep 2
done

if [ "$count" -eq 0 ]; then
  echo "F1 REGRESSION? Plan never converged. Last response:"
  echo "$body"
  exit 1
fi

plan=$(echo "$body" | jq -r '.items[0]')

# 1. Plan name from display-name annotation.
plan_name=$(echo "$plan" | jq -r '.name')
if [ "$plan_name" != "E2E Slug Mismatch Offer" ]; then
  echo "ERROR: plan name expected 'E2E Slug Mismatch Offer', got '$plan_name'"
  exit 1
fi

# 2. Exactly 2 rate cards (region-matched + default), both usage_based.
ratecards_count=$(echo "$plan" | jq -r '.phases[0].rateCards | length')
if [ "$ratecards_count" -ne 2 ]; then
  echo "ERROR: expected 2 rate cards, got $ratecards_count"
  exit 1
fi

for key in "$want_region_key" "$want_default_key"; do
  rc=$(echo "$plan" | jq -r ".phases[0].rateCards[] | select(.key == \"$key\")")
  if [ -z "$rc" ]; then
    echo "ERROR: rate card key '$key' missing from plan"
    exit 1
  fi
  if [ "$(echo "$rc" | jq -r '.type')" != "usage_based" ]; then
    echo "ERROR: rate card '$key' not usage_based"
    exit 1
  fi
  if [ "$(echo "$rc" | jq -r '.featureKey')" != "$key" ]; then
    echo "ERROR: rate card '$key' featureKey mismatch"
    exit 1
  fi
done

# 3. Features must exist and be bound to the meter slug derived from
#    spec.meterName — this is the exact F1 assertion.
echo "Validating features are bound to slug '${want_slug}'..."
features_body=$(kubectl exec -n openmeter-system openmeter-query-slug -- wget -q -O - "$feature_url" 2>/dev/null || true)

f_default=$(echo "$features_body" | jq -r ".[] | select(.key == \"$want_default_key\")")
f_region=$(echo "$features_body" | jq -r ".[] | select(.key == \"$want_region_key\")")
if [ -z "$f_default" ] || [ -z "$f_region" ]; then
  echo "F1 REGRESSION? Expected feature keys '${want_default_key}' and '${want_region_key}'."
  echo "Features present:"; echo "$features_body" | jq -r '.[].key'
  exit 1
fi

if [ "$(echo "$f_default" | jq -r '.meterSlug')" != "$want_slug" ]; then
  echo "F1 REGRESSION: default feature meterSlug = '$(echo "$f_default" | jq -r '.meterSlug')', want '${want_slug}'"
  exit 1
fi
if [ "$(echo "$f_region" | jq -r '.meterSlug')" != "$want_slug" ]; then
  echo "F1 REGRESSION: region feature meterSlug = '$(echo "$f_region" | jq -r '.meterSlug')', want '${want_slug}'"
  exit 1
fi

# 4. Region feature must carry the advanced group-by filter on region=us-east.
filter_val=$(echo "$f_region" | jq -r '.advancedMeterGroupByFilters.region."$eq"')
if [ "$filter_val" != "us-east" ]; then
  echo "ERROR: region feature advanced filter = '$filter_val', want 'us-east'"
  exit 1
fi

echo "Slug-mismatch sync verified: features bound to slug '${want_slug}', plan converged."
exit 0
