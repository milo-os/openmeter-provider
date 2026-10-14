#!/bin/bash
# Requests the usage API must refuse whatever they ask for.
set -euo pipefail
. ./lib.sh

# A user's own context is not a usage scope.
body=$(usage_query User e2e-usage-viewer "$(spec)") || true
expect_status 403 "$body" "a query in a user's context"

# Authorization is delegated: a user without the permission is refused
# before the server looks at the query.
body=$(usage_query Organization "$ORG_A" "$(spec)" e2e-usage-nobody) || true
expect_status 403 "$body" "a user without permission to create UsageQueries"

# Validation.
body=$(usage_query Organization "$ORG_A" "$(spec '.endTime = .startTime')") || true
expect_status 422 "$body" "an empty time range"
body=$(usage_query Organization "$ORG_A" "$(spec '.meters = ["e2e-uq-no-such-meter"]')") || true
expect_status 400 "$body" "an unknown meter"
body=$(usage_query Organization "$ORG_A" "$(spec '.meters = ["e2e-uq-peak-storage"] | .groupings = [{groupBy: ["region"]}]')") || true
expect_status 400 "$body" "a dimension the meter does not have"
