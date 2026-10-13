#!/bin/bash
# Publish every line's usage for project e2e-invoice, the way billing's
# attribution stage emits it (see ../submission/publish-usage.sh).
set -euo pipefail
. ./lib.sh

for line in "${LINES[@]}"; do
  IFS='|' read -r meter _ _ units _ <<<"$line"
  BILLING_ACCOUNT="$ACCOUNT" sh ../submission/publish-usage.sh e2e-invoice "$meter" "$units" >/dev/null
  echo "published ${units} units of ${meter}"
done
