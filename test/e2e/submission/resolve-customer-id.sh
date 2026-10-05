#!/bin/sh
# Resolves OpenMeter's internal customer id (a ULID) from the external
# customer key (the BillingAccount's metadata.uid). Needed because the meter
# query endpoint's filterCustomerId param only accepts the internal id, not
# the external key that every other client-facing endpoint accepts.
#
# Runs from the chainsaw host machine, reaching into the cluster via
# `kubectl exec` on the openmeter-query pod (the app pods are distroless and
# offer no shell/curl of their own).
#
# Usage: resolve-customer-id.sh <key>
# Prints the internal customer id to stdout.
set -eu

key="$1"
url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/customers/${key}"

for i in $(seq 1 12); do
  body=$(kubectl exec -n openmeter-system openmeter-query -- wget -q -O - "$url" 2>/dev/null || true)
  id=$(echo "$body" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
  if [ -n "$id" ]; then
    # printf, NOT echo: chainsaw's ($stdout) capture keeps a trailing
    # newline verbatim, and this value is substituted into a URL query
    # string — an embedded newline there makes the HTTP request line
    # malformed and OpenMeter answers 400.
    printf '%s' "$id"
    exit 0
  fi
  sleep 2
done

echo "customer ${key} not found (last response: ${body})" >&2
exit 1
