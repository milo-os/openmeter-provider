#!/bin/bash
# Configuration of the invoice-general-discount suite. The scripts it runs live in ../invoice
# and read these names; the helpers they share are in ../invoice/common.sh.

ACCOUNT=e2e-inv-gd-account
QUERY_POD=openmeter-query-inv-gd
PROJECT=e2e-invoice-gd
PAYMENT_METHOD=e2e-inv-gd-payment-method
STRIPE_CUSTOMER_CACHE=/tmp/e2e-stripe-customer-invoice-gd.env

# What the customer is billed for, one invoice line each:
#   meter (event type) | OpenMeter meter slug | line name (ServicePricing
#   displayName) | units published | units billed, in the ServicePricing's
#   pricingUnit | price (USD) | line total net of discount (USD) | discount
#   percentage
# The price is a unit price, or for tiered pricing each tier's billed units
# and unit price ("2x0,3x0.10,3x0.05"). Memory is published in byte-seconds
# and billed per GiB-second, tiered egress in bytes and billed per GiB (their
# ServicePricing's unitConversion); the others are billed as published. The
# line total is the price applied to the units billed, less the discount.
LINES=(
  "inv-gd-vcpu-hours|inv_gd_vcpu_hours|Compute: vCPU hours|25|25|0.04|0.00|100"
  "inv-gd-storage-gb-months|inv_gd_storage_gb_months|Block storage: GB-months|50|50|0.02|0.00|100"
  "inv-gd-egress-gb|inv_gd_egress_gb|Network egress: GB|10|10|0.09|0.00|100"
  "inv-gd-memory-byte-seconds|inv_gd_memory_byte_seconds|Compute: memory GiB-seconds|53687091200|50|0.01|0.00|100"
  "inv-gd-egress-bytes|inv_gd_egress_bytes|Network egress: GiB, tiered|8589934592|8|2x0,3x0.10,3x0.05|0.00|100"
)
INVOICE_TOTAL=0.00

. ../invoice/common.sh
