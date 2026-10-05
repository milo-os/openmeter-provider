#!/bin/sh
# Polls OpenMeter for a customer (addressed by its external key, i.e. the
# BillingAccount's metadata.uid) and asserts name, primaryEmail, currency,
# and that a set of project names appear somewhere in the response —
# a cheap but sufficient proxy for usageAttribution.subjectKeys containing
# them, since none of the other fields can plausibly contain a project name.
#
# Runs from the chainsaw host machine, reaching into the cluster via
# `kubectl exec` on the openmeter-query pod (the app pods are distroless and
# offer no shell/curl of their own).
#
# Usage: verify-customer.sh <key> <want-name> <want-email> <want-currency> [want-subject-key ...]
set -eu

key="$1"
want_name="$2"
want_email="$3"
want_currency="$4"
shift 4

url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/customers/${key}"

for i in $(seq 1 12); do
  body=$(kubectl exec -n openmeter-system openmeter-query -- wget -q -O - "$url" 2>/dev/null || true)

  ok=1
  echo "$body" | grep -q "\"name\":\"${want_name}\"" || ok=0
  echo "$body" | grep -q "\"primaryEmail\":\"${want_email}\"" || ok=0
  echo "$body" | grep -q "\"currency\":\"${want_currency}\"" || ok=0
  for subject in "$@"; do
    echo "$body" | grep -q "\"${subject}\"" || ok=0
  done

  if [ "$ok" = "1" ]; then
    echo "customer ${key} converged: ${body}"
    exit 0
  fi
  sleep 2
done

echo "customer ${key} did not converge to name=${want_name} email=${want_email} currency=${want_currency} subjectKeys=$*"
echo "last response: ${body}"
exit 1
