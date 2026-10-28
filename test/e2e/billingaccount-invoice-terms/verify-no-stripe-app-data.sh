#!/bin/sh
# Confirms the OpenMeter customer exists but has no Stripe app data, and
# still has none after several reconciles: nothing has given it a Stripe
# customer it may use.
#
# Usage: verify-no-stripe-app-data.sh <customer-key>
set -eu
. ./lib.sh

key="$1"

for i in $(seq 1 12); do
  if om_get "/customers/${key}" | grep -q "\"key\":\"${key}\""; then
    break
  fi
  sleep 2
done

for i in $(seq 1 6); do
  body=$(om_get "/customers/${key}/apps")
  if ! echo "$body" | grep -q '"items"'; then
    echo "could not read customer ${key}'s app data: ${body:-<empty>}"
    exit 1
  fi
  if echo "$body" | grep -q '"stripeCustomerId"'; then
    echo "customer ${key} unexpectedly has Stripe app data: ${body}"
    exit 1
  fi
  sleep 2
done
echo "customer ${key} has no Stripe app data: ${body}"
