#!/bin/bash
# Drive the invoice to issued and check it, line by line, in OpenMeter and
# Stripe.
#
# A new invoice waits for the profile's collection interval (PT1H: late usage
# may still arrive), is synced to Stripe as a draft, then waits to be
# approved once its draft period (P0D) is over, which OpenMeter's
# advance-invoices CronJob does every 5 minutes. The suite takes those steps
# itself with the invoice's own actions, whichever OpenMeter offers:
# snapshot-quantities (collect now), approve (what the CronJob would do), or
# advance. Approving finalizes the invoice in Stripe.
set -euo pipefail
. ./lib.sh

want_total=$INVOICE_TOTAL

step() {
  invoice=$(the_invoice) || return 1
  id=$(echo "$invoice" | jq -r .id)
  status=$(echo "$invoice" | jq -r .status)
  case "$status" in
    issued | payment_processing | paid) return 0 ;;
  esac
  if [ "$(echo "$invoice" | jq -r '.statusDetails.failed')" = "true" ]; then
    fail "invoice ${id} failed in ${status}" "$(echo "$invoice" | jq -c '{statusDetails, validationIssues}')"
  fi
  actions=$(echo "$invoice" | jq -c '.statusDetails.availableActions // {}')
  if echo "$actions" | jq -e 'has("snapshotQuantities")' >/dev/null; then
    echo "invoice ${id}: $(echo "$invoice" | jq -r .statusDetails.extendedStatus) -> snapshot-quantities"
    om POST "/billing/invoices/${id}/snapshot-quantities" >/dev/null || echo "  HTTP $(om_status)"
  elif echo "$actions" | jq -e 'has("approve")' >/dev/null; then
    echo "invoice ${id}: $(echo "$invoice" | jq -r .statusDetails.extendedStatus) -> approve"
    om POST "/billing/invoices/${id}/approve" >/dev/null || echo "  HTTP $(om_status)"
  elif echo "$actions" | jq -e 'has("advance")' >/dev/null; then
    echo "invoice ${id}: $(echo "$invoice" | jq -r .statusDetails.extendedStatus) -> advance"
    om POST "/billing/invoices/${id}/advance" >/dev/null || echo "  HTTP $(om_status)"
  else
    echo "invoice ${id}: $(echo "$invoice" | jq -r .statusDetails.extendedStatus), waiting"
  fi
  return 1
}

retry 60 step || fail "invoice was not issued" "$(account_invoices | jq -c '[.[] | {id, status, statusDetails, validationIssues}]')"

total=$(echo "$invoice" | jq -r .totals.total)
currency=$(echo "$invoice" | jq -r .currency)
stripe_invoice_id=$(echo "$invoice" | jq -r '.externalIds.invoicing // empty')
num_eq "$total" "$want_total" || fail "invoice total is ${total}, want ${want_total}" "$(echo "$invoice" | jq -c '{totals, lines: [.lines[]? | {name, totals}]}')"
[ "$currency" = "USD" ] || fail "invoice currency is ${currency}, want USD"
[ -n "$stripe_invoice_id" ] || fail "invoice ${id} has no Stripe invoice id" "$invoice"
echo "OK: invoice ${id} ${status}, total ${total} ${currency}, Stripe invoice ${stripe_invoice_id}"

# One line per ServicePricing, titled with its displayName.
for line in "${LINES[@]}"; do
  IFS='|' read -r _ _ name _ line_total <<<"$line"
  got=$(echo "$invoice" | jq -r --arg n "$name" '[.lines[]? | select(.name == $n) | .totals.total] | if length == 1 then .[0] else "missing" end')
  num_eq "$got" "$line_total" 2>/dev/null ||
    fail "invoice line \"${name}\" is ${got}, want ${line_total}" "$(echo "$invoice" | jq -c '[.lines[]? | {name, total: .totals.total}]')"
done
echo "OK: invoice has the ${#LINES[@]} lines: $(echo "$invoice" | jq -c '[.lines[]? | "\(.name) \(.totals.total)"]')"

# The Stripe invoice is what the customer is charged for.
want_customer=$(kubectl get stripepaymentmethod e2e-inv-payment-method -n "$NS" -o jsonpath='{.status.stripeCustomerId}')
want_cents=$(awk -v t="$want_total" 'BEGIN { printf "%d", t * 100 + 0.5 }')
sinv=$(stripe GET "/invoices/${stripe_invoice_id}")
s_status=$(echo "$sinv" | jq -r .status)
s_customer=$(echo "$sinv" | jq -r .customer)
s_total=$(echo "$sinv" | jq -r .total)
case "$s_status" in
  open | paid) ;;
  *) fail "Stripe invoice ${stripe_invoice_id} is ${s_status}, want open or paid" "$sinv" ;;
esac
[ "$s_customer" = "$want_customer" ] || fail "Stripe invoice is for customer ${s_customer}, want ${want_customer}"
[ "$s_total" = "$want_cents" ] || fail "Stripe invoice total is ${s_total} cents, want ${want_cents}" "$(echo "$sinv" | jq -c '{total, subtotal, tax, lines: [.lines.data[]? | {description, amount}]}')"
echo "OK: Stripe invoice ${stripe_invoice_id} is ${s_status} for ${s_total} cents, customer ${s_customer}"

# What the customer reads on the Stripe invoice: OpenMeter titles each line
# "<displayName>: usage in period (<quantity> x <unit price>)".
slines=$(stripe GET "/invoices/${stripe_invoice_id}/lines?limit=100" | jq -c '[.data[]? | {description, amount}]')
for line in "${LINES[@]}"; do
  IFS='|' read -r _ _ name units line_total <<<"$line"
  cents=$(awk -v t="$line_total" 'BEGIN { printf "%d", t * 100 + 0.5 }')
  echo "$slines" | jq -e --arg p "${name}: usage in period (${units} x " --argjson c "$cents" \
    'map(select((.description | startswith($p)) and .amount == $c)) | length == 1' >/dev/null ||
    fail "Stripe invoice has no line \"${name}: usage in period (${units} x ...)\" for ${cents} cents" "$slines"
done
echo "OK: Stripe invoice lines: $(echo "$slines" | jq -c 'map(.description)')"
