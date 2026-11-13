#!/bin/bash
# What waits on the customer's gathering invoice is the next billing date's
# invoice, a month on: the monthly fees billed in advance for the month
# after, and those billed in arrears for the month just ending. Onboarding,
# whose only period is the first, isn't there.
#
# OpenMeter only lines up charges as far as the next billing date, so a
# quarterly or annual fee billed in arrears isn't waiting yet; the
# time-travel suite (../../timetravel) checks them on the dates they bill.
set -euo pipefail
. ./lib.sh

want=$(expected e2e-ct-offer invoices 1 | jq -c '.[] | select(.month == 1)')
usage=$(usage_names e2e-ct-offer)

check() {
  id=$(customer_id)
  got=$(om GET "/billing/invoices?customers=${id}&statuses=gathering&expand=lines&pageSize=100" | trim_time |
    jq -c --argjson usage "$usage" '[.items[]?.lines[]? | select(.deletedAt == null and (.name | IN($usage[]) | not)) | {name, invoiceAt, to: .period.to}]') || return 1
  problems=$(jq -nc --argjson got "$got" --argjson want "$want" '
    [ ($want.lines | map(.name) | sort) as $wn | ($got | map(.name) | sort) as $gn
      | if $wn != $gn then "waiting: \($gn | join(", ")); want \($wn | join(", "))" else empty end ]
    + [ $want.lines[] as $w | ($got | map(select(.name == $w.name)) | .[0] // empty) as $g
        | if $g.invoiceAt != $want.date or $g.to != $w.to then
            "\($w.name): invoiced \($g.invoiceAt) for the period to \($g.to), want \($want.date) to \($w.to)"
          else empty end ]')
  [ "$problems" = "[]" ]
}

if retry 20 check; then
  echo "OK: waiting for $(echo "$want" | jq -r '.date[0:10]'): $(echo "$want" | jq -r '[.lines[].name] | join(", ")')"
  exit 0
fi
fail "the next billing date's charges are wrong: $(echo "${problems:-[]}" | jq -r 'join("; ")')" "${got:-[]}"
