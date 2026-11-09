#!/bin/bash
# Replaces the contract starting tomorrow with one starting today. The start
# date can't change and a contract can't be removed, so the entitlement is
# recreated: on pay as you go first, so the account is subscribed, and then
# moved onto the contract, which changes its plan straight away.
. ./lib.sh

kubectl delete billingentitlement e2e-ir-entitlement -n "$NS" --wait=true --timeout=60s
bash entitlement.sh payg
bash expect-payg.sh
bash entitlement.sh today
