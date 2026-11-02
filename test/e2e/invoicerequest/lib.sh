#!/bin/bash
# Shared helpers for the invoicerequest suite.
set -euo pipefail
QUERY_POD=openmeter-query-ir
# shellcheck source=../offer/lib.sh
. ../offer/lib.sh

NS=openmeter-system
ACCOUNT=e2e-ir-account

# customer_id — OpenMeter's internal id for the account's customer.
customer_id() {
  om_get "/customers/$(kubectl get billingaccount "$ACCOUNT" -n "$NS" -o jsonpath='{.metadata.uid}')" | jq -r '.id'
}

# request NAME MODE — creates an InvoiceRequest for the account.
request() {
  kubectl apply -n "$NS" -f - <<YAML
apiVersion: billing.miloapis.com/v1alpha1
kind: InvoiceRequest
metadata:
  name: $1
spec:
  billingAccountRef:
    name: $ACCOUNT
  mode: $2
YAML
}

# wait_done NAME — waits for the request to finish and prints its status.
wait_done() {
  local name=$1 st
  for _ in $(seq 1 60); do
    st=$(kubectl get invoicerequest "$name" -n "$NS" -o json | jq -c '.status // {}')
    case "$(echo "$st" | jq -r '.phase // ""')" in
      Succeeded|Failed) echo "$st"; return 0 ;;
    esac
    sleep 2
  done
  fail "InvoiceRequest $name did not finish" "$st"
}
