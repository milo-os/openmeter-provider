#!/bin/sh
# Polls until the given BillingProfile is deleted in OpenMeter, confirming
# billingProfileFinalizer.Finalize actually reached it. A blank profile_id
# (no default BillingProfile was ever provisioned, so none was ever created
# — see verify-billing-profile.sh) is a no-op success.
#
# OpenMeter SOFT-deletes billing profiles: a deleted profile still answers
# GET with 200 and a `deletedAt` timestamp rather than 404 — confirmed live
# against a profile the finalizer had just removed. That's different from
# customers, whose key-based lookups genuinely do 404 once deleted (see
# verify-deleted.sh), so "the request succeeded" is NOT evidence the
# profile survived. Both shapes are accepted here: deletedAt set, or a
# real not-found.
#
# Usage: verify-billing-profile-deleted.sh <profile-id-or-empty>
set -eu

profile_id="$1"
if [ -z "$profile_id" ]; then
  echo "no billing profile was ever created for this account; nothing to verify"
  exit 0
fi

base="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/billing/profiles"
url="${base}/${profile_id}"

body=""
for i in $(seq 1 8); do
  body="$(kubectl exec -n openmeter-system openmeter-query -- wget -q -O - "$url" 2>/dev/null || true)"

  # Soft-deleted: 200 with a deletedAt timestamp.
  if echo "$body" | jq -e '.deletedAt // empty' >/dev/null 2>&1; then
    echo "billing profile ${profile_id} confirmed deleted from OpenMeter (deletedAt=$(echo "$body" | jq -r '.deletedAt'))"
    exit 0
  fi

  # Empty body means either a genuine 404 (hard delete) or OpenMeter being
  # unreachable. Don't assume "gone" — a broken finalizer would then pass
  # silently. Confirm the API is actually answering before calling it.
  if [ -z "$body" ] && kubectl exec -n openmeter-system openmeter-query -- wget -q -O - "$base" 2>/dev/null | jq -e '.items' >/dev/null 2>&1; then
    echo "billing profile ${profile_id} confirmed gone from OpenMeter (not found)"
    exit 0
  fi

  sleep 2
done

echo "billing profile ${profile_id} is still live in OpenMeter after delete (no deletedAt)"
echo "last response: ${body:-<empty/unreachable>}"
exit 1
