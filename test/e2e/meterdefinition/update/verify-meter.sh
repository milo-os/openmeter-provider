#!/bin/sh
# Polls OpenMeter for a meter and asserts its description and a set of
# required groupBy keys — the mutable fields openmeter.EnsureMeter converges
# via UpdateMeter when a MeterDefinition's displayName/description/dimensions
# change post-creation.
#
# Runs from the chainsaw host machine, reaching into the cluster via
# `kubectl exec` on the openmeter-query pod.
#
# Usage: verify-meter.sh <slug> <expected-description> [required-groupby-key ...]
set -eu

slug="$1"
want_desc="$2"
shift 2

url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/meters/${slug}"

for i in $(seq 1 12); do
  body=$(kubectl exec -n openmeter-system openmeter-query -- wget -q -O - "$url" 2>/dev/null || true)

  ok=1
  echo "$body" | grep -q "\"description\":\"${want_desc}\"" || ok=0
  for key in "$@"; do
    echo "$body" | grep -q "\"${key}\":" || ok=0
  done

  if [ "$ok" = "1" ]; then
    echo "meter ${slug} converged: ${body}"
    exit 0
  fi
  sleep 2
done

echo "meter ${slug} did not converge to description=${want_desc} groupByKeys=$*"
echo "last response: ${body}"
exit 1
