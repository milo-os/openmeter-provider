#!/bin/sh
# Polls OpenMeter until the customer's Stripe app data names the published
# Stripe customer with no default payment method: OpenMeter emails the
# invoice rather than charging a card.
#
# Usage: verify-stripe-customer.sh <customer-key> <want-stripe-customer-id>
set -eu
. ./lib.sh

key="$1"
want_cust="$2"

for i in $(seq 1 12); do
  body=$(om_get "/customers/${key}/apps")
  if echo "$body" | grep -q "\"stripeCustomerId\":\"${want_cust}\"" &&
    ! echo "$body" | grep -q "\"stripeDefaultPaymentMethodId\":\"[^\"]"; then
    echo "customer ${key} uses Stripe customer ${want_cust} with no payment method: ${body}"
    exit 0
  fi
  sleep 2
done

echo "customer ${key} stripe app data did not converge to stripeCustomerId=${want_cust} without a payment method"
echo "last response: ${body}"
exit 1
