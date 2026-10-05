#!/bin/sh
# Polls OpenMeter until it no longer has the given meter slug, confirming
# reconcileDelete's DeleteMeter call actually reached OpenMeter rather than
# the finalizer being dropped for some other reason.
#
# Usage: verify-deleted.sh <slug>
set -eu

slug="$1"
url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/meters/${slug}"

for i in $(seq 1 8); do
  if ! kubectl exec -n openmeter-system openmeter-query -- wget -q -O /dev/null "$url" 2>/dev/null; then
    echo "meter ${slug} confirmed gone from OpenMeter"
    exit 0
  fi
  sleep 2
done

echo "meter ${slug} is still reachable in OpenMeter after delete"
exit 1
