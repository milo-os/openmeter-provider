#!/bin/sh
# Polls until OpenMeter reports no primaryEmail for the customer.
#
# Regression coverage for a clear that never propagated: the replace-PUT
# omitted primaryEmail when the desired value was empty, and OpenMeter
# preserves omitted fields, so the stale address stayed forever (and the
# drift check rewrote the customer on every reconcile trying to fix it).
# Accepts either an absent primaryEmail or an explicitly empty one.
#
# Usage: verify-email-cleared.sh <customer-key>
set -eu

key="$1"
url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/customers/${key}"

for i in $(seq 1 12); do
  body="$(kubectl exec -n openmeter-system openmeter-query -- wget -q -O - "$url" 2>/dev/null || true)"
  if [ -n "$body" ]; then
    # Cleared == the field is gone, or present but empty.
    if ! echo "$body" | grep -q '"primaryEmail":"[^"]'; then
      echo "customer ${key} primaryEmail cleared: ${body}"
      exit 0
    fi
  fi
  sleep 2
done

echo "customer ${key} primaryEmail did not clear"
echo "last response: ${body:-<empty>}"
exit 1
