#!/bin/sh
# Polls OpenMeter until a customer has no project attributed, confirming
# that removing a project's last binding actually clears the attribution
# (DesiredCustomer.SubjectKeys is sent as an explicit list, never omitted,
# precisely so this convergence happens on the write side).
#
# OpenMeter's own GET representation of "no subjects" varies: it may omit
# usageAttribution from the response entirely, or return it with an empty
# subjectKeys array — observed directly against a real instance, both the
# very first (never-bound) customer and one that just had its last project
# unbound come back with usageAttribution absent altogether, not `{}` or
# `{"subjectKeys":[]}`. So this checks for the ABSENCE of any populated
# subjectKeys entry, rather than requiring one specific empty-value shape.
#
# Usage: verify-empty-subjects.sh <key>
set -eu

key="$1"
url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/customers/${key}"

for i in $(seq 1 12); do
  body=$(kubectl exec -n openmeter-system openmeter-query -- wget -q -O - "$url" 2>/dev/null || true)
  if ! echo "$body" | grep -q '"subjectKeys":\["'; then
    echo "customer ${key} has no attributed projects: ${body}"
    exit 0
  fi
  sleep 2
done

echo "customer ${key} subjectKeys did not become empty"
echo "last response: ${body}"
exit 1
