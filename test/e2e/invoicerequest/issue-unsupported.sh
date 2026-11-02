#!/bin/bash
# OpenMeter sends a contract's first invoice itself, so Issue is unsupported.
. ./lib.sh

request e2e-ir-issue Issue >/dev/null
st=$(wait_done e2e-ir-issue)
[ "$(echo "$st" | jq -r '.phase == "Failed" and (.conditions[] | select(.type == "Completed") | .reason) == "Unsupported"')" = "true" ] ||
  fail "Issue wasn't reported as unsupported" "$st"
echo "OK: Issue is unsupported"
