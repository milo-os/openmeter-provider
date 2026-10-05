#!/bin/sh
# Polls OpenMeter until it no longer has the given customer key, confirming
# reconcileDelete's DeleteCustomer call actually reached OpenMeter rather
# than the finalizer being dropped for some other reason.
#
# Usage: verify-deleted.sh <key>
set -eu

key="$1"
url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/customers/${key}"

for i in $(seq 1 8); do
  if ! kubectl exec -n openmeter-system openmeter-query -- wget -q -O /dev/null "$url" 2>/dev/null; then
    echo "customer ${key} confirmed gone from OpenMeter"
    exit 0
  fi
  sleep 2
done

echo "customer ${key} is still reachable in OpenMeter after delete"
exit 1
