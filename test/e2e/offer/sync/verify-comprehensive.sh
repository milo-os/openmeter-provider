#!/bin/bash
set -euo pipefail

offer_key="${1//-/_}"
want_name="$2"

url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/plans?key=${offer_key}"
feature_url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/features"

echo "Waiting for plan ${offer_key} to converge..."

# Wait for plan to exist
for i in $(seq 1 15); do
  body=$(kubectl exec -n openmeter-system openmeter-query -- wget -q -O - "$url" 2>/dev/null || true)
  
  # Ensure valid JSON and get item count
  count=$(echo "$body" | jq -r '.items | length' 2>/dev/null || echo "0")
  if [ "$count" -gt 0 ]; then
    break
  fi
  sleep 2
done

if [ "$count" -eq 0 ]; then
  echo "Plan not found."
  echo "Last response: $body"
  exit 1
fi

echo "Plan found. Validating state..."
plan=$(echo "$body" | jq -r '.items[0]')

# Validate Plan Name
plan_name=$(echo "$plan" | jq -r '.name')
if [ "$plan_name" != "$want_name" ]; then
  echo "ERROR: Plan name expected '$want_name', got '$plan_name'"
  exit 1
fi

# Validate Phases (should have 1 phase)
phases_count=$(echo "$plan" | jq -r '.phases | length')
if [ "$phases_count" -ne 1 ]; then
  echo "ERROR: Expected 1 phase, got $phases_count"
  exit 1
fi

phase=$(echo "$plan" | jq -r '.phases[0]')
ratecards_count=$(echo "$phase" | jq -r '.rateCards | length')
# usage-item-multi-region has 3 rates (us-east flat, eu-west tiered, default flat) -> 3 rate cards
# recurring-base-fee has 1 -> 1 rate card
# one-time-setup has 1 -> 1 rate card
# Total: 5 rate cards
if [ "$ratecards_count" -ne 5 ]; then
  echo "ERROR: Expected 5 rate cards, got $ratecards_count"
  exit 1
fi

# Fetch features to validate advanced filters
echo "Validating Features..."
features_body=$(kubectl exec -n openmeter-system openmeter-query -- wget -q -O - "$feature_url" 2>/dev/null || true)

function validate_feature() {
  local key=$1
  local dim_val=$2 # Expected value for 'region'
  
  feature=$(echo "$features_body" | jq -r ".[] | select(.key == \"$key\")")
  if [ -z "$feature" ]; then
    echo "ERROR: Feature $key not found"
    exit 1
  fi
  
  if [ "$dim_val" != "default" ]; then
    filter_val=$(echo "$feature" | jq -r '.advancedMeterGroupByFilters.region."$eq"')
    if [ "$filter_val" != "$dim_val" ]; then
      echo "ERROR: Feature $key expected region=$dim_val, got $filter_val"
      exit 1
    fi
  else
    has_filter=$(echo "$feature" | jq -r '.advancedMeterGroupByFilters | length')
    if [ "$has_filter" -gt 0 ]; then
      echo "ERROR: Feature $key expected no filters, got filters"
      exit 1
    fi
  fi
}

validate_feature "e2e_metric_region_us_east" "us-east"
validate_feature "e2e_metric_region_eu_west" "eu-west"
validate_feature "e2e_metric_default" "default"


echo "Validating RateCards..."

function get_ratecard() {
  local key_suffix=$1
  echo "$phase" | jq -r ".rateCards[] | select(.key == \"$key_suffix\")"
}

# 1. us-east flat fee
rc_us=$(get_ratecard "e2e_metric_region_us_east")
if [ "$(echo "$rc_us" | jq -r '.type')" != "usage_based" ]; then echo "ERROR us-east type"; exit 1; fi
if [ "$(echo "$rc_us" | jq -r '.featureKey')" != "e2e_metric_region_us_east" ]; then echo "ERROR us-east feature"; exit 1; fi
if [ "$(echo "$rc_us" | jq -r '.price.type')" != "unit" ]; then echo "ERROR us-east price type"; exit 1; fi
if [ "$(echo "$rc_us" | jq -r '.price.amount')" != "0.10" ] && [ "$(echo "$rc_us" | jq -r '.price.amount')" != "0.1" ]; then echo "ERROR us-east amount: $(echo "$rc_us" | jq -r '.price.amount')"; exit 1; fi
if [ "$(echo "$rc_us" | jq -r '.metadata["miloapis.com/pricing-unit"]')" != "vcpu" ]; then echo "ERROR metadata unit"; exit 1; fi

