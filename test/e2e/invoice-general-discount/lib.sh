#!/bin/bash
# Shared helpers for the invoice-general-discount suite.
set -euo pipefail

NS=openmeter-system
ACCOUNT=e2e-inv-gd-account
QUERY_POD=openmeter-query-inv-gd
PROJECT=e2e-invoice-gd
OM_API="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1"

# What the customer is billed for, one invoice line each:
#   meter (event type) | OpenMeter meter slug | line name (ServicePricing
#   displayName) | units published | units billed, in the ServicePricing's
#   pricingUnit | price (USD) | line total net of discount (USD) | discount percentage
LINES=(
  "inv-gd-vcpu-hours|inv_gd_vcpu_hours|Compute: vCPU hours|25|25|0.04|0.00|100"
  "inv-gd-storage-gb-months|inv_gd_storage_gb_months|Block storage: GB-months|50|50|0.02|0.00|100"
  "inv-gd-egress-gb|inv_gd_egress_gb|Network egress: GB|10|10|0.09|0.00|100"
  "inv-gd-memory-byte-seconds|inv_gd_memory_byte_seconds|Compute: memory GiB-seconds|53687091200|50|0.01|0.00|100"
  "inv-gd-egress-bytes|inv_gd_egress_bytes|Network egress: GiB, tiered|8589934592|8|2x0,3x0.10,3x0.05|0.00|100"
)
INVOICE_TOTAL=0.00

price_tiers() {
  local billed=$1 price=$2
  case "$price" in
    *x*) ;;
    *) price="${billed}x${price}" ;;
  esac
  echo "$price" | tr ',' '\n' | jq -R -s -c \
    'split("\n") | map(select(. != "") | split("x") | {quantity: (.[0] | tonumber), price: (.[1] | tonumber)})'
}

STRIPE_API_VERSION=2024-09-30.acacia

fail() {
  echo "ERROR: $1"
  if [ -n "${2:-}" ]; then
    echo "$2" | jq . 2>/dev/null || echo "$2"
  fi
  exit 1
}

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

OM_STATUS_FILE=$(mktemp)
om() {
  local method=$1 path=$2 body=${3:-} header=${4:-} out status
  local args=(curl -sS -X "$method" -H "Content-Type: application/json" -w '\n%{http_code}')
  if [ -n "$header" ]; then
    args+=(-H "$header")
  fi
  if [ -n "$body" ]; then
    args+=(--data-raw "$body")
  fi
  out=$(kubectl exec -n "$NS" "$QUERY_POD" -- "${args[@]}" "${OM_API}${path}" 2>&1) || true
  status=$(echo "$out" | tail -n 1)
  echo "$status" >"$OM_STATUS_FILE"
  echo "$out" | sed '$d'
  [[ "$status" =~ ^2[0-9][0-9]$ ]]
}

om_status() {
  cat "$OM_STATUS_FILE"
}

stripe() {
  local method=$1 path=$2
  shift 2
  if [ -z "${STRIPE_TEST_API_KEY:-}" ]; then
    fail "STRIPE_TEST_API_KEY is not set (see .env.example)"
  fi
  command curl -sS -X "$method" -u "${STRIPE_TEST_API_KEY}:" \
    -H "Stripe-Version: ${STRIPE_API_VERSION}" "$@" "https://api.stripe.com/v1${path}"
}

stripe_invoice_page() {
  local t=${1##*/}
  t=${t%%\?*}
  t=${t#*_}
  t=${t:0:$((${#t} / 4 * 4))}
  printf '%s' "$t" | base64 -d 2>/dev/null | LC_ALL=C cut -d, -f1,2 | LC_ALL=C tr -cd '[:print:]'
}

customer_key() {
  kubectl get billingaccount "$ACCOUNT" -n "$NS" -o jsonpath='{.metadata.uid}'
}

customer_id() {
  om GET "/customers/$(customer_key)" | jq -r '.id // empty'
}

account_invoices() {
  local id
  id=$(customer_id)
  [ -n "$id" ] || return 1
  om GET "/billing/invoices?customers=${id}&expand=lines&pageSize=100" |
    jq -c '[.items[]? | select(.status != "gathering" and .deletedAt == null)] | sort_by(.createdAt)'
}

the_invoice() {
  local invoices
  invoices=$(account_invoices) || return 1
  [ "$(echo "$invoices" | jq 'length')" = "1" ] || return 1
  echo "$invoices" | jq -c '.[0]'
}

num_eq() {
  awk -v a="$1" -v b="$2" 'BEGIN { exit !(a + 0 == b + 0) }'
}

resync_account() {
  kubectl annotate billingaccount "$ACCOUNT" -n "$NS" --overwrite \
    "e2e.miloapis.com/resync=$(date +%s)-${RANDOM}" >/dev/null
}
