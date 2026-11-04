#!/bin/bash
# stripe-invoice.sh <create|decline|void> — an invoice staff create in Stripe
# themselves, for the account's Stripe customer, which OpenMeter never sees.
#
#   create   creates and finalizes a $12.34 invoice sent for payment in 30
#            days, and expects an Open Milo Invoice for it, projected from
#            Stripe, with the Stripe invoice naming it in its metadata.
#   decline  tries to pay it with Stripe's test card that can be attached to
#            a customer but whose every charge is declined, and expects it to
#            stay Open with the whole amount still due. The card is detached
#            again afterwards.
#   void     voids it in Stripe and expects the Milo Invoice to be Void with
#            nothing owed.
#
# auto_advance is off so Stripe neither emails nor charges anyone. The
# invoice's id is kept in /tmp between the two steps (chainsaw script
# outputs do not carry over between steps).
set -euo pipefail
. ./lib.sh

action=$1
cache=/tmp/e2e-stripe-invoice-${ACCOUNT}.id
customer=$(bash stripe-customer.sh customerId)

case "$action" in
  create)
    sinv=$(stripe POST /invoices -d "customer=${customer}" -d collection_method=send_invoice \
      -d days_until_due=30 -d auto_advance=false -d "description=e2e: an invoice created in Stripe")
    id=$(echo "$sinv" | jq -r '.id // empty')
    [ -n "$id" ] || fail "could not create a Stripe invoice" "$sinv"
    printf '%s' "$id" >"$cache"
    item=$(stripe POST /invoiceitems -d "customer=${customer}" -d "invoice=${id}" -d amount=1234 -d currency=usd \
      -d "description=Bare metal setup")
    [ -n "$(echo "$item" | jq -r '.id // empty')" ] || fail "could not add a line to Stripe invoice ${id}" "$item"
    sinv=$(stripe POST "/invoices/${id}/finalize" -d auto_advance=false)
    [ "$(echo "$sinv" | jq -r .status)" = "open" ] || fail "Stripe did not finalize invoice ${id}" "$sinv"
    echo "OK: created and finalized Stripe invoice ${id} ($(echo "$sinv" | jq -r .number))"
    want_phase=Open
    want_due=12.34
    ;;
  decline)
    id=$(cat "$cache")
    # Attaching a test token gives the customer its own copy of the card.
    # Cards declined outright (pm_card_visa_chargeDeclined*) can't be
    # attached, since Stripe checks a card when attaching it; this one
    # (4000 0000 0000 0341) passes that check and fails every charge.
    attached=$(stripe POST /payment_methods/pm_card_chargeCustomerFail/attach -d "customer=${customer}")
    pm=$(echo "$attached" | jq -r '.id // empty')
    [ -n "$pm" ] || fail "could not attach the declining test card to customer ${customer}" "$attached"
    # Stripe answers 402 with the decline; the invoice records the attempt.
    paid=$(stripe POST "/invoices/${id}/pay" -d "payment_method=${pm}")
    echo "Stripe's answer to the payment: $(echo "$paid" | jq -c '.error // {status}')"
    stripe POST "/payment_methods/${pm}/detach" >/dev/null
    sinv=$(stripe GET "/invoices/${id}")
    [ "$(echo "$sinv" | jq -r .status)" = "open" ] || fail "Stripe invoice ${id} is not open after the declined payment" "$sinv"
    [ "$(echo "$sinv" | jq -r '.attempt_count // 0')" -ge 1 ] || fail "Stripe invoice ${id} records no payment attempt" "$sinv"
    echo "OK: payment of Stripe invoice ${id} declined"
    want_phase=Open
    want_due=12.34
    ;;
  void)
    id=$(cat "$cache")
    sinv=$(stripe POST "/invoices/${id}/void")
    [ "$(echo "$sinv" | jq -r .status)" = "void" ] || fail "Stripe did not void invoice ${id}" "$sinv"
    echo "OK: voided Stripe invoice ${id}"
    want_phase=Void
    want_due=0
    ;;
  *)
    fail "usage: stripe-invoice.sh <create|decline|void>"
    ;;
esac

check() {
  resync_account
  sleep 2
  matches=$(kubectl get invoices.billing.miloapis.com -n "$NS" -o json |
    jq -c --arg id "$id" '[.items[] | select(.metadata.annotations["openmeter.miloapis.com/stripeInvoiceId"] == $id)]')
  [ "$(echo "$matches" | jq 'length')" = "1" ] || return 1
  obj=$(echo "$matches" | jq -c '.[0]')
  [ "$(echo "$obj" | jq -r .status.phase)" = "$want_phase" ]
}
retry 20 check || fail "no ${want_phase} Milo Invoice for Stripe invoice ${id}" "${matches:-[]}"

name=$(echo "$obj" | jq -r .metadata.name)
st=$(echo "$obj" | jq -c .status)
expect() {
  [ "$(echo "$obj" | jq -r "$1")" = "true" ] || fail "Invoice ${name}: $2" "$obj"
}
expect "(.metadata.name | test(\"^${ACCOUNT}-[0-9]{4}-[0-9]{2}-[0-9a-f]{10}\$\"))" "name is not <account>-YYYY-MM-<hash>"
expect '.metadata.annotations["openmeter.miloapis.com/invoiceId"] == null' "claims to be an OpenMeter invoice"
expect ".metadata.annotations[\"openmeter.miloapis.com/stripeInvoiceNumber\"] == \"$(echo "$sinv" | jq -r .number)\"" "no Stripe invoice number annotation"
expect '.status.currencyCode == "USD"' "currency is not USD"
expect '(.status.conditions // [] | map(select(.type == "Ready")) | .[0].status) == "True"' "Ready is not True"
expect '(.status.documentUri // "") != ""' "no documentUri"
num_eq "$(echo "$st" | jq -r .total)" 12.34 || fail "Invoice ${name}: total is not 12.34" "$st"
num_eq "$(echo "$st" | jq -r .amountDue)" "$want_due" || fail "Invoice ${name}: amountDue is not ${want_due}" "$st"
if [ "$action" = create ]; then
  expect '.status.dueDate != null' "no dueDate"
  named() {
    resync_account
    sleep 2
    [ "$(stripe GET "/invoices/${id}" | jq -r .metadata.milo_invoice)" = "$name" ]
  }
  retry 20 named || fail "Stripe invoice ${id} does not name Invoice ${name}"
fi
echo "OK: Invoice ${name} is ${want_phase} for Stripe invoice ${id}"
