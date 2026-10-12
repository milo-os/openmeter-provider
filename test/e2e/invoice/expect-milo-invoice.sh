#!/bin/bash
# expect-milo-invoice.sh PHASE — the BillingAccount controller projected the
# OpenMeter invoice onto exactly one Milo Invoice, in PHASE, with its
# amounts.
set -euo pipefail
. ./lib.sh

want_phase=$1

invoice=$(the_invoice) || fail "expected exactly one OpenMeter invoice" "$(account_invoices)"
id=$(echo "$invoice" | jq -r .id)
total=$(echo "$invoice" | jq -r .totals.total)

check() {
  resync_account
  sleep 2
  matches=$(kubectl get invoices.billing.miloapis.com -n "$NS" -o json |
    jq -c --arg id "$id" '[.items[] | select(.metadata.annotations["openmeter.miloapis.com/invoiceId"] == $id)]')
  [ "$(echo "$matches" | jq 'length')" = "1" ] || return 1
  obj=$(echo "$matches" | jq -c '.[0]')
  [ "$(echo "$obj" | jq -r .status.phase)" = "$want_phase" ]
}

retry 20 check || fail "no Milo Invoice for OpenMeter invoice ${id} in phase ${want_phase}" "${matches:-[]}"

name=$(echo "$obj" | jq -r .metadata.name)
st=$(echo "$obj" | jq -c .status)
expect() {
  [ "$(echo "$obj" | jq -r "$1")" = "true" ] || fail "Invoice ${name}: $2" "$obj"
}

expect "(.metadata.name | test(\"^${ACCOUNT}-[0-9]{4}-[0-9]{2}-[0-9a-f]{10}\$\"))" "name is not <account>-YYYY-MM-<hash>"
expect ".spec.billingAccountRef.name == \"${ACCOUNT}\"" "billingAccountRef is not ${ACCOUNT}"
expect '.status.currencyCode == "USD"' "currency is not USD"
expect '(.status.conditions // [] | map(select(.type == "Ready")) | .[0].status) == "True"' "Ready is not True"
num_eq "$(echo "$st" | jq -r .total)" "$total" || fail "Invoice ${name}: total is not ${total}" "$st"

case "$want_phase" in
  Open)
    num_eq "$(echo "$st" | jq -r .amountDue)" "$total" || fail "Invoice ${name}: amountDue is not ${total}" "$st"
    num_eq "$(echo "$st" | jq -r .amountPaid)" 0 || fail "Invoice ${name}: amountPaid is not 0" "$st"
    expect '.status.paidAt == null' "paidAt is set on an Open invoice"
    ;;
  Paid)
    num_eq "$(echo "$st" | jq -r .amountPaid)" "$total" || fail "Invoice ${name}: amountPaid is not ${total}" "$st"
    num_eq "$(echo "$st" | jq -r .amountDue)" 0 || fail "Invoice ${name}: amountDue is not 0" "$st"
    expect '.status.paidAt != null' "paidAt is not set"
    ;;
esac
echo "OK: Invoice ${name} is ${want_phase} for ${total} USD (OpenMeter invoice ${id})"
kubectl get invoices.billing.miloapis.com "$name" -n "$NS" -o yaml
