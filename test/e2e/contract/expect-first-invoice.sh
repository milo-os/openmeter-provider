#!/bin/bash
# The contract's first invoice: the one-time fee and every fee billed in
# advance, each for its first period, and nothing billed in arrears.
#
# The provider starts the subscription at the contract's start when it gets
# to it within OpenMeter's tolerance, and then every fee is at its full
# total. Here the contract started at midnight and the suite runs later in
# the day, so the subscription starts now: the provider warns that it
# started late (ContractStartedLate), and OpenMeter prorates each fee billed
# in advance for the part of its first period left. ../../timetravel starts
# one on time.
#
# OpenMeter would invoice it after its collection interval; the suite
# invoices the customer's pending lines now instead, as the invoice suite
# does. If the provider has already done so, the invoice is there.
set -euo pipefail
. ./lib.sh

want=$(expected e2e-ct-offer invoices 0 | jq -c '.[0]')
start=$(starts_at)
from=$(subscription | trim_time | jq -r '.active_from')
late=$(jq -n --arg start "$start" --arg from "$from" '($from | fromdateiso8601) - ($start | fromdateiso8601) > 120')

uid=$(kubectl get billingentitlement "$ENTITLEMENT" -n "$NS" -o jsonpath='{.metadata.uid}')
warned=$(kubectl get events -n "$NS" -o json --field-selector "involvedObject.uid=${uid},reason=ContractStartedLate" | jq '.items | length')
if [ "$late" = true ]; then
  [ "$warned" -ge 1 ] || fail "the subscription started at ${from}, after the contract's start at ${start}, with no ContractStartedLate warning"
  echo "OK: the subscription started late, at ${from}, and the provider warned"
else
  [ "$warned" = 0 ] || fail "ContractStartedLate warning for a subscription that started on time, at ${from}"
  echo "OK: the subscription started at the contract's start"
fi

id=$(customer_id)
[ -n "$id" ] || fail "OpenMeter customer for ${ACCOUNT} not found"
om POST "/billing/invoices/invoice" "{\"customerId\":\"${id}\"}" >/dev/null ||
  echo "invoicing pending lines: HTTP $(om_status) (the invoice may already exist)"

check() {
  invoice=$(account_invoices | jq -c '.[0] // empty' | trim_time)
  [ -n "$invoice" ] || return 1
  got=$(echo "$invoice" | jq -c --argjson usage "$(usage_names e2e-ct-offer)" '[.lines[]? | select(.deletedAt == null and (.name | IN($usage[]) | not))
    | {name, total: .totals.total, from: .period.from, to: .period.to}] | sort_by(.name)')
  # A fee billed in advance is charged for its period from the
  # subscription's start, prorated over its whole first period (OpenMeter
  # rounds to the cent).
  problems=$(jq -nc --argjson got "$got" --argjson want "$want" --arg start "$start" --arg from "$from" '
    def t: fromdateiso8601;
    [ ($want.lines | map(.name) | sort) as $wn | ($got | map(.name) | sort) as $gn
      | if $wn != $gn then "lines \($gn), want \($wn)" else empty end ]
    + [ $want.lines[] as $w | ($got | map(select(.name == $w.name)) | .[0] // empty) as $g
        | ($w.total | tonumber) as $full
        | (if $w.from == $w.to then $full else $full * (($w.to | t) - ($from | t)) / (($w.to | t) - ($start | t)) end) as $due
        | (if (($g.total | tonumber) - $due | fabs) > 0.011 then "\($w.name): \($g.total), want \($due * 100 | round / 100)" else empty end),
          (if $w.from != $w.to and $g.from != $from then "\($w.name): period starts \($g.from), want \($from)" else empty end),
          (if $g.to != $w.to and $w.from != $w.to then "\($w.name): period ends \($g.to), want \($w.to)" else empty end) ]')
  [ "$problems" = "[]" ]
}

if retry 20 check; then
  echo "OK: the first invoice charges $(echo "$want" | jq -r '[.lines[].name] | join(", ")'), $(echo "$want" | jq -r .total) in all"
  exit 0
fi
fail "the first invoice is wrong: $(echo "${problems:-[\"no invoice\"]}" | jq -r 'join("; ")')" \
  "$(echo "${invoice:-{\}}" | jq -c '{id, status, total: .totals.total, lines: [.lines[]? | {name, type, total: .totals.total, period}]}')"
