#!/bin/bash
# stripe-customer.sh <customerId|paymentMethodId>
#
# The account's Stripe customer, built the way stripe-provider builds it
# (stripe-provider internal/stripe/client.go applyCustomerDetails, from the
# BillingAccount's contactInfo): name = businessName (else name), email,
# address, the business/individual names and billing_account in metadata,
# and the card as invoice_settings.default_payment_method. stripe-provider
# does not run in e2e, so this stands in for it. Stripe copies the name,
# email and address onto every invoice as its billing details.
#
# The details are read from billingaccount.yaml, so they are written down
# once. The customer is this suite's own (the other suites share a bare one
# from ../billingaccount/resolve-stripe-customer.sh), cached in /tmp across
# runs and brought up to date on each run rather than recreated.
set -euo pipefail
. ./lib.sh

field=$1
cache=/tmp/e2e-stripe-customer-invoice.env

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
  # Bring the cached customer up to date; start over if it is gone.
  if ! stripe POST "/customers/${customer_id}" "${details[@]}" | jq -e '.id and (.deleted | not)' >/dev/null; then
    customer_id=""
  fi
fi

if [ -z "$customer_id" ]; then
  customer_id=$(stripe POST /customers "${details[@]}" | jq -r '.id // empty')
  [ -n "$customer_id" ] || fail "could not create the Stripe test customer"
  # pm_card_visa is a template: attaching it clones a new card owned by
  # this customer, and that clone's id is the one to use.
  payment_method_id=$(stripe POST /payment_methods/pm_card_visa/attach -d "customer=${customer_id}" | jq -r '.id // empty')
  [ -n "$payment_method_id" ] || fail "could not attach a test card to Stripe customer ${customer_id}"
  printf "customer_id='%s'\npayment_method_id='%s'\n" "$customer_id" "$payment_method_id" >"$cache"
fi

stripe POST "/customers/${customer_id}" -d "invoice_settings[default_payment_method]=${payment_method_id}" >/dev/null

# printf, not echo: chainsaw substitutes this output verbatim, and a
# trailing newline would end up in Stripe request paths.
case "$field" in
  customerId) printf '%s' "$customer_id" ;;
  paymentMethodId) printf '%s' "$payment_method_id" ;;
  *) fail "usage: stripe-customer.sh <customerId|paymentMethodId>" ;;
esac
