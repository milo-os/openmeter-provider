#!/bin/bash
# Simulates the billing operator publishing an Offer. The e2e cluster has
# billing's CRDs but not its operator, so nothing else performs these steps.
#
# Real publish flow (milo-os/billing):
#   1. A client creates the Offer as Draft with spec.servicePricingRefs. Its
#      webhook forbids clients from setting spec.servicePricings.
#   2. The client flips spec.launchStage to GA. The snapshot is still empty.
#   3. Billing's Offer controller (buildSnapshots) resolves each ref and writes
#      spec.servicePricings as an unmodified copy of each ServicePricing spec,
#      in ref order. A ref without a namespace defaults to milo-system.
#
# Usage:
#   publish.sh <offer> ga        step 2 only
#   publish.sh <offer> snapshot  step 3 only
#   publish.sh <offer>           steps 2 and 3
set -euo pipefail

offer="$1"
step="${2:-all}"

if [ "$step" = "ga" ] || [ "$step" = "all" ]; then
  kubectl patch offer "$offer" --type merge -p '{"spec":{"launchStage":"GA"}}'
fi

if [ "$step" = "snapshot" ] || [ "$step" = "all" ]; then
  offer_json=$(kubectl get offer "$offer" -o json)
  refs=$(echo "$offer_json" | jq -c '.spec.servicePricingRefs // []')
  offer_discount=$(echo "$offer_json" | jq -c '.spec.discount // null')
  if [ "$(echo "$refs" | jq 'length')" -eq 0 ]; then
    echo "publish.sh: offer ${offer} has no servicePricingRefs to snapshot"
    exit 1
  fi
  snapshot='[]'
  count=$(echo "$refs" | jq 'length')
  for i in $(seq 0 $((count - 1))); do
    name=$(echo "$refs" | jq -r ".[$i].name")
    ns=$(echo "$refs" | jq -r ".[$i] | if (.namespace // \"\") == \"\" then \"milo-system\" else .namespace end")
    if ! sp=$(kubectl get servicepricing "$name" -n "$ns" -o json); then
      echo "publish.sh: ServicePricing ${ns}/${name} referenced by ${offer} not found"
      exit 1
    fi
    discount=$(echo "$refs" | jq -c --argjson od "$offer_discount" ".[$i].discount // \$od")
    if [ "$discount" != "null" ]; then
      snapshot=$(echo "$snapshot" | jq -c --argjson sp "$sp" --argjson disc "$discount" '. + [{name: $sp.metadata.name, spec: $sp.spec, discount: $disc}]')
    else
      snapshot=$(echo "$snapshot" | jq -c --argjson sp "$sp" '. + [{name: $sp.metadata.name, spec: $sp.spec}]')
    fi
  done
  kubectl patch offer "$offer" --type merge -p "{\"spec\":{\"servicePricings\":${snapshot}}}"
  echo "Snapshotted $(echo "$snapshot" | jq 'length') service pricing(s) into ${offer}."
fi
