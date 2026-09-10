#!/bin/sh
# resolve-stripe-customer.sh <namespace> <customerId|paymentMethodId>
#
# Decides what stripeCustomerId/stripePaymentMethodId the fabricated
# StripePaymentMethod (see stripepaymentmethod-active.yaml) should carry,
# matching whatever payment app actually backs the org's current default
# BillingProfile:
#
#   - Sandbox app (or no default profile at all): OpenMeter performs no
#     real validation, so a fixed placeholder id is fine.
#   - Real Stripe app (see `task dev:install-stripe-app`): OpenMeter
#     validates the id against the real Stripe account — confirmed live,
#     a placeholder 412s with "stripe customer cus_e2e_test123 not found
#     in stripe account". A real test-mode customer is created instead,
#     with one of Stripe's built-in shared test payment method tokens
#     (pm_card_visa — always valid in test mode, no Checkout/Elements
#     flow needed) attached to it.
#
# This script runs locally (a chainsaw `script` step, not routed through
# `kubectl exec`), so Stripe's public API is directly reachable; only the
# check against OpenMeter itself needs the in-cluster openmeter-query pod.
#
# Cached in /tmp so the two field lookups (one chainsaw script step per
# field) reuse the same created Stripe customer instead of creating two.
set -eu

ns="$1"
field="$2"
cache="/tmp/e2e-stripe-customer-${ns}.env"

resolve() {
  if [ -f "$cache" ]; then
    . "$cache"
    return
  fi

  profiles_body="$(kubectl exec -n openmeter-system openmeter-query -- wget -q -O - \
    "http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/billing/profiles?expand=apps" 2>/dev/null || true)"
  # Distinguish "reached OpenMeter, it has no default profile" (fine — fake
  # ids, nothing validates them) from "couldn't reach OpenMeter at all".
  # Silently treating the latter as the former would hand fake ids to a
  # real-Stripe cluster and surface as a confusing 412 much later instead
  # of failing here with the actual cause.
  if ! echo "$profiles_body" | jq -e '.items' >/dev/null 2>&1; then
    echo "ERROR: could not read billing profiles from OpenMeter via the openmeter-query pod — cannot tell whether the default profile is Stripe-backed." >&2
    echo "       response was: ${profiles_body:-<empty>}" >&2
    exit 1
  fi
  default_payment_app_id="$(echo "$profiles_body" | jq -r '.items[]? | select(.default==true) | .apps.payment.id // empty' 2>/dev/null || true)"

  app_type=""
  if [ -n "$default_payment_app_id" ]; then
    app_body="$(kubectl exec -n openmeter-system openmeter-query -- wget -q -O - \
      "http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/apps/${default_payment_app_id}" 2>/dev/null || true)"
    app_type="$(echo "$app_body" | jq -r '.type // empty' 2>/dev/null || true)"
  fi

  if [ "$app_type" = "stripe" ]; then
    if [ -z "${STRIPE_API_KEY:-}" ]; then
      echo "ERROR: the default BillingProfile's payment app is real Stripe, but STRIPE_API_KEY is not set locally (see .env.example) — cannot create a real test customer to verify against." >&2
      exit 1
    fi
    customer_id="$(command curl -s -u "${STRIPE_API_KEY}:" https://api.stripe.com/v1/customers \
      -d description="openmeter-provider e2e (${ns})" | jq -r '.id')"
    if [ -z "$customer_id" ] || [ "$customer_id" = "null" ]; then
      echo "ERROR: failed to create a real Stripe test customer (check STRIPE_API_KEY)." >&2
      exit 1
    fi
    # pm_card_visa is a *shared template token*, not a PaymentMethod that
    # can belong to anyone: attaching it makes Stripe clone it into a brand
    # new PaymentMethod (pm_1...) owned by this customer, and returns that.
    # The new id is what has to be reported — reporting the template token
    # instead 412s with "stripe payment method pm_card_visa does not belong
    # to stripe customer ..." (confirmed live), since OpenMeter verifies
    # ownership against the real Stripe account.
    payment_method_id="$(command curl -s -u "${STRIPE_API_KEY}:" \
      "https://api.stripe.com/v1/payment_methods/pm_card_visa/attach" \
      -d "customer=${customer_id}" | jq -r '.id')"
    if [ -z "$payment_method_id" ] || [ "$payment_method_id" = "null" ]; then
      echo "ERROR: failed to attach a test payment method to Stripe customer ${customer_id}." >&2
      exit 1
    fi
    {
      echo "STRIPE_CUSTOMER_ID='${customer_id}'"
      echo "STRIPE_PAYMENT_METHOD_ID='${payment_method_id}'"
    } > "$cache"
  else
    {
      echo "STRIPE_CUSTOMER_ID='cus_e2e_test123'"
      echo "STRIPE_PAYMENT_METHOD_ID='pm_e2e_test456'"
    } > "$cache"
  fi

  . "$cache"
}

resolve

# printf, not echo: chainsaw's ($stdout) binding captures this verbatim
# and substitutes it straight into stripepaymentmethod-active.yaml's
# stripeCustomerId/stripePaymentMethodId fields, which OpenMeter's Stripe
# integration then embeds directly into Stripe API request paths. echo's
# trailing newline would ride along into that value and trip Stripe's own
# "forbidden control characters in URL path" check — confirmed live.
case "$field" in
  customerId) printf '%s' "$STRIPE_CUSTOMER_ID" ;;
  paymentMethodId) printf '%s' "$STRIPE_PAYMENT_METHOD_ID" ;;
  *) echo "usage: resolve-stripe-customer.sh <namespace> <customerId|paymentMethodId>" >&2; exit 1 ;;
esac
