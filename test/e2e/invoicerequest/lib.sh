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

# live_subscription — the customer's one live subscription, as JSON.
live_subscription() {
  om_get "/customers/$(kubectl get billingaccount "$ACCOUNT" -n "$NS" -o jsonpath='{.metadata.uid}')/subscriptions" |
    jq -c '[.items // [] | .[] | select(.status != "inactive")] | if length == 1 then .[0] else error("\(length) live subscriptions") end'
}

# billing_period SUBSCRIPTION_ID — the subscription's current billing period.
billing_period() {
  om_get "/subscriptions/$1" | jq -c '.alignment.currentAlignedBillingPeriod'
}

# invoice_lines — every line of every invoice of the account's customer,
# gathering ones included, with its invoice's status.
invoice_lines() {
  om_get "/billing/invoices?customers=$(customer_id)&expand=lines" |
    jq -c '[.items[]? | .status as $s | .id as $inv | .lines[]? | {invoice: $inv, status: $s, name, total: .totals.total, from: .period.from, to: .period.to}]'
}
