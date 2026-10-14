#!/bin/bash
# Queries made in a project read only that project's usage, on its
# organization's billing accounts, which they do not reveal.
set -euo pipefail
. ./lib.sh

PRJ=Project

body=$(usage_query "$PRJ" e2e-uq-web "$(spec)") || fail "project query failed (HTTP $(usage_status))" "$body"
[ "$(echo "$body" | jq -c .status.scope)" = '{"type":"Project","name":"e2e-uq-web"}' ] ||
  fail "scope is not project e2e-uq-web" "$(echo "$body" | jq .status.scope)"
[ "$(echo "$body" | jq -c '.status.billingAccounts // []')" = "[]" ] ||
  fail "a project query must not list its organization's billing accounts" "$(echo "$body" | jq .status.billingAccounts)"
num_eq "$(total "$body" "$CPU_METER")" 15 || fail "e2e-uq-web's cpu should be 15, not its organization's 22" "$(meter "$body" "$CPU_METER")"
[ "$(by "$body" "$CPU_METER" project_name)" = '{"e2e-uq-web":15}' ] ||
  fail "a project query must only return its own project" "$(by "$body" "$CPU_METER" project_name)"
num_eq "$(total "$body" "$PEAK_METER")" 40 || fail "e2e-uq-web's peak should be 40, not api's 70" "$(meter "$body" "$PEAK_METER")"
echo "OK: project e2e-uq-web sees only its own usage (cpu 15, peak 40)"

# Naming its own project changes nothing.
body=$(usage_query "$PRJ" e2e-uq-web "$(spec '.projects = ["e2e-uq-web"]')") || fail "own project filter failed (HTTP $(usage_status))" "$body"
num_eq "$(total "$body" "$CPU_METER")" 15 || fail "cpu should still be 15" "$(meter "$body" "$CPU_METER")"
echo "OK: naming its own project is allowed"

# A project whose organization is only in spec.ownerRef.
body=$(usage_query "$PRJ" e2e-uq-api "$(spec)") || fail "project e2e-uq-api query failed (HTTP $(usage_status))" "$body"
num_eq "$(total "$body" "$CPU_METER")" 7 || fail "e2e-uq-api's cpu should be 7" "$(meter "$body" "$CPU_METER")"
echo "OK: project e2e-uq-api (organization from ownerRef) sees cpu 7"

# A project of the other organization.
body=$(usage_query "$PRJ" e2e-uq-shop "$(spec)") || fail "project e2e-uq-shop query failed (HTTP $(usage_status))" "$body"
num_eq "$(total "$body" "$CPU_METER")" 100 || fail "e2e-uq-shop's cpu should be 100" "$(meter "$body" "$CPU_METER")"
echo "OK: project e2e-uq-shop sees cpu 100"

body=$(usage_query "$PRJ" e2e-uq-web "$(spec '.projects = ["e2e-uq-api"]')") || true
expect_status 403 "$body" "project e2e-uq-web cannot read e2e-uq-api"
body=$(usage_query "$PRJ" e2e-uq-web "$(spec '.billingAccounts = [{name: "e2e-uq-a-main"}]')") || true
expect_status 403 "$body" "a project query cannot name billing accounts"
