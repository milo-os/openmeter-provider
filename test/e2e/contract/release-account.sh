#!/bin/bash
# Lets the suite's BillingAccount be deleted although its first invoice is
# still open, which the provider otherwise waits on: it's a test account,
# and another suite needs the Stripe customer it's linked to. Cleanup runs
# this with bash; chainsaw runs scripts with sh, which isn't bash in CI.
. "$(dirname "$0")/lib.sh"
kubectl annotate billingaccount "$ACCOUNT" -n "$NS" --overwrite \
  openmeter.miloapis.com/force-delete=true >/dev/null 2>&1 || true
