#!/bin/sh
set -eu

key="$1"
want_name="$2"

url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/plans?key=${key}"

for i in $(seq 1 12); do
  body=$(kubectl exec -n openmeter-system openmeter-query -- wget -q -O - "$url" 2>/dev/null || true)

  ok=1
  # Response is a paginated list. Check if items is not empty and name matches
  echo "$body" | grep -q "\"name\":\"${want_name}\"" || ok=0
  echo "$body" | grep -q "\"key\":\"${key}\"" || ok=0

  if [ "$ok" = "1" ]; then
    echo "plan ${key} converged: ${body}"
    exit 0
  fi
  sleep 2
done

echo "plan ${key} did not converge to name=${want_name}"
echo "last response: ${body}"
exit 1
