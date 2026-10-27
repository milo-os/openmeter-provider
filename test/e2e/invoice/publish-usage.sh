#!/bin/bash
# Publish every line's usage for the suite's PROJECT, the way billing's
# attribution stage emits it (see ../submission/publish-usage.sh).
set -euo pipefail
. ./lib.sh

for line in "${LINES[@]}"; do
  IFS='|' read -r meter _ _ units _ _ _ _ <<<"$line"
  BILLING_ACCOUNT="$ACCOUNT" sh ../submission/publish-usage.sh "$PROJECT" "$meter" "$units" >/dev/null
  echo "published ${units} units of ${meter}"
done
