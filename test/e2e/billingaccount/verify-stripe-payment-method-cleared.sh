#!/bin/sh
# Polls OpenMeter for a customer's Stripe app data and confirms the default
# payment method was cleared while the Stripe customer id was kept.
#
# Usage: verify-stripe-payment-method-cleared.sh <key> <want-stripe-customer-id>
set -eu

key="$1"
want_cust="$2"
url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/customers/${key}/apps"

for i in $(seq 1 12); do
  body=$(kubectl exec -n openmeter-system openmeter-query -- wget -q -O - "$url" 2>/dev/null || true)
  if echo "$body" | grep -q "\"stripeCustomerId\":\"${want_cust}\"" &&
    ! echo "$body" | grep -q "\"stripeDefaultPaymentMethodId\":\"[^\"]"; then
    echo "customer ${key} stripe default payment method cleared: ${body}"
    exit 0
  fi
  sleep 2
done

echo "customer ${key} stripe app data still carries a default payment method (or lost stripeCustomerId=${want_cust})"
echo "last response: ${body}"
exit 1
