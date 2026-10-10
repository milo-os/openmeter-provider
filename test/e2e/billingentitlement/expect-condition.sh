#!/bin/bash
# expect-condition.sh STATUS REASON [MESSAGE_SUBSTRING]
#
# Polls e2e-be-entitlement until its OpenMeterSubscriptionSynced condition has
# STATUS and REASON (and, when given, a message containing the substring).
set -euo pipefail
QUERY_POD=openmeter-query-be
# shellcheck source=../offer/lib.sh
. ../offer/lib.sh

want_status="$1"
want_reason="$2"
want_message="${3:-}"

check() {
  cond=$(kubectl get billingentitlement e2e-be-entitlement -n openmeter-system -o json |
    jq -c '.status.conditions // [] | map(select(.type == "OpenMeterSubscriptionSynced")) | .[0] // {}')
  [ "$(echo "$cond" | jq -r '.status')" = "$want_status" ] &&
    [ "$(echo "$cond" | jq -r '.reason')" = "$want_reason" ] &&
    echo "$cond" | jq -r '.message' | grep -qF -- "$want_message"
}

if retry 45 check; then
  echo "OK: OpenMeterSubscriptionSynced=${want_status}/${want_reason}"
  exit 0
fi
fail "OpenMeterSubscriptionSynced is not ${want_status}/${want_reason}${want_message:+ containing \"$want_message\"}" "${cond:-{\}}"
