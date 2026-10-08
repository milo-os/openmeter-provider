#!/bin/bash
# Delete-convergence verification.
#
# Usage:
#   verify.sh <offer-key> <mode>
#
#   mode=present  : wait for the OpenMeter plan identified by <offer-key> to
#                   exist (create-convergence check).
#   mode=absent   : wait for the plan to be absent from OpenMeter AND for the
#                   Offer object to be fully gone from Kubernetes (finalizer
#                   released / deletion completed). This is the safety property:
#                   no live plan may be orphaned, and no finalizer may wedge the
#                   object.
set -euo pipefail

offer_key="${1//-/_}"
mode="$2"
url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/plans?key=${offer_key}"

if [ "$mode" = "present" ]; then
  echo "Waiting for plan ${offer_key} to converge..."
  for i in $(seq 1 20); do
    body=$(kubectl exec -n openmeter-system openmeter-query-del -- wget -q -O - "$url" 2>/dev/null || true)
    count=$(echo "$body" | jq -r '.items | length' 2>/dev/null || echo "0")
    [ "$count" -gt 0 ] && break
    sleep 2
  done
  if [ "$count" -gt 0 ]; then
    echo "Plan ${offer_key} present."
    exit 0
  fi
  echo "ERROR: plan ${offer_key} did not converge."
  echo "Last response: $body"
  exit 1

elif [ "$mode" = "absent" ]; then
  echo "Waiting for plan ${offer_key} to be deleted from OpenMeter..."
  for i in $(seq 1 20); do
    body=$(kubectl exec -n openmeter-system openmeter-query-del -- wget -q -O - "$url" 2>/dev/null || true)
    count=$(echo "$body" | jq -r '.items | length' 2>/dev/null || echo "0")
    [ "$count" -eq 0 ] && break
    sleep 2
  done
  if [ "$count" -ne 0 ]; then
    echo "ERROR: plan ${offer_key} still exists after Offer deletion (unsync risk!)."
    echo "Response: $body"
    exit 1
  fi

  echo "Waiting for Offer object to be fully deleted (finalizer released)..."
  for i in $(seq 1 20); do
    obj=$(kubectl get offer e2e-offer-delete -n openmeter-system -o json 2>/dev/null || true)
    if [ -z "$obj" ]; then
      echo "Offer object gone."
      exit 0
    fi
    sleep 2
  done
  echo "ERROR: Offer e2e-offer-delete still present — finalizer likely wedged."
  kubectl get offer e2e-offer-delete -n openmeter-system -o yaml || true
  exit 1

else
  echo "verify.sh: unknown mode '$mode' (want present|absent)"
  exit 2
fi
