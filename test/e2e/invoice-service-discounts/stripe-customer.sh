#!/bin/bash
# stripe-customer.sh <customerId|paymentMethodId>
set -euo pipefail
. ./lib.sh

field=$1
cache=/tmp/e2e-stripe-customer-invoice-sd.env

ci=$(kubectl create --dry-run=client -o json -f billingaccount.yaml | jq -c .spec.contactInfo)
name=$(echo "$ci" | jq -r 'if (.businessName // "") != "" then .businessName else .name end')
details=(
  -d "name=${name}"
  -d "email=$(echo "$ci" | jq -r .email)"
  -d "address[line1]=$(echo "$ci" | jq -r .address.line1)"
  -d "address[city]=$(echo "$ci" | jq -r .address.city)"
  -d "address[state]=$(echo "$ci" | jq -r .address.region)"
  -d "address[postal_code]=$(echo "$ci" | jq -r .address.postalCode)"
  -d "address[country]=$(echo "$ci" | jq -r .address.country)"
  -d "metadata[billing_account]=${ACCOUNT}"
  -d "metadata[business_name]=$(echo "$ci" | jq -r '.businessName // ""')"
  -d "metadata[individual_name]=$(echo "$ci" | jq -r '.name // ""')"
)

customer_id=""
payment_method_id=""
if [ -f "$cache" ]; then
  . "$cache"
  if ! stripe POST "/customers/${customer_id}" "${details[@]}" | jq -e '.id and (.deleted | not)' >/dev/null; then
    customer_id=""
  fi
fi

if [ -z "$customer_id" ]; then
  customer_id=$(stripe POST /customers "${details[@]}" | jq -r '.id // empty')
  [ -n "$customer_id" ] || fail "could not create the Stripe test customer"
  payment_method_id=$(stripe POST /payment_methods/pm_card_visa/attach -d "customer=${customer_id}" | jq -r '.id // empty')
  [ -n "$payment_method_id" ] || fail "could not attach a test card to Stripe customer ${customer_id}"
  printf "customer_id='%s'\npayment_method_id='%s'\n" "$customer_id" "$payment_method_id" >"$cache"
fi

stripe POST "/customers/${customer_id}" -d "invoice_settings[default_payment_method]=${payment_method_id}" >/dev/null

case "$field" in
  customerId) printf '%s' "$customer_id" ;;
  paymentMethodId) printf '%s' "$payment_method_id" ;;
  *) fail "usage: stripe-customer.sh <customerId|paymentMethodId>" ;;
esac
