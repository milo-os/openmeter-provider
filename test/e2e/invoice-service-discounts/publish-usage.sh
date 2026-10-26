#!/bin/bash
set -euo pipefail
. ./lib.sh

for line in "${LINES[@]}"; do
  IFS='|' read -r meter _ _ units _ _ _ _ <<<"$line"
  BILLING_ACCOUNT="$ACCOUNT" sh ../submission/publish-usage.sh "$PROJECT" "$meter" "$units" >/dev/null
  echo "published ${units} units of ${meter}"
done
