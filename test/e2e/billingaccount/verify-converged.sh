#!/bin/sh
# verify-converged.sh <customer-key> <billing-profile-id-or-empty>
#
# Asserts the reconcile loop has actually SETTLED: samples the customer's and
# the per-account BillingProfile's updatedAt, waits long enough for several
# reconciles to run, and fails if either moved.
#
# This exists because two separate drift checks shipped that could never
# converge, each issuing a remote write on every single reconcile forever:
#   - the BillingProfile check compared against an anchored collection
#     alignment that OpenMeter silently rewrites to {type: subscription}, and
#   - the customer check compared a cleared contactInfo.email against a
#     primaryEmail that a replace-PUT omitting the field never actually
#     cleared.
# Both were invisible to every other assertion here, because the end state
# still looked correct — only the endless rewriting was wrong. Anything that
# asserts "the desired value is present" cannot catch that; only watching for
# quiescence can.
set -eu

key="$1"
profile_id="${2:-}"
base="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1"

fetch_updated_at() {
  # $1 = url. Prints the resource's updatedAt, or "" if unreadable.
  kubectl exec -n openmeter-system openmeter-query -- wget -q -O - "$1" 2>/dev/null \
    | sed -n 's/.*"updatedAt":"\([^"]*\)".*/\1/p'
}

customer_url="${base}/customers/${key}"

customer_before="$(fetch_updated_at "$customer_url")"
if [ -z "$customer_before" ]; then
  echo "could not read customer ${key} updatedAt; cannot assert convergence"
  exit 1
fi

profile_before=""
if [ -n "$profile_id" ]; then
  profile_before="$(fetch_updated_at "${base}/billing/profiles/${profile_id}")"
  if [ -z "$profile_before" ]; then
    echo "could not read billing profile ${profile_id} updatedAt; cannot assert convergence"
    exit 1
  fi
fi

# Long enough to span several reconciles. A non-converging drift check
# rewrites on every pass, and every watch event triggers one, so a loop shows
# up quickly — this doesn't need to be proportional to any requeue backoff.
sleep 12

customer_after="$(fetch_updated_at "$customer_url")"
if [ "$customer_before" != "$customer_after" ]; then
  echo "customer ${key} is still being rewritten: updatedAt moved ${customer_before} -> ${customer_after}"
  echo "the customer drift check never converges — EnsureCustomer is issuing an update on every reconcile"
  exit 1
fi

if [ -n "$profile_id" ]; then
  profile_after="$(fetch_updated_at "${base}/billing/profiles/${profile_id}")"
  if [ "$profile_before" != "$profile_after" ]; then
    echo "billing profile ${profile_id} is still being rewritten: updatedAt moved ${profile_before} -> ${profile_after}"
    echo "the billing-profile drift check never converges — EnsureBillingProfile is issuing an update on every reconcile"
    exit 1
  fi
fi

echo "converged: customer${profile_id:+ and billing profile} stopped changing (updatedAt stable at ${customer_before})"
