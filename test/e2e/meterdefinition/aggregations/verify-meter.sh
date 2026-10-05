#!/bin/sh
# Polls OpenMeter for a meter and asserts its aggregation, valueProperty
# presence, and (optionally) a groupBy key, all of which are what
# openmeter.EnsureMeter derives from MeterDefinition.spec.measurement.
#
# Runs from the chainsaw host machine, reaching into the cluster via
# `kubectl exec` on the openmeter-query pod (the app pods are distroless and
# offer no shell/curl of their own).
#
# Usage: verify-meter.sh <slug> <want-aggregation> <yes|no: want valueProperty> [want-groupby-key]
set -eu

slug="$1"
want_agg="$2"
want_value_property="$3"
want_groupby_key="${4:-}"

url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/meters/${slug}"

for i in $(seq 1 12); do
  body=$(kubectl exec -n openmeter-system openmeter-query -- wget -q -O - "$url" 2>/dev/null || true)

  ok=1
  echo "$body" | grep -q "\"aggregation\":\"${want_agg}\"" || ok=0
  if [ "$want_value_property" = "yes" ]; then
    echo "$body" | grep -q '"valueProperty":"\$\.value"' || ok=0
  else
    echo "$body" | grep -q '"valueProperty"' && ok=0
  fi
  if [ -n "$want_groupby_key" ]; then
    echo "$body" | grep -q "\"${want_groupby_key}\":" || ok=0
  fi

  if [ "$ok" = "1" ]; then
    echo "meter ${slug} converged: ${body}"
    exit 0
  fi
  sleep 2
done

echo "meter ${slug} did not converge to aggregation=${want_agg} valueProperty=${want_value_property} groupByKey=${want_groupby_key}"
echo "last response: ${body}"
exit 1
