#!/bin/bash
# The amendment's due date passes. OpenMeter's clock can't be moved forward a
# month in e2e, so the due date recorded on the entitlement is moved back
# instead: the provider then takes the path it takes when it missed the
# period boundary (it was down, say), and makes the change straight away.
#
# From then on the account is billed the amended monthly charge, on the same
# billing day, and the setup fee isn't charged again.
. ./lib.sh

today=$(date -u +%Y-%m-%d)
past=$(date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v-1H +%Y-%m-%dT%H:%M:%SZ)
amended=$(plan_key "$(kubectl get offer e2e-ir-offer-amended -o jsonpath='{.metadata.uid}')")

kubectl annotate billingentitlement e2e-ir-entitlement -n "$NS" --overwrite \
  "openmeter.miloapis.com/offerChangeAt=$past" >/dev/null

check() {
  be=$(kubectl get billingentitlement e2e-ir-entitlement -n "$NS" -o json) || return 1
  [ "$(echo "$be" | jq -r '.status.conditions[] | select(.type == "OpenMeterSubscriptionSynced") | "\(.status)/\(.reason)"')" = "True/Synced" ] || return 1
  [ "$(echo "$be" | jq -r '.metadata.annotations["openmeter.miloapis.com/offerChangeAt"] // "none"')" = "none" ] || return 1
  sub=$(live_subscription) || return 1
  [ "$(echo "$sub" | jq -r --arg k "$amended" --arg d "$today" '.status == "active" and .plan.key == $k and .billingAnchor[0:10] == $d')" = "true" ] || return 1
  period=$(billing_period "$(echo "$sub" | jq -r '.id')") || return 1
  lines=$(invoice_lines) || return 1
  [ "$(echo "$lines" | jq -r --arg next "$(echo "$period" | jq -r '.to[0:10]')" '
    (map(select(.name == "Server installation")) | length) == 1
    and (map(select(.status == "gathering" and .name == "Dedicated server" and .from[0:10] == $next)) | length) == 1
    and (map(select(.status == "gathering" and .name == "Dedicated server" and .from[0:10] == $next and (.total | tonumber) == 1200)) | length) == 1')" = "true" ]
}

if retry 45 check; then
  echo "OK: on the amended plan, still billing on $today; next month's fee is 1200 and the setup fee was charged once"
  # What the rest of this month costs depends on OpenMeter's proration,
  # which the provider leaves at its default: shown, not asserted.
  echo "    every line: $(echo "$lines" | jq -c '[.[] | {status, name, total, from, to}]')"
  exit 0
fi
fail "the amended charge isn't what's billed from the next period" \
  "$(jq -n --argjson be "${be:-null}" --argjson sub "${sub:-null}" --argjson period "${period:-null}" --argjson lines "${lines:-null}" \
    '{annotations: $be.metadata.annotations, conditions: $be.status.conditions, subscription: {id: $sub.id, plan: $sub.plan.key, activeFrom: $sub.activeFrom, billingAnchor: $sub.billingAnchor}, period: $period, lines: $lines}')"
