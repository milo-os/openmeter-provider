#!/bin/bash
# Entitles the account to the contract Offer with a contract starting today,
# so its charges start now and bill on today's day of the month.
set -euo pipefail
today=$(date -u +%Y-%m-%dT00:00:00Z)
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
    startsAt: "$today"
YAML
