#!/bin/bash
# expect-subscription.sh OFFER
#
# Checks the entitlement's subscription (read through v3) against what
# OFFER's fees should become (schedule.py items):
#
#   - it bills on the contract's day: its billing anchor is the start;
#   - each fixed fee is one item in the current phase, with its cadence
#     (none for a one-time fee), payment term and total;
#   - a fee whose last period ends with the current billing period has its
#     removal scheduled there (the item's active_to); every other fee runs
#     on (no active_to).
set -euo pipefail
. ./lib.sh

offer=${1:-e2e-ct-offer}

want=$(expected "$offer" items)
start=$(starts_at)

check() {
  sub=$(subscription | trim_time) || return 1
  problems=$(echo "$sub" | jq -c --argjson want "$want" --arg start "$start" '
    def t: if . == null then null else sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601 end;
    (.current_period.to // null) as $periodEnd
    # "Now" as OpenMeter sees it, which the time-travel suites move: the
    # start of the current billing period.
    | ((.current_period.from // (now | todate)) | t) as $now
    | [.phases[] | select((.active_from | t) <= $now and ((.active_to | t) == null or (.active_to | t) > $now))]
    | (if length == 1 then .[0].items else [] end) as $items
    | [ (if length != 1 then "\(length) current phases, want 1" else empty end) ]
      + [ $want[] as $w
          | ($items | map(select(.rate_card.name == $w.name))) as $m
          | if ($m | length) != 1 then "\($w.name): \($m | length) items, want 1"
            else $m[0] as $i
            | (if ($i.rate_card.billing_cadence // null) != $w.cadence then "\($w.name): cadence \($i.rate_card.billing_cadence // null), want \($w.cadence)" else empty end),
              (if $i.rate_card.payment_term != $w.paymentTerm then "\($w.name): payment term \($i.rate_card.payment_term), want \($w.paymentTerm)" else empty end),
              (if ($i.rate_card.price.amount | tonumber) != ($w.amount | tonumber) then "\($w.name): amount \($i.rate_card.price.amount), want \($w.amount)" else empty end),
              (if $w.endsAt != null and $w.endsAt == $periodEnd then
                 (if ($i.active_to // null) != $w.endsAt then "\($w.name): ends \($i.active_to // "never"), want \($w.endsAt), the end of this billing period" else empty end)
               else
                 (if ($i.active_to // null) != null then "\($w.name): ends \($i.active_to), want it running on" else empty end)
               end)
            end ]
  ' | jq -c --arg anchor "$(echo "$sub" | jq -r .billing_anchor)" --arg start "$start" \
      'if $anchor != $start then . + ["billing anchor \($anchor), want the contract start \($start)"] else . end')
  [ "$problems" = "[]" ]
}

if retry 20 check; then
  echo "OK: the subscription bills on ${start}'s day and has the $(echo "$want" | jq length) fees of ${offer} as items"
  exit 0
fi
fail "the subscription does not match ${offer}: $(echo "${problems:-[]}" | jq -r 'join("; ")')" \
  "$(echo "${sub:-{\}}" | jq -c '{billing_anchor, current_period, phases: [.phases[]? | {key, active_from, active_to, items: [.items[] | {name: .rate_card.name, cadence: .rate_card.billing_cadence, term: .rate_card.payment_term, amount: .rate_card.price.amount, active_to}]}]}')"
