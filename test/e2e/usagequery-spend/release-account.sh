#!/bin/bash
# Cleanup: the account's usage leaves a draft invoice with a balance when the
# subscription ends, which keeps its customer-link finalizer waiting. The
# force-delete annotation releases it (and unlinks the shared Stripe test
# customer, reused by ../billingaccount/resolve-stripe-customer.sh).
. ./lib.sh

kubectl annotate billingaccount "$ACCOUNT" -n "$NS_ORG" --overwrite \
  openmeter.miloapis.com/force-delete=true >/dev/null 2>&1 || true
