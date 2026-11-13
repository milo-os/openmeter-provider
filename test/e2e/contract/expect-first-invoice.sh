#!/bin/bash
# The contract's first invoice: the one-time fee and every fee billed in
# advance, each at its full total for its first period, and nothing billed
# in arrears.
#
# OpenMeter would invoice it after its collection interval; the suite
# invoices the customer's pending lines now instead, as the invoice suite
# does. If the provider has already done so, the invoice is there.
set -euo pipefail
. ./lib.sh

want=$(expected e2e-ct-offer invoices 0 | jq -c '.[0]')
start=$(starts_at)

id=$(customer_id)
[ -n "$id" ] || fail "OpenMeter customer for ${ACCOUNT} not found"
om POST "/billing/invoices/invoice" "{\"customerId\":\"${id}\"}" >/dev/null ||
  echo "invoicing pending lines: HTTP $(om_status) (the invoice may already exist)"

check() {
  invoice=$(account_invoices | jq -c '.[0] // empty' | trim_time)
  [ -n "$invoice" ] || return 1
  got=$(echo "$invoice" | jq -c --argjson usage "$(usage_names e2e-ct-offer)" '[.lines[]? | select(.deletedAt == null and (.name | IN($usage[]) | not))
    | {name, total: .totals.total, from: .period.from, to: .period.to}] | sort_by(.name)')
  problems=$(jq -nc --argjson got "$got" --argjson want "$want" --arg start "$start" '
    [ ($want.lines | map(.name) | sort) as $wn | ($got | map(.name) | sort) as $gn
      | if $wn != $gn then "lines \($gn), want \($wn)" else empty end ]
    + [ $want.lines[] as $w | ($got | map(select(.name == $w.name)) | .[0] // empty) as $g
        | (if ($g.total | tonumber) != ($w.total | tonumber) then "\($w.name): \($g.total), want \($w.total)" else empty end),
          (if $g.to != $w.to and $w.from != $w.to then "\($w.name): period ends \($g.to), want \($w.to)" else empty end) ]')
  [ "$problems" = "[]" ]
}

if retry 20 check; then
  echo "OK: the first invoice charges $(echo "$want" | jq -r '[.lines[].name] | join(", ")'), $(echo "$want" | jq -r .total) in all"
  exit 0
fi
fail "the first invoice is wrong: $(echo "${problems:-[\"no invoice\"]}" | jq -r 'join("; ")')" \
  "$(echo "${invoice:-{\}}" | jq -c '{id, status, total: .totals.total, lines: [.lines[]? | {name, type, total: .totals.total, period}]}')"
