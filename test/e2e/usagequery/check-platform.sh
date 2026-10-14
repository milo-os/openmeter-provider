#!/bin/bash
# Platform queries (no organization or project) can read any billing
# account, but must say which.
set -euo pipefail
. ./lib.sh

body=$(usage_query "" "" "$(spec)") || true
expect_status 400 "$body" "a platform query without billingAccounts or projects"
body=$(usage_query "" "" "$(spec '.billingAccounts = [{name: "e2e-uq-a-main"}]')") || true
expect_status 400 "$body" "a platform query naming an account without its namespace"

body=$(usage_query "" "" "$(spec '.billingAccounts = [{namespace: "organization-e2e-uq-b", name: "e2e-uq-b-main"}]')") ||
  fail "platform account query failed (HTTP $(usage_status))" "$body"
[ "$(echo "$body" | jq -c .status.scope)" = '{"type":"Platform"}' ] || fail "scope is not the platform" "$(echo "$body" | jq .status.scope)"
num_eq "$(total "$body" "$CPU_METER")" 100 || fail "e2e-uq-b-main's cpu should be 100" "$(meter "$body" "$CPU_METER")"
echo "OK: platform reads a named billing account (cpu 100)"

# By project: the accounts of the project's organization, filtered to it.
body=$(usage_query "" "" "$(spec '.projects = ["e2e-uq-web"]')") || fail "platform project query failed (HTTP $(usage_status))" "$body"
num_eq "$(total "$body" "$CPU_METER")" 15 || fail "e2e-uq-web's cpu should be 15" "$(meter "$body" "$CPU_METER")"
[ "$(echo "$body" | jq -c '[.status.billingAccounts[] | .name]')" = '["e2e-uq-a-main"]' ] ||
  fail "a platform project query should cover the project's organization's accounts" "$(echo "$body" | jq .status.billingAccounts)"
echo "OK: platform reads a project's usage (cpu 15)"

# Projects of two organizations at once.
body=$(usage_query "" "" "$(spec '.projects = ["e2e-uq-web", "e2e-uq-shop"]')") || fail "platform two-project query failed (HTTP $(usage_status))" "$body"
num_eq "$(total "$body" "$CPU_METER")" 115 || fail "web + shop cpu should be 115" "$(meter "$body" "$CPU_METER")"
[ "$(by "$body" "$CPU_METER" project_name)" = '{"e2e-uq-shop":100,"e2e-uq-web":15}' ] ||
  fail "cpu by project should be web 15, shop 100" "$(by "$body" "$CPU_METER" project_name)"
echo "OK: platform reads projects across organizations (cpu 115)"
