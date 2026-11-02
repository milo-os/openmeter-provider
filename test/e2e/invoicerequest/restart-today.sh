#!/bin/bash
# Replaces the contract starting tomorrow with one starting today: the start
# date can't change, so the entitlement is recreated.
. ./lib.sh

kubectl delete billingentitlement e2e-ir-entitlement -n "$NS" --wait=true --timeout=60s
bash entitlement.sh today
