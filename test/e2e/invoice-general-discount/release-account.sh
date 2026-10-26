#!/bin/bash
. ./lib.sh

kubectl annotate billingaccount "$ACCOUNT" -n "$NS" --overwrite \
  openmeter.miloapis.com/force-delete=true >/dev/null 2>&1 || true
