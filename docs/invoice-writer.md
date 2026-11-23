# Invoice Writer

OpenMeter works out each invoice: its lines, usage, discounts and periods.
On its own, its Stripe app then writes the invoice to Stripe. For an
account on the invoice writer, stripe-provider writes it instead, line by
line, through a `StripeInvoice`. Each line is the pricing's own Stripe
Product, has its own tax code, service period and discount, and staff
charges carry their notes.

stripe-provider's side is in
[`docs/architecture/invoicing.md`](https://github.com/milo-os/stripe-provider/blob/main/docs/architecture/invoicing.md).
This is openmeter-provider's side.

- [Turning It On](#turning-it-on)
- [Moving an Account](#moving-an-account)
- [An Invoice, Step by Step](#an-invoice-step-by-step)
- [Lines](#lines)
- [Payment](#payment)
- [When an Invoice Is Held](#when-an-invoice-is-held)
- [What Stays the Same](#what-stays-the-same)
- [Limits](#limits)

## Turning It On

```yaml
invoiceWriter:
  # Unset: every account.
  accountSelector:
    matchLabels:
      billing.miloapis.com/invoice-writer: "true"
  # The ServicePricing a charge staff add without a catalog item is billed under.
  customChargePricing: custom-charge
  # appName: Datum invoice writer
```

With `invoiceWriter` set, the provider installs OpenMeter's Custom Invoicing
app, called `appName`, the first time it reconciles a covered account. It
turns on both of the app's sync hooks before any profile uses it. Without
them, OpenMeter would issue invoices without waiting for us.

stripe-provider must be running with its invoice writer, and each Stripe
webhook endpoint subscribed to the invoice events its README lists.

## Moving an Account

OpenMeter can't change the apps on a billing profile. So a covered account
gets a second profile of its own, on the Custom Invoicing app, tagged in
its metadata with `invoicingApp: <app id>`. The provider then points the
account's customer at it. The account's first profile, on the default
profile's apps, is kept, because its invoices still reference it. An
account that stops being covered goes back to that first profile.

The move takes effect from the next invoice OpenMeter cuts. Usage not yet
invoiced moves with it. Invoices already cut, draft or issued, stay on the
app they were cut with, and the writer leaves them alone.

Once the account's customer is on the new profile, the provider sets the
account's `InvoiceWriterReady` condition to True. billing only accepts
charges from staff on accounts where it is. An account moved back gets
`InvoiceWriterReady=False`.

## An Invoice, Step by Step

With both hooks on, an OpenMeter invoice stops twice on its way to being
issued. The provider lists a covered account's invoices in either stop on
every reconcile. That is every minute while one is waiting, and again
whenever stripe-provider updates one of the account's `StripeInvoice`s.

| OpenMeter | The provider | stripe-provider |
|---|---|---|
| `draft.syncing` | Builds the lines and creates `StripeInvoice` `openmeter-<invoice id>` with `purpose: Invoice` and `source: {system: OpenMeter, invoiceId}`. | Writes the Stripe draft, then reports `Synced`. |
| | Once `Synced` is True for the current lines, and Stripe's total before tax agrees with OpenMeter's, calls `draft/synchronized` with the Stripe invoice id and each line's invoice item id. | |
| `issuing.syncing` | Sets `spec.finalize`. | Finalizes the Stripe invoice. Stripe charges the card or sends the invoice. |
| | Once `Finalized` is True, calls `issuing/synchronized` with Stripe's invoice number. | |
| `payment_processing` and on | Passes payment changes on (see [Payment](#payment)). | Reads the invoice's state from Stripe. |

A draft with nothing to bill is deleted. Stripe needs at least one line,
and nothing is owed.

## Lines

Each of the account's contract and usage lines becomes one Stripe line per
detailed line OpenMeter worked out for it. That is one for a fee, and one
per billed tier of usage.

| `StripeInvoiceLine` | From |
|---|---|
| `key` | The detailed line's id |
| `description` | The detailed line's name |
| `servicePricingRef` | The ServicePricing the rate card's description names (`ServicePricing <name> …`, written by the Offer mapping) |
| `quantity`, `unitAmount` | OpenMeter's, when they multiply out to its amount to the cent. Otherwise 1 × the amount, as for a prorated fee. |
| `amount` | OpenMeter's amount before discounts |
| `period` | OpenMeter's period, its end made inclusive |
| `tax.code` | The detailed line's Stripe tax code, else its line's |
| `discountPercent` | The rate card's percentage discount. OpenMeter records it on each detailed line as an amount discount with reason `ratecard_percentage`. A maximum-spend discount, or two different percentages on one line, holds the invoice. |

A charge staff added becomes one line from its `Charge`, as billing's
[staff charges design](https://github.com/milo-os/billing/blob/main/docs/enhancements/staff-charges.md#from-charge-to-stripe-line)
describes:
- its description, quantity, unit price, tax, discount and note;
- its pricing, or `customChargePricing` for a charge with no catalog item;
- its namespace/name, UID and who added it, in the line's metadata.

Before telling OpenMeter the draft is written, the provider checks Stripe's
subtotal less discounts against OpenMeter's amount less discounts. Tax
isn't compared: Custom Invoicing works out none, and Stripe Tax does.

## Payment

OpenMeter learns nothing from Stripe for these invoices, so the provider
tells it, from the `StripeInvoice`'s state:

| `StripeInvoice` | OpenMeter trigger |
|---|---|
| `Paid` | `paid` |
| `Void` | `void` |
| `Uncollectible` | `payment_uncollectible` |
| `Open`, past its due date | `payment_overdue` |
| `Open`, `PaymentFailed` True | `payment_failed` |

Each trigger is sent once. The last one sent is recorded in the
`openmeter.miloapis.com/payment-trigger` annotation. A trigger OpenMeter
refuses from the invoice's current state is reported in a Warning event and
not sent again.

The projected `Invoice` takes its total from Stripe once the invoice is
issued there. OpenMeter's total leaves out tax for these invoices. Amounts
paid and due, the due date, number and link come from Stripe, as before.

## When an Invoice Is Held

When the provider can't write an invoice, or won't move it on, it emits an
`InvoiceWriterHeld` Warning event on the account saying why. It emits one
again only if the reason changes. Causes:
- a line whose rate card doesn't name its pricing;
- a charge that no longer exists;
- a charge with no catalog item, and no `customChargePricing` set;
- a discount Stripe can't express;
- Stripe's total differing from OpenMeter's;
- more than 250 lines;
- lines changed after the invoice was marked to finalize.

Nothing in OpenMeter times out a hook. An invoice in `draft.syncing` can
only be deleted, and one in `issuing.syncing` can't be voided or deleted.
So the provider also warns about any invoice that has waited on a hook for
more than an hour.

## What Stays the Same

- The `Invoice` projection still matches each OpenMeter invoice to its
  Stripe invoice through the external id. The draft sync now sets that id.
- Stripe-only invoices are still told apart by the `om_invoice_id`
  metadata, which stripe-provider writes from `spec.source`.
- Accounts not covered stay on OpenMeter's Stripe app, untouched. For a
  covered account the provider skips OpenMeter's Stripe customer data,
  which only that app uses.

## Limits

- **The move isn't timed to a billing period.** Usage not yet invoiced
  moves to the new app at the next invoice. With progressive billing, an
  invoice cut mid-period goes on whichever app the account is on at the
  time. Production moves should be timed for just after an account's
  invoice.
- **Changes after the draft sync.** OpenMeter doesn't run the draft sync
  again when an invoice changes later, because its "synced" marker isn't
  cleared. The provider rewrites the Stripe draft while it can. Once the
  invoice is marked to finalize, a change holds it instead.
- **Fees folded into one price.** A contract's quantity is part of its fee's
  flat price in OpenMeter. That fee reads as 1 × the total, with the
  quantity only in its name (for example "Dedicated server × 3").
