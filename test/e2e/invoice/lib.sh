#!/bin/bash
# Shared helpers for the invoice suite. Source from this directory:
#
#   . ./lib.sh
#
# OpenMeter is only reachable inside the cluster, so its calls go through
# the curl pod (query-pod.yaml) via `kubectl exec`. Stripe is called from
# this machine with STRIPE_TEST_API_KEY (loaded from .env by the Taskfile).
#
# Chainsaw script outputs do not carry over between steps, so every script
# re-derives what it needs (customer, invoice) from the fixed resource names
# below instead of passing ids along.

NS=openmeter-system
ACCOUNT=e2e-inv-account
QUERY_POD=openmeter-query-inv
OM_API="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1"

# What the customer is billed for, one invoice line each:
#   meter (event type) | OpenMeter meter slug | line name (ServicePricing
#   displayName) | units published | units billed, in the ServicePricing's
#   pricingUnit | unit price (USD) | line total (USD)
# Memory is published in byte-seconds and billed per GiB-second (its
# ServicePricing's unitConversion); the others are billed as published.
LINES=(
  "inv-vcpu-hours|inv_vcpu_hours|Compute: vCPU hours|25|25|0.04|1.00"
  "inv-storage-gb-months|inv_storage_gb_months|Block storage: GB-months|50|50|0.02|1.00"
  "inv-egress-gb|inv_egress_gb|Network egress: GB|10|10|0.09|0.90"
  "inv-memory-byte-seconds|inv_memory_byte_seconds|Compute: memory GiB-seconds|53687091200|50|0.01|0.50"
)
INVOICE_TOTAL=3.40

# The API version stripe-go v80 (OpenMeter v1.0.0-beta.232) is pinned to.
# Objects fetched for the relayed webhook are rendered in it.
STRIPE_API_VERSION=2024-09-30.acacia

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

# om METHOD PATH [BODY] [HEADER] — call the OpenMeter API. Prints the body
# and returns non-zero unless the status is 2xx. om_status prints the HTTP
# status of the last call: callers usually run om in a command substitution
# (a subshell), so it is kept in a file rather than a variable.
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

# stripe METHOD PATH [curl args...] — call the Stripe API (test mode).
stripe() {
  local method=$1 path=$2
  shift 2
  if [ -z "${STRIPE_TEST_API_KEY:-}" ]; then
    fail "STRIPE_TEST_API_KEY is not set (see .env.example)"
  fi
  command curl -sS -X "$method" -u "${STRIPE_TEST_API_KEY}:" \
    -H "Stripe-Version: ${STRIPE_API_VERSION}" "$@" "https://api.stripe.com/v1${path}"
}

# stripe_invoice_page URL — which invoice a Stripe hosted invoice URL opens:
# "<account>,<invoice secret>". The URL's token
# (https://invoice.stripe.com/i/<account>/<mode>_<token>) is base64 of
# "<account>,<invoice secret>,<timestamp>" followed by a signature, and
# Stripe signs a new one on every read, so two URLs of one invoice differ
# only after the secret.
stripe_invoice_page() {
  local t=${1##*/}
  t=${t%%\?*}
  t=${t#*_}
  t=${t:0:$((${#t} / 4 * 4))}
  printf '%s' "$t" | base64 -d 2>/dev/null | LC_ALL=C cut -d, -f1,2 | LC_ALL=C tr -cd '[:print:]'
}

# customer_key — the BillingAccount UID (OpenMeter customer key).
customer_key() {
  kubectl get billingaccount "$ACCOUNT" -n "$NS" -o jsonpath='{.metadata.uid}'
}

# customer_id — OpenMeter's internal customer id (invoice APIs need it).
customer_id() {
  om GET "/customers/$(customer_key)" | jq -r '.id // empty'
}

# account_invoices — the customer's invoices except gathering ones, as a
# JSON array (oldest first).
account_invoices() {
  local id
  id=$(customer_id)
  [ -n "$id" ] || return 1
  om GET "/billing/invoices?customers=${id}&expand=lines&pageSize=100" |
    jq -c '[.items[]? | select(.status != "gathering" and .deletedAt == null)] | sort_by(.createdAt)'
}

# the_invoice — the one invoice this suite created. Fails if there is not
# exactly one non-gathering invoice.
the_invoice() {
  local invoices
  invoices=$(account_invoices) || return 1
  [ "$(echo "$invoices" | jq 'length')" = "1" ] || return 1
  echo "$invoices" | jq -c '.[0]'
}

# num_eq A B — numeric equality of two decimal strings.
num_eq() {
  awk -v a="$1" -v b="$2" 'BEGIN { exit !(a + 0 == b + 0) }'
}

# resync_account — make the BillingAccount controller reconcile now (it
# projects invoices on every reconcile, but only resyncs every 10 minutes
# on its own).
resync_account() {
  kubectl annotate billingaccount "$ACCOUNT" -n "$NS" --overwrite \
    "e2e.miloapis.com/resync=$(date +%s)-${RANDOM}" >/dev/null
}
