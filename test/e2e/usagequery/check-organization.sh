#!/bin/bash
# Queries made in organization e2e-uq-a read only that organization's
# billing accounts, and can narrow them but never reach organization
# e2e-uq-b.
set -euo pipefail
. ./lib.sh

ORG=Organization

# Everything the organization can see.
body=$(usage_query "$ORG" "$ORG_A" "$(spec)") || fail "organization query failed (HTTP $(usage_status))" "$body"
[ "$(echo "$body" | jq -c .status.scope)" = '{"type":"Organization","name":"e2e-uq-a"}' ] ||
  fail "scope is not organization e2e-uq-a" "$(echo "$body" | jq .status.scope)"
[ "$(echo "$body" | jq -c '[.status.billingAccounts[] | .namespace + "/" + .name]')" = '["organization-e2e-uq-a/e2e-uq-a-main"]' ] ||
  fail "the query covered other billing accounts" "$(echo "$body" | jq .status.billingAccounts)"
num_eq "$(total "$body" "$CPU_METER")" 22 || fail "cpu total should be 22 (web 15 + api 7), without e2e-uq-b's 100" "$(meter "$body" "$CPU_METER")"
[ "$(by "$body" "$CPU_METER" project_name)" = '{"e2e-uq-api":7,"e2e-uq-web":15}' ] ||
  fail "cpu by project should be web 15, api 7, and no shop" "$(by "$body" "$CPU_METER" project_name)"
echo "OK: organization sees its own usage only (cpu 22: web 15, api 7)"

# Daily buckets: yesterday 10, today 12, adding up to the total.
points=$(meter "$body" "$CPU_METER" | jq -c '[.groupings[0].series[0].points[].value | tonumber]')
[ "$points" = "[10,12]" ] || fail "cpu daily points should be [10,12]" "$points"
echo "OK: daily buckets [10,12]"

# Max is the largest value, not a sum.
num_eq "$(total "$body" "$PEAK_METER")" 70 || fail "peak total should be 70 (the max)" "$(meter "$body" "$PEAK_METER")"
[ "$(by "$body" "$PEAK_METER" project_name)" = '{"e2e-uq-api":70,"e2e-uq-web":40}' ] ||
  fail "peak by project should be web 40, api 70" "$(by "$body" "$PEAK_METER" project_name)"
echo "OK: Max meter total is 70"

# Totals only, in another time zone: no buckets, same totals. The time zone
# also checks the image can load IANA zones (it embeds Go's database).
body=$(usage_query "$ORG" "$ORG_A" "$(spec 'del(.granularity) | .timeZone = "America/New_York"')") ||
  fail "totals-only query failed (HTTP $(usage_status))" "$body"
num_eq "$(total "$body" "$CPU_METER")" 22 || fail "totals-only cpu should be 22" "$(meter "$body" "$CPU_METER")"
[ "$(meter "$body" "$CPU_METER" | jq '[.groupings[].series[]?.points // [] | length] | add')" = "0" ] ||
  fail "a totals-only query should return no buckets" "$(meter "$body" "$CPU_METER")"
echo "OK: totals only, in America/New_York"

# A declared dimension.
body=$(usage_query "$ORG" "$ORG_A" "$(spec '.meters = ["e2e-uq-cpu-hours"] | .groupings = [{groupBy: ["region"]}]')") ||
  fail "region query failed (HTTP $(usage_status))" "$body"
[ "$(by "$body" "$CPU_METER" region)" = '{"eu-west1":7,"us-east4":15}' ] ||
  fail "cpu by region should be us-east4 15, eu-west1 7" "$(by "$body" "$CPU_METER" region)"
echo "OK: cpu by region us-east4 15, eu-west1 7"

# Narrowing to a project, including one Milo admitted without its
# organization label (found through spec.ownerRef).
body=$(usage_query "$ORG" "$ORG_A" "$(spec '.projects = ["e2e-uq-web"]')") || fail "project filter failed (HTTP $(usage_status))" "$body"
num_eq "$(total "$body" "$CPU_METER")" 15 || fail "cpu for e2e-uq-web should be 15" "$(meter "$body" "$CPU_METER")"
body=$(usage_query "$ORG" "$ORG_A" "$(spec '.projects = ["e2e-uq-api"]')") || fail "project filter (ownerRef) failed (HTTP $(usage_status))" "$body"
num_eq "$(total "$body" "$CPU_METER")" 7 || fail "cpu for e2e-uq-api should be 7" "$(meter "$body" "$CPU_METER")"
echo "OK: narrowing to a project (web 15, api 7)"

# Narrowing to one of its billing accounts, with or without its namespace.
for ref in '{name: "e2e-uq-a-main"}' '{namespace: "organization-e2e-uq-a", name: "e2e-uq-a-main"}'; do
  body=$(usage_query "$ORG" "$ORG_A" "$(spec ".billingAccounts = [${ref}]")") || fail "account filter ${ref} failed (HTTP $(usage_status))" "$body"
  num_eq "$(total "$body" "$CPU_METER")" 22 || fail "cpu for e2e-uq-a-main should be 22" "$(meter "$body" "$CPU_METER")"
done
echo "OK: narrowing to its billing account"

# Reaching into the other organization is forbidden, not empty.
body=$(usage_query "$ORG" "$ORG_A" "$(spec '.projects = ["e2e-uq-shop"]')") || true
expect_status 403 "$body" "organization e2e-uq-a cannot read e2e-uq-b's project"
body=$(usage_query "$ORG" "$ORG_A" "$(spec '.billingAccounts = [{namespace: "organization-e2e-uq-b", name: "e2e-uq-b-main"}]')") || true
expect_status 403 "$body" "organization e2e-uq-a cannot read e2e-uq-b's billing account"
body=$(usage_query "$ORG" "$ORG_A" "$(spec '.projects = ["e2e-uq-no-such-project"]')") || true
expect_status 403 "$body" "an unknown project answers like another organization's"
body=$(usage_query "$ORG" "$ORG_A" "$(spec '.billingAccounts = [{name: "e2e-uq-no-such-account"}]')") || true
expect_status 404 "$body" "a missing billing account of its own is not found"

# The other organization sees only its own usage.
body=$(usage_query "$ORG" "$ORG_B" "$(spec)") || fail "organization e2e-uq-b query failed (HTTP $(usage_status))" "$body"
num_eq "$(total "$body" "$CPU_METER")" 100 || fail "e2e-uq-b's cpu should be 100" "$(meter "$body" "$CPU_METER")"
num_eq "$(total "$body" "$PEAK_METER")" 999 || fail "e2e-uq-b's peak should be 999" "$(meter "$body" "$PEAK_METER")"
echo "OK: organization e2e-uq-b sees only its own usage (cpu 100)"
