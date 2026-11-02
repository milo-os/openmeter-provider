#!/bin/bash
# After the start, a Preview shows what comes next: the first invoice while
# it's still a draft, or else next month's fee.
. ./lib.sh

request e2e-ir-preview-next Preview >/dev/null
st=$(wait_done e2e-ir-preview-next)

ok=$(echo "$st" | jq -r '
  .phase == "Succeeded" and (.lines | length) >= 1
  and (.lines | all(.description == "Server installation" or .description == "Dedicated server"))
  and (.total | tonumber) > 0')
[ "$ok" = "true" ] || fail "unexpected preview after the start" "$st"
echo "OK: $(echo "$st" | jq -r '.conditions[] | select(.type == "Completed") | .message'): $(echo "$st" | jq -c '{total, lines: [.lines[] | {description, amount}]}')"
