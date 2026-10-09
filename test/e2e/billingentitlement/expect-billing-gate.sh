#!/bin/bash
# expect-billing-gate.sh
#
# Before the account has a payment method, OpenMeter refuses to subscribe it
# when the org's default BillingProfile invoices through a real Stripe app
# (the customer has no Stripe app data). Against a Sandbox app there is no
# such check and the subscription is created straight away, so the
# assertion is skipped.
set -euo pipefail
QUERY_POD=openmeter-query-be
# shellcheck source=../offer/lib.sh
. ../offer/lib.sh

app_id=$(om_get "/billing/profiles?expand=apps" | jq -r '.items[]? | select(.default == true) | .apps.payment.id // empty')
app_type=""
if [ -n "$app_id" ]; then
  app_type=$(om_get "/apps/${app_id}" | jq -r '.type // empty')
fi

if [ "$app_type" != "stripe" ]; then
  echo "SKIP: default billing profile payment app is '${app_type:-none}', not stripe; OpenMeter does not gate subscriptions on Stripe data"
  exit 0
fi
bash expect-condition.sh False CustomerBillingNotReady "default payment method"
