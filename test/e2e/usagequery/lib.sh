#!/bin/bash
# Shared helpers for the usagequery suite. Source from this directory:
#
#   . ./lib.sh
#
# OpenMeter is only reachable inside the cluster, so ingestion goes through
# the curl pod (query-pod.yaml) via `kubectl exec`.
#
# UsageQuery requests go to the cluster's API server, which proxies them to
# the usage API server through its APIService, as Milo does. They are made
# as an impersonated user, with the parent Milo would set for an
# organization or project URL passed as impersonated user extras
# (Impersonate-Extra-*). The API server forwards those extras to the usage
# API server exactly like Milo's own (X-Remote-Extra-*). kubectl cannot set
# impersonated extras, so these calls use curl with the kubeconfig's
# credentials.

NS=openmeter-system
QUERY_POD=openmeter-query-uq
OM_API="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1"
USAGE_API="/apis/usage.billing.miloapis.com/v1alpha1/usagequeries"

ORG_A=e2e-uq-a
ORG_B=e2e-uq-b
NS_A=organization-e2e-uq-a
NS_B=organization-e2e-uq-b
ACCOUNT_A=e2e-uq-a-main
ACCOUNT_B=e2e-uq-b-main
CPU_METER=e2e-uq-cpu-hours
PEAK_METER=e2e-uq-peak-storage

# The user the suite's RBAC (rbac.yaml) lets create UsageQueries.
VIEWER=e2e-usage-viewer

# fail MESSAGE [JSON] — print and exit 1.
fail() {
  echo "ERROR: $1"
  if [ -n "${2:-}" ]; then
    echo "$2" | jq . 2>/dev/null || echo "$2"
  fi
  exit 1
}

# retry TRIES CMD... — run CMD every 3s until it succeeds or TRIES runs out.
retry() {
  local tries=$1
  shift
  for _ in $(seq 1 "$tries"); do
    if "$@"; then
      return 0
    fi
    sleep 3
  done
  return 1
}

# num_eq A B — numeric equality of two decimal strings.
num_eq() {
  awk -v a="$1" -v b="$2" 'BEGIN { exit !(a + 0 == b + 0) }'
}

# om METHOD PATH [BODY] [CONTENT_TYPE] — call the OpenMeter API. Prints the
# body; non-zero unless the status is 2xx.
om() {
  local method=$1 path=$2 body=${3:-} type=${4:-application/json} out status
  local args=(curl -sS -X "$method" -H "Content-Type: ${type}" -w '\n%{http_code}')
  if [ -n "$body" ]; then
    args+=(--data-raw "$body")
  fi
  out=$(kubectl exec -n "$NS" "$QUERY_POD" -- "${args[@]}" "${OM_API}${path}" 2>&1) || true
  status=$(echo "$out" | tail -n 1)
  echo "$out" | sed '$d'
  [[ "$status" =~ ^2[0-9][0-9]$ ]]
}

# account_uid NAMESPACE NAME — a BillingAccount's UID, its OpenMeter customer
# key (and the subject its usage is attributed by).
account_uid() {
  kubectl get billingaccount "$2" -n "$1" -o jsonpath='{.metadata.uid}'
}

# Credentials of the current kubeconfig context, for curl.
KUBE_DIR=$(mktemp -d)
trap 'rm -rf "$KUBE_DIR"' EXIT
KUBE_SERVER=$(kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.server}')
kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d >"$KUBE_DIR/ca.crt"
kubectl config view --raw --minify -o jsonpath='{.users[0].user.client-certificate-data}' | base64 -d >"$KUBE_DIR/client.crt"
kubectl config view --raw --minify -o jsonpath='{.users[0].user.client-key-data}' | base64 -d >"$KUBE_DIR/client.key"
[ -s "$KUBE_DIR/client.crt" ] || fail "the current kubeconfig has no client certificate to impersonate with"

STATUS_FILE="$KUBE_DIR/status"

