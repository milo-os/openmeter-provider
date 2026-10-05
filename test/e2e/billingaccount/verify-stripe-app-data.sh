#!/bin/sh
# Polls OpenMeter for a customer's Stripe app data and confirms it matches
# the fabricated StripePaymentMethod status (see stripepaymentmethod-active.yaml).
#
# Usage: verify-stripe-app-data.sh <key> <want-stripe-customer-id> <want-stripe-payment-method-id>
set -eu

key="$1"
want_cust="$2"
want_pm="$3"
url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/customers/${key}/apps"

for i in $(seq 1 12); do
  body=$(kubectl exec -n openmeter-system openmeter-query -- wget -q -O - "$url" 2>/dev/null || true)
  ok=1
  echo "$body" | grep -q "\"stripeCustomerId\":\"${want_cust}\"" || ok=0
  echo "$body" | grep -q "\"stripeDefaultPaymentMethodId\":\"${want_pm}\"" || ok=0
  if [ "$ok" = "1" ]; then
    echo "customer ${key} stripe app data converged: ${body}"
    exit 0
  fi
  sleep 2
done

echo "customer ${key} stripe app data did not converge to stripeCustomerId=${want_cust} stripeDefaultPaymentMethodId=${want_pm}"
echo "last response: ${body}"
exit 1
