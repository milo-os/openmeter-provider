#!/bin/bash
# Pay the invoice in Stripe and deliver Stripe's invoice.paid webhook to
# OpenMeter.
#
# Stripe charges an auto-advancing invoice about an hour after it is
# finalized, so the suite pays it now with the test card. Stripe then sends
# invoice.paid to the webhook OpenMeter registered, but that URL
# (apps.baseURL) is not reachable in a local cluster. The suite relays the
# event itself, as `stripe listen` would: signed with the app's webhook
# secret, which OpenMeter keeps in plain text in its database (secret
# adapter v1.0.0-beta.232). This cannot fake a payment: OpenMeter re-reads
# the invoice from Stripe and only marks it paid if Stripe says it is.
set -euo pipefail
. ./lib.sh

invoice=$(the_invoice) || fail "expected exactly one OpenMeter invoice" "$(account_invoices)"
id=$(echo "$invoice" | jq -r .id)
stripe_invoice_id=$(echo "$invoice" | jq -r '.externalIds.invoicing // empty')
[ -n "$stripe_invoice_id" ] || fail "invoice ${id} has no Stripe invoice id"

sinv=$(stripe GET "/invoices/${stripe_invoice_id}")
if [ "$(echo "$sinv" | jq -r .status)" != "paid" ]; then
  sinv=$(stripe POST "/invoices/${stripe_invoice_id}/pay")
fi
[ "$(echo "$sinv" | jq -r .status)" = "paid" ] || fail "Stripe did not mark invoice ${stripe_invoice_id} paid" "$sinv"
echo "OK: Stripe invoice ${stripe_invoice_id} paid"

# The webhook secret of the Stripe app the invoice was issued through.
app_id=$(om GET "/billing/invoices/${id}?expand=workflow.apps" | jq -r '.workflow.apps.invoicing.id // empty')
[ -n "$app_id" ] || fail "could not resolve the invoicing app of invoice ${id}"
# OpenMeter's bundled PostgreSQL (chart postgresql-16): superuser postgres,
# password in POSTGRES_PASSWORD, OpenMeter's tables in database
# "application".
pg_pod=$(kubectl get pods -n "$NS" -l app.kubernetes.io/name=postgres,app.kubernetes.io/component=primary -o name | head -1)
[ -n "$pg_pod" ] || fail "OpenMeter's PostgreSQL pod not found in ${NS}"
secret=$(kubectl exec -n "$NS" "$pg_pod" -c postgresql -- sh -c '
  export PGPASSWORD="$POSTGRES_PASSWORD"
  psql -h 127.0.0.1 -U postgres -d application -tA \
    -c "select webhook_secret from app_stripes where id = '"'${app_id}'"' and deleted_at is null"' | tr -d '[:space:]')
[ -n "$secret" ] || fail "could not read the webhook secret of Stripe app ${app_id}"

payload=$(jq -c -n --argjson inv "$sinv" --arg evt "evt_e2e_$(date +%s)${RANDOM}" --arg ver "$STRIPE_API_VERSION" \
  '{id: $evt, object: "event", api_version: $ver, created: (now | floor), livemode: false,
    pending_webhooks: 0, request: {id: null, idempotency_key: null},
    type: "invoice.paid", data: {object: $inv}}')
ts=$(date +%s)
sig=$(printf '%s.%s' "$ts" "$payload" | openssl dgst -sha256 -hmac "$secret" | awk '{print $NF}')
resp=$(om POST "/apps/${app_id}/stripe/webhook" "$payload" "Stripe-Signature: t=${ts},v1=${sig}") ||
  fail "OpenMeter rejected the relayed invoice.paid webhook (HTTP $(om_status))" "$resp"
echo "OK: relayed invoice.paid to OpenMeter: ${resp}"

check() {
  status=$(om GET "/billing/invoices/${id}" | jq -r .status)
  [ "$status" = "paid" ]
}
retry 20 check || fail "OpenMeter invoice ${id} is ${status:-unknown}, want paid"
echo "OK: OpenMeter invoice ${id} is paid"
