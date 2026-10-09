#!/bin/bash
# Shared helpers for the offer e2e suites. Source from a suite directory:
#
#   QUERY_POD=openmeter-query . ../lib.sh
#
# The OpenMeter API is only reachable from inside the cluster, so every call
# goes through a long-lived busybox pod (QUERY_POD) via `kubectl exec`.
# Assertions never depend on generated feature or rate card keys: features
# are resolved through the plan's rate cards and identified by their
# meter and filters, flat fees by their service-pricing metadata.

OM_API="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1"

# om_get PATH — GET an OpenMeter API path; non-zero exit on HTTP errors.
om_get() {
  kubectl exec -n openmeter-system "$QUERY_POD" -- wget -q -O - "${OM_API}$1" 2>/dev/null
}

# plan_key UID — the OpenMeter plan key of an Offer.
plan_key() {
  echo "${1//-/_}"
}


# retry TRIES CMD... — run CMD every 2s until it succeeds or TRIES runs out.
retry() {
  local tries=$1
  shift
  for _ in $(seq 1 "$tries"); do
    if "$@"; then
      return 0
    fi
    sleep 2
  done
  return 1
}

# plan_versions KEY — every non-deleted version of the plan, as a JSON array.
plan_versions() {
  om_get "/plans?key=$1" | jq -c '.items // []'
}

# active_plan KEY — the active plan version as JSON, or nothing.
active_plan() {
  plan_versions "$1" | jq -c 'map(select(.status == "active")) | .[0] // empty'
}

# usage_cards PLAN_JSON — each usage rate card of the plan's single phase,
# one JSON object per line, with its feature attached as .feature.
usage_cards() {
  local plan=$1 card fkey feature
  echo "$plan" | jq -c '.phases[0].rateCards[] | select(.type == "usage_based")' | while read -r card; do
    fkey=$(echo "$card" | jq -r '.featureKey')
    feature=$(om_get "/features/${fkey}" || echo 'null')
    echo "$card" | jq -c --argjson feature "$feature" '. + {feature: $feature}'
  done
}

# flat_card PLAN_JSON SERVICE_PRICING — the flat-fee card built from the
# named ServicePricing.
flat_card() {
  echo "$1" | jq -c --arg sp "$2" \
    '.phases[0].rateCards[] | select(.type == "flat_fee" and .metadata["miloapis.com/service-pricing"] == $sp)'
}

# fail MESSAGE [JSON] — print and exit 1.
fail() {
  echo "ERROR: $1"
  if [ -n "${2:-}" ]; then
    echo "$2" | jq . 2>/dev/null || echo "$2"
  fi
  exit 1
}

# expect_jq JSON FILTER MESSAGE — fail unless FILTER evaluates to true.
expect_jq() {
  if [ "$(echo "$1" | jq -r "$2")" != "true" ]; then
    fail "$3" "$1"
  fi
}
