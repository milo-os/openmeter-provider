#!/bin/bash
# Send the suite's usage (the table in lib.sh) straight to OpenMeter, in two
# batches with a boundary W between them, all after the subscription
# started (usage before it is not billed). Records START, W and END in
# TIMES_FILE for the next steps.
set -euo pipefail
. ./lib.sh

uid=$(account_uid "$NS_ORG" "$ACCOUNT")
[ -n "$uid" ] || fail "billing account ${NS_ORG}/${ACCOUNT} has no UID"

subs=$(om GET "/customers/${uid}/subscriptions") || fail "could not list the account's subscriptions" "$subs"
active_from=$(echo "$subs" | jq -r '[.items[] | select(.status == "active")][0].activeFrom // empty')
[ -n "$active_from" ] || fail "the account has no active subscription" "$subs"
# To the second, as OpenMeter bills it.
start=$(echo "$active_from" | sed -E 's/\.[0-9]+//')

now() { date -u '+%Y-%m-%dT%H:%M:%SZ'; }
run="e2e-sp-$(date +%s)-$$"

# event N METER TIME VALUE [REGION]
event() {
  jq -cn --arg id "${run}-$1" --arg type "$2" --arg subject "$uid" --arg time "$3" \
    --arg value "$4" --arg project "$PROJECT" --arg region "${5:-}" '{
      specversion: "1.0", id: $id, source: "/e2e/usagequery-spend", type: $type,
      subject: $subject, time: $time, datacontenttype: "application/json",
      data: {value: $value, dimensions: ({project_name: $project}
        + (if $region == "" then {} else {region: $region} end))}
    }'
}

send() {
  local out
  out=$(om POST /events "$(jq -cs . <<<"$1")" application/cloudevents-batch+json) || fail "OpenMeter rejected the events" "$out"
}

# Whole seconds apart, so START < A < W < B.
sleep 2
a=$(now)
send "$(event 1 "$REQUESTS" "$a" 4 us-east4; event 2 "$REQUESTS" "$a" 2 eu-west1; event 3 "$EGRESS" "$a" 8)"
sleep 2
w=$(now)
sleep 2
b=$(now)
send "$(event 4 "$REQUESTS" "$b" 6 us-east4; event 5 "$EGRESS" "$b" 20)"
end=$(date -u -v+1H '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u -d '1 hour' '+%Y-%m-%dT%H:%M:%SZ')

printf "START=%s\nW=%s\nEND=%s\n" "$start" "$w" "$end" >"$TIMES_FILE"
echo "OK: subscription from ${start}; batch A at ${a}, W=${w}, batch B at ${b}"
