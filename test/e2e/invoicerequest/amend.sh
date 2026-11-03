#!/bin/bash
# The contract started today, so an amendment waits for the end of its
# billing period: the subscription stays on the original plan, and the
# entitlement records when the change is due, the end of the period
# OpenMeter reports.
. ./lib.sh

kubectl patch billingentitlement e2e-ir-entitlement -n "$NS" --type=merge \
  --patch '{"spec":{"offerRef":{"name":"e2e-ir-offer-amended"}}}'

today=$(date -u +%Y-%m-%dT00:00:00Z)
customer=$(kubectl get billingaccount "$ACCOUNT" -n "$NS" -o jsonpath='{.metadata.uid}')
original=$(plan_key "$(kubectl get offer e2e-ir-offer -o jsonpath='{.metadata.uid}')")

check() {
  be=$(kubectl get billingentitlement e2e-ir-entitlement -n "$NS" -o json) || return 1
  [ "$(echo "$be" | jq -r '.status.conditions[] | select(.type == "OpenMeterSubscriptionSynced") | .reason')" = "OfferChangeScheduled" ] || return 1
  sub=$(om_get "/customers/${customer}/subscriptions" | jq -c '[.items // [] | .[] | select(.status != "inactive")] | .[0]') || return 1
  period=$(om_get "/subscriptions/$(echo "$sub" | jq -r '.id')" | jq -c '.alignment.currentAlignedBillingPeriod') || return 1
  [ "$(echo "$sub" | jq -r '.plan.key')" = "$original" ] || return 1
  [ "$(echo "$be" | jq -r '.metadata.annotations["openmeter.miloapis.com/contractStartedAt"]')" = "$today" ] || return 1
  due=$(echo "$be" | jq -r '.metadata.annotations["openmeter.miloapis.com/offerChangeAt"]')
  [ "$(date -u -d "$due" +%s 2>/dev/null)" = "$(date -u -d "$(echo "$period" | jq -r '.to')" +%s 2>/dev/null)" ]
}

if retry 30 check; then
  echo "OK: the amendment is due at the end of the billing period ($due); still on the original plan"
  exit 0
fi
fail "the amendment was not deferred to the end of the billing period" \
  "$(jq -n --argjson be "${be:-null}" --argjson sub "${sub:-null}" --argjson period "${period:-null}" \
    '{annotations: $be.metadata.annotations, conditions: $be.status.conditions, subscription: {id: $sub.id, plan: $sub.plan, activeFrom: $sub.activeFrom}, period: $period}')"
