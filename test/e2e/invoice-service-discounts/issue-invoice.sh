#!/bin/bash
# Drive the invoice to issued and check it, line by line, in OpenMeter and
# Stripe. Verifies that 100% discount gets 0 pricing and 15% discount works correctly.
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

# Verify each line in OpenMeter:
for line in "${LINES[@]}"; do
  IFS='|' read -r _ _ name published billed rate line_total discount_pct <<<"$line"
  got=$(echo "$invoice" | jq -c --arg n "$name" '[.lines[]? | select(.name == $n)] | if length == 1 then .[0] else null end')
  [ "$got" != "null" ] || fail "invoice has no single line \"${name}\"" "$(echo "$invoice" | jq -c '[.lines[]? | .name]')"
  summary=$(echo "$got" | jq -c '{quantity, meteredQuantity, total: .totals.total, discountsTotal: .totals.discountsTotal, tiers: [.children[]? | {name, quantity, perUnitAmount}]}')
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
  if [ "$discount_pct" -eq 100 ]; then
    num_eq "$line_total" "0.00" || fail "100% discount line ${name} must be 0 pricing, got ${line_total}"
    num_eq "$(echo "$got" | jq -r '.totals.total')" "0" || fail "100% discount line ${name} OpenMeter total must be 0"
  elif [ "$discount_pct" -gt 0 ]; then
    num_eq "$line_total" "0.85" || fail "15% discount line ${name} must be 0.85, got ${line_total}"
    num_eq "$(echo "$got" | jq -r '.totals.total')" "0.85" || fail "15% discount line ${name} OpenMeter total must be 0.85"
  fi
done
echo "OK: invoice has the ${#LINES[@]} lines: $(echo "$invoice" | jq -c '[.lines[]? | "\(.name) \(.totals.total)"]')"

# Check Stripe invoice:
want_customer=$(kubectl get stripepaymentmethod e2e-inv-sd-payment-method -n "$NS" -o jsonpath='{.status.stripeCustomerId}')
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

# Verify Stripe invoice lines and discount lines:
slines=$(stripe GET "/invoices/${stripe_invoice_id}/lines?limit=100" | jq -c '[.data[]? | {description, amount}]')
for line in "${LINES[@]}"; do
  IFS='|' read -r _ _ name _ billed rate line_total discount_pct <<<"$line"
  cents=$(awk -v t="$line_total" 'BEGIN { printf "%d", t * 100 + 0.5 }')
  echo "$slines" | jq -e --arg p "${name}: " --argjson c "$cents" \
    'map(select(.description | startswith($p)) | .amount) | add == $c' >/dev/null ||
    fail "Stripe invoice lines for \"${name}\" don't add up to ${cents} cents" "$slines"
done
echo "OK: Stripe invoice lines: $(echo "$slines" | jq -c 'map(.description)')"
