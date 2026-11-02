#!/bin/bash
# Lets the account be deleted although the issued invoice is still open: the
# force-delete annotation releases its finalizer after canceling the
# subscription and unlinking its Stripe customer.
. ./lib.sh
kubectl annotate billingaccount "$ACCOUNT" -n "$NS" --overwrite \
  openmeter.miloapis.com/force-delete=true >/dev/null 2>&1 || true
