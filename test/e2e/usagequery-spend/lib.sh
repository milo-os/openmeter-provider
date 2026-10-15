#!/bin/bash
# Shared helpers for the usagequery-spend suite. Source from this directory:
#
#   . ./lib.sh
#
# Reuses the usagequery suite's helpers (usage_query, om, account_uid,
# num_eq, fail, retry) and its UsageQuery viewer RBAC.
. ../usagequery/lib.sh

QUERY_POD=openmeter-query-sp

ORG=e2e-sp
NS_ORG=organization-e2e-sp
PROJECT=e2e-sp-web
ACCOUNT=e2e-sp-account
REQUESTS=e2e-sp-requests
EGRESS=e2e-sp-egress

# Times shared between steps (chainsaw script outputs do not carry over):
# START (the subscription's start, to the second), W (between the two
# batches of usage) and END. Written by ingest.sh.
TIMES_FILE=/tmp/e2e-usagequery-spend-times.env

# The usage ingest.sh sends, in two batches around W, all from PROJECT:
#
#   meter            batch A (before W)        batch B (after W)
#   e2e-sp-requests  us-east4 4, eu-west1 2    us-east4 6
#   e2e-sp-egress    8                         20
#
# Prices (servicepricings.yaml): requests 0.5 in us-east4, 0.25 elsewhere;
# egress graduated, 1 up to 10, 0.5 up to 100, 0.1 above.
#
# Whole subscription: requests 10 x 0.5 + 2 x 0.25 = 5.5
#                     egress   10 x 1 + 18 x 0.5   = 19
# From W only:        requests 6 x 0.5             = 3
#                     egress   continues after A's 8 units:
#                              2 x 1 + 18 x 0.5    = 11 (not 15)
# Total (status.spend):  whole subscription 24.5, from W 14

# spend_spec START END — a spec reading both meters' totals with spend.
spend_spec() {
  jq -cn --arg start "$1" --arg end "$2" --arg r "$REQUESTS" --arg e "$EGRESS" '{
    startTime: $start, endTime: $end, meters: [$r, $e],
    groupings: [{groupBy: []}], includeSpend: true
  }'
}

# spend_of RESPONSE METER — "AMOUNT CURRENCY", or the spendError.
spend_of() {
  echo "$1" | jq -r --arg m "$2" '.status.meters[] | select(.meterDefinition == $m) |
    if .spend then "\(.spend.amount) \(.spend.currency)" else "ERROR: \(.spendError // "no spend")" end'
}

# expect_spend RESPONSE METER AMOUNT WHAT — fail unless METER's spend is
# AMOUNT USD.
expect_spend() {
  local got amount currency
  got=$(spend_of "$1" "$2")
  read -r amount currency <<<"$got"
  if [ "$currency" != "USD" ] || ! num_eq "$amount" "$3"; then
    fail "$4: $2 spend is '$got', want $3 USD" "$(echo "$1" | jq -c '.status.meters')"
  fi
  echo "OK: $4: $2 spend $amount USD"
}

# expect_total RESPONSE AMOUNT WHAT — fail unless status.spend, every meter's
# spend together, is AMOUNT USD.
expect_total() {
  local got amount currency
  got=$(echo "$1" | jq -r 'if .status.spend then "\(.status.spend.amount) \(.status.spend.currency)" else "ERROR: \(.status.spendError // "no spend")" end')
  read -r amount currency <<<"$got"
  if [ "$currency" != "USD" ] || ! num_eq "$amount" "$2"; then
    fail "$3: total spend is '$got', want $2 USD" "$(echo "$1" | jq -c '.status | {spend, spendError}')"
  fi
  echo "OK: $3: total spend $amount USD"
}
