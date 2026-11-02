#!/bin/bash
# entitlement.sh today|tomorrow
#
# Entitles the account to the contract Offer with a contract starting on
# that day (midnight UTC).
set -euo pipefail
case "$1" in
  today) start=$(date -u +%Y-%m-%dT00:00:00Z) ;;
  tomorrow) start=$(date -u -d '1 day' +%Y-%m-%dT00:00:00Z 2>/dev/null || date -u -v+1d +%Y-%m-%dT00:00:00Z) ;;
  *) echo "usage: entitlement.sh today|tomorrow" >&2; exit 2 ;;
esac
kubectl apply -n openmeter-system -f - <<YAML
apiVersion: billing.miloapis.com/v1alpha1
kind: BillingEntitlement
metadata:
  name: e2e-ir-entitlement
spec:
  billingAccountRef:
    name: e2e-ir-account
  offerRef:
    name: e2e-ir-offer
  contract:
    startsAt: "$start"
YAML
