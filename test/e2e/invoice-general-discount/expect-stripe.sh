#!/bin/bash
# The suite follows an invoice through Stripe, so it needs the org's
# default billing profile to use the Stripe app (`task dev:install-stripe-app`)
# and STRIPE_TEST_API_KEY to reach the same Stripe account from here.
set -euo pipefail
. ./lib.sh

[ -n "${STRIPE_TEST_API_KEY:-}" ] || fail "STRIPE_TEST_API_KEY is not set (see .env.example)"

profiles=$(om GET "/billing/profiles?expand=apps") || fail "could not read billing profiles (HTTP $(om_status))" "$profiles"
app_id=$(echo "$profiles" | jq -r '.items[]? | select(.default == true) | .apps.invoicing.id // empty')
[ -n "$app_id" ] || fail "OpenMeter has no default billing profile"
app_type=$(om GET "/apps/${app_id}" | jq -r '.type // empty')
[ "$app_type" = "stripe" ] || fail "the default billing profile's invoicing app is '${app_type:-unknown}', not stripe; run task dev:install-stripe-app"
echo "OK: default billing profile invoices through Stripe app ${app_id}"

tax_enabled=$(echo "$profiles" | jq -r '.items[]? | select(.default == true) | .workflow.tax.enabled // false')
if [ "$tax_enabled" = "true" ]; then
  tax=$(stripe GET /tax/settings)
  [ "$(echo "$tax" | jq -r .status)" = "active" ] ||
    fail "the default billing profile calculates tax, but Stripe Tax is not active on the Stripe test account (https://dashboard.stripe.com/test/settings/tax)" \
      "$(echo "$tax" | jq -c '{status, status_details}')"
  echo "OK: Stripe Tax is active"
fi
