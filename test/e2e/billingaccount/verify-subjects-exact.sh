#!/bin/sh
# Polls OpenMeter for a customer and asserts usageAttribution.subjectKeys is
# EXACTLY the given set (order-independent) — not merely a superset. This is
# a stricter check than verify-customer.sh's "contains these subjects", which
# would silently pass even if a project that should have been dropped (e.g.
# after its BillingAccountBinding transitions to Superseded, or is unbound)
# were still present alongside the ones that should remain.
#
# Runs from the chainsaw host machine, reaching into the cluster via
# `kubectl exec` on the openmeter-query pod (the app pods are distroless and
# offer no shell/curl of their own).
#
# Usage: verify-subjects-exact.sh <key> [<want-subject> ...]
# Pass zero want-subjects to assert subjectKeys is exactly empty.
set -eu

key="$1"
shift
# Guard the zero-subject case explicitly: `printf '%s\n'` with no further
# args still emits one empty line (not zero lines), which would otherwise
# make want_sorted a single "," instead of the empty string got_sorted
# produces when subjectKeys is genuinely [].
if [ "$#" -eq 0 ]; then
  want_sorted=""
else
  want_sorted="$(printf '%s\n' "$@" | sort | tr '\n' ',')"
fi

url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/customers/${key}"

for i in $(seq 1 15); do
  body=$(kubectl exec -n openmeter-system openmeter-query -- wget -q -O - "$url" 2>/dev/null || true)

  # subjectKeys is a JSON array of quoted strings; extract just the array's
  # contents, pull each quoted string out, sort, and compare as a
  # comma-joined set — order-independent, and any extra or missing entry
  # changes the joined string.
  array_body=$(echo "$body" | grep -o '"subjectKeys":\[[^]]*\]' || true)
  got_sorted=$(echo "$array_body" | grep -o '"[^"]*"' | sed '1d' | tr -d '"' | sort | tr '\n' ',')

  if [ "$got_sorted" = "$want_sorted" ]; then
    echo "customer ${key} subjectKeys converged: [${got_sorted%,}] (want [${want_sorted%,}])"
    exit 0
  fi
  sleep 2
done

echo "customer ${key} subjectKeys did not converge: got [${got_sorted%,}], want [${want_sorted%,}]"
echo "last response: ${body}"
exit 1
