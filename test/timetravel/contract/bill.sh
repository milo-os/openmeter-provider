#!/bin/bash
# bill.sh MONTH [AMENDED_AT]
#
# Moves the clock to an hour after the contract's billing date MONTH months
# in, lets everything that runs on time catch up, and checks what is
# invoiced then against schedule.py:
#
#   1. the entitlement is reconciled, as its own requeue would, at the
#      billing date and an hour on: fees whose last period is the current
#      one are scheduled to end, and an amendment that is due is made;
#   2. OpenMeter's subscription-sync job runs, as its CronJob would,
#      bringing the customer's pending lines up to date;
#   3. the customer's pending lines are invoiced (OpenMeter's collect job
#      would, after its collection interval).
#
# The invoices created since the billing date must have exactly the fixed
# fees schedule.py lists for MONTH, each at its total for the period ending
# when it says. Usage lines are left out. With AMENDED_AT, the contract
# moved to e2e-ct-offer-amended at that month.
#
# BILL_REPORT_ONLY=1 prints a month that's billed wrong and carries on, to
# see a whole year's mistakes in one run.
set -euo pipefail
. ./lib.sh

month=$1
amended_at=${2:-}
start=$(starts_at)
date=$(billing_date "$start" "$month")
at=$(plus_seconds "$date" 3600)

if [ -n "$amended_at" ]; then
  amended=$(mktemp)
  kubectl get offer e2e-ct-offer-amended -o json >"$amended"
  want=$(expected e2e-ct-offer invoices "$month" "$amended" "$amended_at" | jq -c --argjson m "$month" '.[] | select(.month == $m)')
else
  want=$(expected e2e-ct-offer invoices "$month" | jq -c --argjson m "$month" '.[] | select(.month == $m)')
fi
[ -n "$want" ] || want='{"lines": [], "total": "0"}'

# The entitlement is reconciled just after the billing date, as its own
# requeue would (it comes back 5s after a deferred change is due), then an
# hour on: an amendment is only made within 90s of its date.
resync() {
  kubectl annotate billingentitlement "$ENTITLEMENT" -n "$NS" --overwrite "e2e.miloapis.com/resync=${month}-$(date +%s)" >/dev/null
  sleep 10
}
echo "Month ${month}: moving to ${date} and then ${at}"
clock to "$(plus_seconds "$date" 5)"
resync
clock to "$at"
resync
echo "Reconciled the entitlement at ${date} and an hour on"

job="tt-sync-${month}-$(date +%s)"
kubectl create job -n "$NS" "$job" --from=cronjob/openmeter-subscription-sync >/dev/null
kubectl wait -n "$NS" --for=condition=complete "job/${job}" --timeout=240s >/dev/null ||
  fail "subscription sync job ${job} did not complete" "$(kubectl logs -n "$NS" "job/${job}" --tail=20 2>&1)"
kubectl delete job -n "$NS" "$job" --wait=false >/dev/null
echo "Synced the subscription (job ${job})"

# OpenMeter can be slow to answer while it catches up on a billing date.
id=""
for _ in $(seq 1 10); do
  id=$(customer_id 2>/dev/null) && [ -n "$id" ] && break
  sleep 3
done
[ -n "$id" ] || fail "no OpenMeter customer for the account"
om POST "/billing/invoices/invoice" "{\"customerId\":\"${id}\"}" >/dev/null ||
  echo "invoicing pending lines: HTTP $(om_status)"

check() {
  invoices=$(account_invoices | trim_time | jq -c --arg since "$date" '[.[] | select(.createdAt >= $since)]')
  got=$(echo "$invoices" | jq -c --argjson usage "$(usage_names e2e-ct-offer)" '[.[].lines[]? | select(.deletedAt == null and (.name | IN($usage[]) | not))
    | {name, total: .totals.total, to: .period.to}] | sort_by(.name)')
  problems=$(jq -nc --argjson got "$got" --argjson want "$want" '
    [ ($want.lines | map(.name) | sort) as $wn | ($got | map(.name) | sort) as $gn
      | if $wn != $gn then "charged \($gn | join(", ")); want \($wn | join(", "))" else empty end ]
    + [ $want.lines[] as $w | ($got | map(select(.name == $w.name)) | .[0] // empty) as $g
        | (if ($g.total | tonumber) != ($w.total | tonumber) then "\($w.name): \($g.total), want \($w.total)" else empty end),
          (if $w.from != $w.to and $g.to != $w.to then "\($w.name): for the period to \($g.to), want to \($w.to)" else empty end) ]')
  [ "$problems" = "[]" ]
}

if retry 20 check; then
  echo "OK: month ${month} (${date}) charges $(echo "$want" | jq -r '[.lines[].name] | join(", ")'), $(echo "$want" | jq -r .total) in all"
  exit 0
fi
if [ -n "${BILL_REPORT_ONLY:-}" ]; then
  echo "MISMATCH: month ${month} (${date}): $(echo "${problems:-[]}" | jq -r 'join("; ")')"
  exit 0
fi
fail "month ${month} (${date}) is billed wrong: $(echo "${problems:-[]}" | jq -r 'join("; ")')" \
  "$(echo "${invoices:-[]}" | jq -c '[.[] | {id, status, createdAt, total: .totals.total, lines: [.lines[]? | select(.deletedAt == null) | {name, type, total: .totals.total, period}]}]')"
