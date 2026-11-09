#!/bin/bash
# entitlement.sh payg|today|tomorrow
#
# payg entitles the account to the pay-as-you-go Offer, with no contract.
# today and tomorrow move it onto the contract Offer, with a contract
# starting on that day (midnight UTC). Applied to an entitlement that
# already exists, that's how an existing account moves onto a contract.
set -euo pipefail
case "$1" in
  payg)
    kubectl apply -n openmeter-system -f - <<YAML
apiVersion: billing.miloapis.com/v1alpha1
kind: BillingEntitlement
metadata:
  name: e2e-ir-entitlement
spec:
  billingAccountRef:
    name: e2e-ir-account
  offerRef:
    name: e2e-ir-payg
YAML
    exit 0 ;;
  today) start=$(date -u +%Y-%m-%dT00:00:00Z) ;;
  tomorrow) start=$(date -u -d '1 day' +%Y-%m-%dT00:00:00Z 2>/dev/null || date -u -v+1d +%Y-%m-%dT00:00:00Z) ;;
  *) echo "usage: entitlement.sh payg|today|tomorrow" >&2; exit 2 ;;
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
