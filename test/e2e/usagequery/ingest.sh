#!/bin/bash
# Send the suite's usage straight to OpenMeter's ingest API, skipping NATS
# and the submission consumer (the submission suite covers those). Each
# event looks like one the consumer forwards: its subject is the
# BillingAccount UID (the customer key) and its data carries the value and
# dimensions, project_name included.
#
#   meter             account  project      region     value  when
#   e2e-uq-cpu-hours  a-main   e2e-uq-web   us-east4      10  yesterday
#   e2e-uq-cpu-hours  a-main   e2e-uq-web   us-east4       5  today
#   e2e-uq-cpu-hours  a-main   e2e-uq-api   eu-west1       7  today
#   e2e-uq-cpu-hours  b-main   e2e-uq-shop  us-east4     100  today
#   e2e-uq-peak-...   a-main   e2e-uq-web                 40  yesterday
#   e2e-uq-peak-...   a-main   e2e-uq-api                 70  today
#   e2e-uq-peak-...   b-main   e2e-uq-shop               999  today
set -euo pipefail
. ./lib.sh

uid_a=$(account_uid "$NS_A" "$ACCOUNT_A")
uid_b=$(account_uid "$NS_B" "$ACCOUNT_B")
[ -n "$uid_a" ] && [ -n "$uid_b" ] || fail "billing accounts have no UID yet"

# The controller creates each account's OpenMeter customer; queries filter
# by those customers.
for uid in "$uid_a" "$uid_b"; do
  retry 20 om GET "/customers/${uid}" >/dev/null || fail "OpenMeter customer ${uid} was not created"
done

yesterday=$(date -u -v-26H '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u -d '26 hours ago' '+%Y-%m-%dT%H:%M:%SZ')
today=$(date -u -v-1H '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u -d '1 hour ago' '+%Y-%m-%dT%H:%M:%SZ')
run="e2e-uq-$(date +%s)-$$"

# event N METER SUBJECT TIME VALUE PROJECT [REGION]
event() {
  jq -cn --arg id "${run}-$1" --arg type "$2" --arg subject "$3" --arg time "$4" \
    --arg value "$5" --arg project "$6" --arg region "${7:-}" '{
      specversion: "1.0", id: $id, source: "/e2e/usagequery", type: $type,
      subject: $subject, time: $time, datacontenttype: "application/json",
      data: {value: $value, dimensions: ({project_name: $project}
        + (if $region == "" then {} else {region: $region} end))}
    }'
}

events=$(jq -cs . <<EOF
$(event 1 "$CPU_METER" "$uid_a" "$yesterday" 10 e2e-uq-web us-east4)
$(event 2 "$CPU_METER" "$uid_a" "$today" 5 e2e-uq-web us-east4)
$(event 3 "$CPU_METER" "$uid_a" "$today" 7 e2e-uq-api eu-west1)
$(event 4 "$CPU_METER" "$uid_b" "$today" 100 e2e-uq-shop us-east4)
$(event 5 "$PEAK_METER" "$uid_a" "$yesterday" 40 e2e-uq-web)
$(event 6 "$PEAK_METER" "$uid_a" "$today" 70 e2e-uq-api)
$(event 7 "$PEAK_METER" "$uid_b" "$today" 999 e2e-uq-shop)
EOF
)

out=$(om POST /events "$events" application/cloudevents-batch+json) || fail "OpenMeter rejected the events" "$out"
echo "OK: ingested $(echo "$events" | jq length) events (run ${run})"
