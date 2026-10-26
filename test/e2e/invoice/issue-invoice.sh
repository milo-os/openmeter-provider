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

# One line per ServicePricing, titled with its displayName, billing the usage
# in the ServicePricing's pricingUnit at its rates: the quantity is the
# published usage converted, and each tier (one for a unit price) bills its
# share of it at the Offer's price, unchanged. OpenMeter only sets
# meteredQuantity (the usage as published) on lines that convert; on the
# others it is the quantity itself.
for line in "${LINES[@]}"; do
  IFS='|' read -r _ _ name published billed rate line_total discount_pct <<<"$line"
  got=$(echo "$invoice" | jq -c --arg n "$name" '[.lines[]? | select(.name == $n)] | if length == 1 then .[0] else null end')
  [ "$got" != "null" ] || fail "invoice has no single line \"${name}\"" "$(echo "$invoice" | jq -c '[.lines[]? | .name]')"
  summary=$(echo "$got" | jq -c '{quantity, meteredQuantity, total: .totals.total, tiers: [.children[]? | {name, quantity, perUnitAmount}]}')
  num_eq "$(echo "$got" | jq -r .totals.total)" "$line_total" ||
    fail "invoice line \"${name}\" total, want ${line_total}" "$summary"
  num_eq "$(echo "$got" | jq -r .quantity)" "$billed" ||
    fail "invoice line \"${name}\" quantity, want ${billed} in the pricing unit" "$summary"
  num_eq "$(echo "$got" | jq -r '.meteredQuantity // .quantity')" "$published" ||
    fail "invoice line \"${name}\" metered quantity, want the ${published} units published" "$summary"
  echo "$got" | jq -e --argjson want "$(price_tiers "$billed" "$rate")" \
    '[.children[]? | {quantity: (.quantity | tonumber), price: (.perUnitAmount | tonumber)}] | sort == ($want | sort)' >/dev/null ||
    fail "invoice line \"${name}\" prices, want ${rate} (units x price per tier) as in the Offer" "$summary"

  # Explicitly verify discount calculations:
  if [ "${discount_pct:-0}" -eq 100 ]; then
    num_eq "$line_total" "0.00" || fail "100% discount line ${name} must be 0 pricing, got ${line_total}"
    num_eq "$(echo "$got" | jq -r '.totals.total')" "0" || fail "100% discount line ${name} OpenMeter total must be 0"
  elif [ "${discount_pct:-0}" -eq 30 ]; then
    num_eq "$line_total" "0.70" || fail "30% discount line ${name} must be 0.70, got ${line_total}"
    num_eq "$(echo "$got" | jq -r '.totals.total')" "0.70" || fail "30% discount line ${name} OpenMeter total must be 0.70"
  fi
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
# "<displayName>: usage in period (<quantity> x <unit price>)", or one line
# per tier, "<displayName>: usage price for tier <n> (<quantity> x <unit
# price>)", the quantity in the ServicePricing's pricingUnit (the prices are
# checked above). A displayName's lines add up to its line total.
slines=$(stripe GET "/invoices/${stripe_invoice_id}/lines?limit=100" | jq -c '[.data[]? | {description, amount}]')
for line in "${LINES[@]}"; do
  IFS='|' read -r _ _ name _ billed rate line_total discount_pct <<<"$line"
  cents=$(awk -v t="$line_total" 'BEGIN { printf "%d", t * 100 + 0.5 }')
  echo "$slines" | jq -e --arg p "${name}: " --argjson c "$cents" \
    'map(select(.description | startswith($p)) | .amount) | add == $c' >/dev/null ||
    fail "Stripe invoice lines for \"${name}\" don't add up to ${cents} cents" "$slines"
  for tier in $(price_tiers "$billed" "$rate" | jq -r '.[] | select(.price > 0) | .quantity'); do
    echo "$slines" | jq -e --arg p "${name}: " --arg q "(${tier} x " \
      'map(select((.description | startswith($p)) and (.description | contains($q)))) | length >= 1' >/dev/null ||
      fail "Stripe invoice has no line \"${name}: ... (${tier} x ...)\"" "$slines"
  done
done
echo "OK: Stripe invoice lines: $(echo "$slines" | jq -c 'map(.description)')"