# 2. eu-west tiered
rc_eu=$(get_ratecard "e2e_metric_region_eu_west")
if [ "$(echo "$rc_eu" | jq -r '.price.type')" != "tiered" ]; then echo "ERROR eu-west price type"; exit 1; fi
if [ "$(echo "$rc_eu" | jq -r '.price.mode')" != "graduated" ]; then echo "ERROR eu-west tier mode"; exit 1; fi
tier0_upto=$(echo "$rc_eu" | jq -r '.price.tiers[0].upToAmount')
tier0_rate=$(echo "$rc_eu" | jq -r '.price.tiers[0].unitPrice.amount')
tier1_upto=$(echo "$rc_eu" | jq -r '.price.tiers[1].upToAmount')
tier1_rate=$(echo "$rc_eu" | jq -r '.price.tiers[1].unitPrice.amount')

if [ "$tier0_upto" != "100" ]; then echo "ERROR eu-west tier0 upto"; exit 1; fi
if [ "$tier0_rate" != "0.12" ] && [ "$tier0_rate" != "0.12" ]; then echo "ERROR eu-west tier0 rate: $tier0_rate"; exit 1; fi
if [ "$tier1_upto" != "null" ]; then echo "ERROR eu-west tier1 upto"; exit 1; fi
if [ "$tier1_rate" != "0.08" ] && [ "$tier1_rate" != "0.08" ]; then echo "ERROR eu-west tier1 rate: $tier1_rate"; exit 1; fi

# 3. default flat fee
rc_def=$(get_ratecard "e2e_metric_default")
if [ "$(echo "$rc_def" | jq -r '.price.amount')" != "0.05" ] && [ "$(echo "$rc_def" | jq -r '.price.amount')" != "0.05" ]; then echo "ERROR default amount: $(echo "$rc_def" | jq -r '.price.amount')"; exit 1; fi

# 4. recurring fee
rc_rec=$(get_ratecard "recurring_base_fee")
if [ "$(echo "$rc_rec" | jq -r '.type')" != "flat_fee" ]; then echo "ERROR recurring type"; exit 1; fi
if [ "$(echo "$rc_rec" | jq -r '.billingCadence')" != "P1M" ]; then echo "ERROR recurring cadence"; exit 1; fi
if [ "$(echo "$rc_rec" | jq -r '.price.amount')" != "50.00" ] && [ "$(echo "$rc_rec" | jq -r '.price.amount')" != "50" ]; then echo "ERROR recurring amount: $(echo "$rc_rec" | jq -r '.price.amount')"; exit 1; fi
if [ "$(echo "$rc_rec" | jq -r '.metadata["miloapis.com/service-ref"]')" != "platform.miloapis.com" ]; then echo "ERROR recurring meta"; exit 1; fi

# 5. one-time fee
rc_one=$(get_ratecard "one_time_setup")
if [ "$(echo "$rc_one" | jq -r '.billingCadence')" != "null" ]; then echo "ERROR one-time cadence"; exit 1; fi
if [ "$(echo "$rc_one" | jq -r '.price.amount')" != "100.00" ] && [ "$(echo "$rc_one" | jq -r '.price.amount')" != "100" ]; then echo "ERROR one-time amount: $(echo "$rc_one" | jq -r '.price.amount')"; exit 1; fi
if [ "$(echo "$rc_one" | jq -r '.metadata["miloapis.com/trigger"]')" != "BillingAccountActivation" ]; then echo "ERROR one-time trigger"; exit 1; fi

echo "All OpenMeter validations passed successfully!"
exit 0
