#!/bin/bash
# expect-amendment.sh [MONTH]
#
# An amendment made MONTH months into a started contract (0 by default)
# waits for the next billing date that ends a period of every fee billed in
# advance (schedule.py amendment): changing plan mid-period would charge
# the rest of such a fee's period again. This contract bills Enterprise
# support a year in advance, so that's its next anniversary. The entitlement
# says when (offerChangeAt and its condition), and the subscription stays on
# the contract's first plan until then.
set -euo pipefail
. ./lib.sh

period_end=$(expected e2e-ct-offer amendment "${1:-0}" | jq -r .date)

bash "${CONTRACT_DIR}/expect-condition.sh" OfferChangeScheduled "at the end of its billing period, ${period_end}"

change_at=$(kubectl get billingentitlement "$ENTITLEMENT" -n "$NS" -o jsonpath='{.metadata.annotations.openmeter\.miloapis\.com/offerChangeAt}')
[ "$change_at" = "$period_end" ] || fail "offerChangeAt is \"${change_at}\", want ${period_end}, the end of the billing period"
echo "OK: the amendment takes effect ${change_at}"

# At the contract's start the subscription is still exactly the first
# Offer's (later on, fees that have ended are on it too).
if [ $# -eq 0 ]; then
  bash "${CONTRACT_DIR}/expect-subscription.sh" e2e-ct-offer
fi
