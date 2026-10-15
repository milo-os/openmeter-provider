#!/bin/bash
# Spend per meter, from the organization and the project, over the whole
# subscription and from W only (expected amounts in lib.sh), and equal to
# what OpenMeter itself puts on the account's invoice.
set -euo pipefail
. ./lib.sh
. "$TIMES_FILE"

full=$(usage_query Organization "$ORG" "$(spend_spec "$START" "$END")") || fail "organization query failed (HTTP $(usage_status))" "$full"
expect_spend "$full" "$REQUESTS" 5.5 "whole subscription, two rate cards on one meter"
expect_spend "$full" "$EGRESS" 19 "whole subscription, graduated tiers"
expect_total "$full" 24.5 "whole subscription"

late=$(usage_query Organization "$ORG" "$(spend_spec "$W" "$END")") || fail "organization query from W failed (HTTP $(usage_status))" "$late"
expect_spend "$late" "$REQUESTS" 3 "from W"
expect_spend "$late" "$EGRESS" 11 "from W, tiers continuing after the usage before W"
expect_total "$late" 14 "from W"

project=$(usage_query Project "$PROJECT" "$(spend_spec "$START" "$END")") || fail "project query failed (HTTP $(usage_status))" "$project"
expect_spend "$project" "$REQUESTS" 5.5 "in the project"
expect_spend "$project" "$EGRESS" 19 "in the project"
expect_total "$project" 24.5 "in the project"

# Without includeSpend, nothing is priced.
plain=$(usage_query Organization "$ORG" "$(spend_spec "$START" "$END" | jq -c 'del(.includeSpend)')") || fail "query without spend failed" "$plain"
[ "$(echo "$plain" | jq '([.status.meters[] | select(.spend or .spendError)] | length) + (if .status.spend or .status.spendError then 1 else 0 end)')" = "0" ] ||
  fail "spend returned without includeSpend" "$(echo "$plain" | jq -c '.status.meters')"
echo "OK: no spend without includeSpend"

# The same amounts as the lines OpenMeter is gathering for the account's
# next invoice, which covers the whole subscription so far. Its lines are
# named after the ServicePricings' displayNames ("Requests", "Egress").
uid=$(account_uid "$NS_ORG" "$ACCOUNT")
customer=$(om GET "/customers/${uid}" | jq -r .id)
invoices=$(om GET "/billing/invoices?customers=${customer}&statuses=gathering&expand=lines") || fail "could not read the account's invoices" "$invoices"
for m in "Requests|$REQUESTS" "Egress|$EGRESS"; do
  IFS='|' read -r name meter_name <<<"$m"
  line_total=$(echo "$invoices" | jq -r --arg n "$name" \
    '[.items[].lines[]? | select(.name | startswith($n)) | .totals.total | tonumber] | add // 0')
  read -r amount _ <<<"$(spend_of "$full" "$meter_name")"
  num_eq "$line_total" "$amount" ||
    fail "${meter_name}: spend ${amount}, but OpenMeter's gathering invoice has ${line_total}" \
      "$(echo "$invoices" | jq -c '[.items[].lines[]? | {name, quantity, total: .totals.total}]')"
  echo "OK: ${meter_name} spend ${amount} equals OpenMeter's invoice lines"
done
invoice_total=$(echo "$invoices" | jq -r '[.items[].lines[]? | .totals.total | tonumber] | add // 0')
total=$(echo "$full" | jq -r '.status.spend.amount')
num_eq "$invoice_total" "$total" || fail "total spend ${total}, but OpenMeter's gathering invoice totals ${invoice_total}" "$invoices"
echo "OK: total spend ${total} equals OpenMeter's gathering invoice"
