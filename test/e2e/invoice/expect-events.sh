#!/bin/bash
# expect-events.sh <openmeter|stripe> REASON... — the provider recorded the
# activity timeline Events REASON... on the Milo Invoice, each exactly once,
# and no others, with the detail billing's ActivityPolicies read from their
# event.billing.miloapis.com/ annotations (see api/v1alpha1/events.go in
# go.miloapis.com/billing).
#
#   openmeter  the Milo Invoice projected from the suite's OpenMeter invoice
#   stripe     the Milo Invoice projected from the invoice stripe-invoice.sh
#              created in Stripe
#
# To catch duplicates, it resyncs the account a few more times once every
# Event is there and checks nothing new was recorded: a repeated sync must
# not announce a transition again.
set -euo pipefail
. ./lib.sh

source=${1:-}
shift || true
[ "$#" -gt 0 ] || fail "usage: expect-events.sh <openmeter|stripe> REASON..."
want=("$@")

case "$source" in
  openmeter)
    invoice=$(the_invoice) || fail "expected exactly one OpenMeter invoice" "$(account_invoices)"
    key=openmeter.miloapis.com/invoiceId
    id=$(echo "$invoice" | jq -r .id)
    ;;
  stripe)
    key=openmeter.miloapis.com/stripeInvoiceId
    id=$(cat "/tmp/e2e-stripe-invoice-${ACCOUNT}.id")
    ;;
  *)
    fail "usage: expect-events.sh <openmeter|stripe> REASON..."
    ;;
esac

matches=$(kubectl get invoices.billing.miloapis.com -n "$NS" -o json |
  jq -c --arg key "$key" --arg id "$id" '[.items[] | select(.metadata.annotations[$key] == $id)]')
[ "$(echo "$matches" | jq 'length')" = "1" ] || fail "expected one Milo Invoice for ${source} invoice ${id}" "$matches"
name=$(echo "$matches" | jq -r '.[0].metadata.name')

# events — the provider's Events on the Invoice.
events() {
  kubectl get events -n "$NS" \
    --field-selector "involvedObject.kind=Invoice,involvedObject.name=${name}" -o json |
    jq -c '[.items[] | select(.source.component == "openmeter-provider")]'
}

all_recorded() {
  resync_account
  sleep 2
  evs=$(events)
  for reason in "${want[@]}"; do
    [ "$(echo "$evs" | jq --arg r "$reason" '[.[] | select(.reason == $r)] | length')" != "0" ] || return 1
  done
}
retry 20 all_recorded || fail "Invoice ${name}: expected Events ${want[*]}" "${evs:-[]}"

# Nothing more is announced however often the account syncs.
for _ in 1 2 3; do
  resync_account
  sleep 3
done
evs=$(events)
obj=$(kubectl get invoices.billing.miloapis.com "$name" -n "$NS" -o json)

got=$(echo "$evs" | jq -r '[.[].reason] | sort | join(" ")')
expected=$(printf '%s\n' "${want[@]}" | sort | paste -sd' ' -)
[ "$got" = "$expected" ] || fail "Invoice ${name}: Events are [${got}], want [${expected}]" "$evs"

P=event.billing.miloapis.com/
for reason in "${want[@]}"; do
  ev=$(echo "$evs" | jq -c --arg r "$reason" '[.[] | select(.reason == $r)][0]')
  ann() { echo "$ev" | jq -r --arg k "${P}$1" '.metadata.annotations[$k] // empty'; }
  bad() { fail "Invoice ${name}: ${reason} $1" "$ev"; }

  # The recorder folds a repeated identical Event into one with a higher
  # count, so a count above 1 is a duplicate too.
  [ "$(echo "$ev" | jq -r '.count // 1')" = "1" ] || bad "was recorded more than once"

  [ "$(ann billing-account)" = "$ACCOUNT" ] || bad "does not name billing account ${ACCOUNT}"
  [ "$(ann period-start)" = "$(echo "$obj" | jq -r .spec.period.start)" ] || bad "does not carry the invoice's period start"
  [ "$(ann currency)" = "USD" ] || bad "currency is not USD"
  # The suite's namespace is not an organization's, so nothing scopes the
  # Event to a customer timeline.
  [ "$(echo "$ev" | jq -r '.metadata.annotations["platform.miloapis.com/scope.type"] // empty')" = "" ] ||
    bad "is scoped to a tenant outside an organization namespace"
  # Billing writes the wording from the annotations; Stripe's codes and
  # messages must never reach them or the message.
  if echo "$ev" | jq -r '[.message, (.metadata.annotations | tostring)] | join(" ")' |
    grep -qiE 'card_declined|generic_decline|insufficient_funds|your card|in_[A-Za-z0-9]{8,}|pi_[A-Za-z0-9]{8,}'; then
    bad "carries Stripe's own codes, text or ids"
  fi

  case "$reason" in
    InvoiceIssued)
      [ "$(echo "$ev" | jq -r .type)" = "Normal" ] || bad "is not Normal"
      num_eq "$(ann amount)" "$(echo "$obj" | jq -r .status.total)" || bad "amount is not the invoice total"
      due=$(echo "$obj" | jq -r '.status.dueDate // empty')
      [ -z "$due" ] || [ "$(ann due-date)" = "$due" ] || bad "due date is not the invoice's"
      ;;
    InvoicePaymentSucceeded)
      [ "$(echo "$ev" | jq -r .type)" = "Normal" ] || bad "is not Normal"
      num_eq "$(ann amount)" "$(echo "$obj" | jq -r .status.amountPaid)" || bad "amount is not what was paid"
      ;;
    InvoicePaymentFailed)
      [ "$(echo "$ev" | jq -r .type)" = "Warning" ] || bad "is not a Warning"
      [ -n "$(ann amount)" ] || bad "has no amount"
      # stripe-invoice.sh decline pays with Stripe's test card that fails
      # every charge with a generic decline.
      [ "$(ann decline-reason)" = "Declined" ] || bad "decline reason is not Declined"
      ;;
    InvoiceVoided)
      [ "$(echo "$ev" | jq -r .type)" = "Normal" ] || bad "is not Normal"
      ;;
  esac
done
echo "OK: Invoice ${name} has Events [${got}], each once"
echo "$evs" | jq -r '.[] | "  \(.reason): \(.metadata.annotations | with_entries(select(.key | startswith("event.billing"))) | tostring)"'
