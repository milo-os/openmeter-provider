#!/bin/bash
# Failure cleanup: release the account if a failed run left it unable to
# delete (an open invoice keeps its finalizer waiting forever). The
# force-delete annotation also unlinks the customer's Stripe customer:
# OpenMeter links a Stripe customer to one live customer only, and the suite
# reuses its Stripe customer on the next run (stripe-customer.sh).
. ./lib.sh

kubectl annotate billingaccount "$ACCOUNT" -n "$NS" --overwrite \
  openmeter.miloapis.com/force-delete=true >/dev/null 2>&1 || true
