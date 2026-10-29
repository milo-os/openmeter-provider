#!/bin/bash
# Configuration of the invoice suite. The scripts it runs live in ../invoice
# and read these names; the helpers they share are in ../invoice/common.sh.

ACCOUNT=e2e-inv-account
QUERY_POD=openmeter-query-inv
PROJECT=e2e-invoice
PAYMENT_METHOD=e2e-inv-payment-method
STRIPE_CUSTOMER_CACHE=/tmp/e2e-stripe-customer-invoice.env

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
  "inv-vcpu-hours|inv_vcpu_hours|Compute: vCPU hours|25|25|0.04|1.00|0"
  "inv-storage-gb-months|inv_storage_gb_months|Block storage: GB-months|50|50|0.02|1.00|0"
  "inv-egress-gb|inv_egress_gb|Network egress: GB|10|10|0.09|0.90|0"
  "inv-memory-byte-seconds|inv_memory_byte_seconds|Compute: memory GiB-seconds|53687091200|50|0.01|0.50|0"
  "inv-egress-bytes|inv_egress_bytes|Network egress: GiB, tiered|8589934592|8|2x0,3x0.10,3x0.05|0.45|0"
  "inv-backup-gb|inv_backup_gb|Backup storage: GB|20|20|0.05|0.00|100"
  "inv-api-requests|inv_api_requests|API: requests|100|100|0.01|0.70|30"
)
INVOICE_TOTAL=4.55
CREDIT_AMOUNT=1.00
NET_TOTAL=3.55

. ../invoice/common.sh
