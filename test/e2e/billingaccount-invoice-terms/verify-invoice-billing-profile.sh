#!/bin/sh
# Polls for the account's per-account BillingProfile and confirms it carries
# the Invoice arrangement's terms: invoices are sent rather than charged to
# a card, due after the arrangement's net days (45 in the fixture).
#
# Skips, like ../billingaccount/verify-billing-profile.sh, when this
# OpenMeter instance has no default BillingProfile to derive one from.
#
# Usage: verify-invoice-billing-profile.sh <account-name> <namespace>
set -eu
. ./lib.sh

account="$1"
ns="$2"

if ! om_get "/billing/profiles" | grep -q '"default":true'; then
  echo "SKIP: no default BillingProfile on this OpenMeter instance (ops precondition) — skipping the per-account BillingProfile check"
  exit 0
fi

for i in $(seq 1 12); do
  profile_id=$(kubectl get billingaccount "$account" -n "$ns" -o jsonpath='{.metadata.annotations.openmeter\.miloapis\.com/billing-profile-id}' 2>/dev/null || true)
  if [ -n "$profile_id" ]; then
    body=$(om_get "/billing/profiles/${profile_id}")
    if echo "$body" | grep -q '"collectionMethod":"send_invoice"' &&
      echo "$body" | grep -q '"dueAfter":"P45D"'; then
      echo "billing profile ${profile_id} sends invoices due after 45 days: ${body}"
      exit 0
    fi
  fi
  sleep 2
done

echo "billing profile for ${account} did not converge to send_invoice / P45D (profile_id=${profile_id:-<empty>})"
echo "last response: ${body:-<none>}"
exit 1