# usage_query PARENT_TYPE PARENT_NAME SPEC_JSON [USER]
#
# Creates a UsageQuery as USER (default: the viewer) in the given parent:
# Organization or Project with its name, User for a user's context, or ""
# for the platform. Prints the response body; usage_status prints the HTTP
# status. Returns non-zero unless the status is 201.
usage_query() {
  local parent_type=$1 parent_name=$2 spec=$3 user=${4:-$VIEWER} group body out status
  local args=(
    -sS -X POST -w '\n%{http_code}'
    --cacert "$KUBE_DIR/ca.crt" --cert "$KUBE_DIR/client.crt" --key "$KUBE_DIR/client.key"
    -H "Content-Type: application/json"
    -H "Impersonate-User: ${user}"
  )
  if [ -n "$parent_type" ]; then
    group=resourcemanager.miloapis.com
    if [ "$parent_type" = "User" ]; then
      group=iam.miloapis.com
    fi
    # Extra keys contain '/', which header names cannot: they are
    # percent-encoded, as Milo's own forwarded extras are.
    args+=(
      -H "Impersonate-Extra-iam.miloapis.com%2fparent-api-group: ${group}"
      -H "Impersonate-Extra-iam.miloapis.com%2fparent-type: ${parent_type}"
      -H "Impersonate-Extra-iam.miloapis.com%2fparent-name: ${parent_name}"
    )
  fi
  body=$(jq -cn --argjson spec "$spec" '{
    apiVersion: "usage.billing.miloapis.com/v1alpha1",
    kind: "UsageQuery",
    metadata: {name: "e2e"},
    spec: $spec
  }')
  out=$(command curl "${args[@]}" --data-raw "$body" "${KUBE_SERVER}${USAGE_API}" 2>&1) || true
  status=$(echo "$out" | tail -n 1)
  echo "$status" >"$STATUS_FILE"
  echo "$out" | sed '$d'
  [ "$status" = "201" ]
}

usage_status() {
  cat "$STATUS_FILE"
}

# The suite's query window: from the start of the day before yesterday (UTC)
# until an hour from now, so it covers every event ingest.sh sends.
WINDOW_START=$(date -u -v-2d '+%Y-%m-%dT00:00:00Z' 2>/dev/null || date -u -d '2 days ago' '+%Y-%m-%dT00:00:00Z')
WINDOW_END=$(date -u -v+1H '+%Y-%m-%dT%H:00:00Z' 2>/dev/null || date -u -d '1 hour' '+%Y-%m-%dT%H:00:00Z')

# spec [JQ_FILTER] — the suite's base query spec (daily buckets over the
# window, both meters, the total plus breakdowns by project and region),
# with JQ_FILTER applied to it.
spec() {
  jq -cn --arg start "$WINDOW_START" --arg end "$WINDOW_END" \
    --arg cpu "$CPU_METER" --arg peak "$PEAK_METER" "{
      startTime: \$start,
      endTime: \$end,
      granularity: \"Day\",
      meters: [\$cpu, \$peak],
      groupings: [{groupBy: []}, {groupBy: [\"project_name\"]}]
    } | ${1:-.}"
}

# meter RESPONSE METER — one meter's result.
meter() {
  echo "$1" | jq -c --arg m "$2" '.status.meters[] | select(.meterDefinition == $m)'
}

# total RESPONSE METER — the meter's total (the grouping with no groupBy);
# 0 when it has no usage.
total() {
  meter "$1" "$2" | jq -r '[.groupings[] | select((.groupBy // []) == []) | .series[]?.total][0] // "0"'
}

# by RESPONSE METER DIMENSION — the meter's totals broken down by DIMENSION,
# as a JSON object {value: total}.
by() {
  meter "$1" "$2" | jq -c --arg d "$3" '
    [.groupings[] | select(.groupBy == [$d]) | .series[]? | {key: .group[$d], value: (.total | tonumber)}]
    | from_entries'
}

# expect_status WANT RESPONSE WHAT — fail unless the last call returned WANT.
expect_status() {
  local got
  got=$(usage_status)
  [ "$got" = "$1" ] || fail "$3: expected HTTP $1, got $got" "$2"
  echo "OK: $3 (HTTP $1)"
}
