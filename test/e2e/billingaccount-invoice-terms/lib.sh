#!/bin/sh
# Shared by this suite's scripts: GET a path on openmeter-api through the
# suite's query pod.
query_pod="openmeter-query-it"
api="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1"

om_get() {
  kubectl exec -n openmeter-system "$query_pod" -- wget -q -O - "${api}$1" 2>/dev/null || true
}
