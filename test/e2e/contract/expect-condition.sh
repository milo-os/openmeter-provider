#!/bin/bash
# expect-condition.sh REASON [MESSAGE_SUBSTRING]
#
# Polls the entitlement until its OpenMeterSubscriptionSynced condition is
# True with REASON (and, when given, a message containing the substring).
set -euo pipefail
. ./lib.sh

want_reason="$1"
want_message="${2:-}"

check() {
  cond=$(kubectl get billingentitlement "$ENTITLEMENT" -n "$NS" -o json |
    jq -c '.status.conditions // [] | map(select(.type == "OpenMeterSubscriptionSynced")) | .[0] // {}')
  [ "$(echo "$cond" | jq -r '.status')" = "True" ] &&
    [ "$(echo "$cond" | jq -r '.reason')" = "$want_reason" ] &&
    echo "$cond" | jq -r '.message' | grep -qF -- "$want_message"
}

if retry 30 check; then
  echo "OK: OpenMeterSubscriptionSynced=True/${want_reason}"
  exit 0
fi
fail "OpenMeterSubscriptionSynced is not True/${want_reason}${want_message:+ containing \"$want_message\"}" "${cond:-{\}}"
