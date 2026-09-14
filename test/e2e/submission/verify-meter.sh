#!/bin/sh
# Polls OpenMeter for a meter and asserts it exists with the expected
# aggregation — proof MeterDefinitionReconciler's EnsureMeter ran before we
# start publishing usage events for it.
#
# Runs from the chainsaw host machine, reaching into the cluster via
# `kubectl exec` on the openmeter-query pod (the app pods are distroless and
# offer no shell/curl of their own).
#
# Usage: verify-meter.sh <slug> <want-aggregation>
# <want-aggregation> is OpenMeter's own wire value (e.g. "SUM"), not the
# MeterDefinition CRD's spec.measurement.aggregation casing (e.g. "Sum") —
# OpenMeter upcases it server-side.
set -eu

slug="$1"
want_agg="$2"

url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/meters/${slug}"

for i in $(seq 1 12); do
  body=$(kubectl exec -n openmeter-system openmeter-query -- wget -q -O - "$url" 2>/dev/null || true)

  if echo "$body" | grep -q "\"aggregation\":\"${want_agg}\""; then
    echo "meter ${slug} converged: ${body}"
    exit 0
  fi
  sleep 2
done

echo "meter ${slug} did not converge to aggregation=${want_agg}"
echo "last response: ${body}"
exit 1
