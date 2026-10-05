# TODO

Tracks work deliberately deferred during the amberflo-provider → openmeter-provider
migration, so it isn't lost once the controller-by-controller pass is done.

Migration is happening controller by controller, each ported from the equivalent
`amberflo-provider/internal/controller/*.go`, with e2e coverage before moving to the
next one. This file exists so scope deliberately left out of a given pass has a home to
land in later, instead of getting silently forgotten.

## Controller migration status

| Controller | Status | Notes |
|---|---|---|
| `MeterDefinitionReconciler` | ✅ Done | All 7 `MeterAggregation` values covered; e2e in `test/e2e/meterdefinition/`. |
| `BillingAccountReconciler` | ✅ Done (full parity, minus two genuinely-inapplicable fields) | Customer sync, invoice sync, Stripe app-data sync, and per-account BillingProfile sync (paymentTerms) all implemented. See "BillingAccount: what's still not synced" below for the two fields with no OpenMeter home at all. e2e in `test/e2e/billingaccount/`. |
| `OfferReconciler` | ⏳ Not started | Amberflo: `amberflo-provider/internal/controller/offer_controller.go`. Syncs `Offer` → Amberflo Product Plan; watches `meterdefinitions`. Depends on `MeterDefinitionReconciler` (done), so this is next. |
| `BillingEntitlementReconciler` | ⏳ Not started | Amberflo: `amberflo-provider/internal/controller/billingentitlement_controller.go`. Syncs `BillingEntitlement` → Amberflo customer-plan assignment. Depends on **both** `BillingAccountReconciler` and `OfferReconciler` (customer + product plan must exist first) — do this last. |

## BillingAccount: what's synced, and what's genuinely not

`BillingAccountReconciler` now covers everything Amberflo's own
`BillingAccountReconciler` does, adapted to OpenMeter's (simpler, in two of the three
cases) model:

- **Customer sync** — name (BusinessName → ContactInfo.Name → "namespace/name"
  fallback), email, currency, billing address, and project-derived usage attribution.
- **Invoice sync** (`internal/controller/invoice_sync.go`) — lists OpenMeter's own
  invoices (`ListInvoices`) and upserts a Milo `Invoice` per period, mapping
  `InvoiceStatus` (draft/gathering/issuing/issued/payment_processing/overdue/paid/
  voided/uncollectible) onto `InvoicePhase` (Open/PastDue/Paid/Void). Simpler than
  Amberflo's `MapPhase`: OpenMeter computes `overdue` server-side, so there's no
  client-side past-due/grace-period math. Requires a `BillingProfile` with an invoicing
  app (e.g. Stripe invoicing) already provisioned on the OpenMeter instance — an
  ops/deployment precondition, not something this reconciler provisions.
- **Stripe app-data sync** (`internal/controller/stripe_customer.go`,
  `internal/openmeter/stripe.go`) — resolves the Stripe customer id the same way
  Amberflo does (`DefaultPaymentMethodRef` → `PaymentMethod` (Active) →
  `StripePaymentMethod.status.stripeCustomerId`), then calls
  `EnsureCustomerStripeAppData`. Unlike Amberflo's `scheduleStripePaymentSwitch`, this is
  an **immediate** upsert, not a future-dated scheduled transition — OpenMeter has no
  concept of a scheduled payment-method switch; its billing engine just uses whatever
  Stripe customer/payment-method id is current at its own next invoicing cycle. Requires
  a `stripe` App installed at the OpenMeter instance level first (same class of
  ops precondition as the BillingProfile above).
- **`spec.paymentTerms.*` sync via a private per-account `BillingProfile`**
  (`internal/controller/billingprofile_sync.go`, `internal/openmeter/billingprofile.go`)
  — a deliberate design choice after discussing the tradeoff: OpenMeter's payment-terms
  equivalents (`BillingWorkflow.Invoicing.DueAfter`, `.Collection.Alignment`/`.Interval`)
  live on a *shared* `BillingProfile` resource, not inline on `Customer` the way
  Amberflo's traits are — so getting Amberflo's "any customer can have any combination"
  flexibility means creating one `BillingProfile` **per `BillingAccount`** (not deduped
  by distinct terms tuple) and pointing that account's OpenMeter customer at it via
  `BillingProfileCustomerOverrideCreate`. `netDays` → `Invoicing.DueAfter` (ISO8601
  duration, `"P<n>D"`). `invoiceFrequency`/`invoiceDayOfMonth` → an anchored
  `Collection.Alignment` (`RecurringPeriodV2`): the recurring interval as an ISO8601
  duration (`"P1M"`/`"P3M"`/`"P1Y"` — using the raw-duration variant, not
  `RecurringPeriodIntervalEnum`, which has no Quarterly value) plus an anchor date whose
  day-of-month encodes `invoiceDayOfMonth`. The profile id is persisted on
  `BillingAccount`'s `openmeter.miloapis.com/billing-profile-id` annotation —
  `BillingProfile` has no external "key" field the way `Customer`/`Meter` do, so it can't
  be rediscovered deterministically the way those are. Cleaned up by a second,
  independent finalizer (`BillingProfileFinalizer`) alongside `CustomerLinkFinalizer`.
  `Apps`/`Supplier` (which app handles invoicing, and who the legal invoicing entity is —
  same for every account) are cloned from the org's existing default `BillingProfile`
  rather than assembled per-account; that default profile is the same ops/deployment
  precondition invoice sync already requires, just consumed here too.

  > ⚠️ **Only `netDays` actually takes effect today.** OpenMeter accepts the anchored
  > `Collection.Alignment` described above and then falls back to its default. Verified
  > against a real instance by POSTing
  > `{alignment: {type: anchored, anchor: 2024-01-15, interval: P3M}, dueAfter: P45D}`
  > and reading back `{alignment: {type: subscription}, interval: PT1H, dueAfter: P45D}`
  > — `dueAfter` persisted, the alignment did not. So `spec.paymentTerms.netDays` is
  > honored, while **`invoiceFrequency` and `invoiceDayOfMonth` have no observable
  > effect**.
  >
  > This is **not** SDK skew, and not a malformed payload — all three of SDK, Helm chart
  > and server image are `v1.0.0-beta.232`, the probe matched the SDK's JSON tags
  > exactly, and the server *does* validate the discriminator (a bogus `type` is
  > rejected with `discriminator property "type" has invalid value`, while `"anchored"`
  > is accepted). The schema's own description, visible in that rejection, says
  > *"Defaults to subscription"*. So anchored alignment is **specified but unimplemented**
  > in this release.
  >
  > `Collection.Interval` — a *sibling* of `alignment`, which this code leaves unset at
  > its `PT1H` default — **is** honored (`P1D`/`P7D` both round-trip). It is NOT a
  > substitute for `invoiceFrequency` though: the SDK documents it as a grace period that
  > *delays* collection of pending line items, so mapping Monthly onto `P1M` would mean
  > "wait a month before collecting", not "invoice monthly".
  >
  > **The mapping itself is correct** — this is an unimplemented server feature, not a
  > misuse of the API. OpenMeter defines two invoicing models and the BillingProfile is
  > where you choose between them (schema description, quoted verbatim from the
  > validation error above): *"Defaults to subscription, which means that we are to
  > create a new invoice every time a subscription period starts (for in advance items)
  > or ends (for in arrears items)."*
  >
  > | Alignment | Invoice created | Cadence comes from |
  > | --- | --- | --- |
  > | `subscription` (default, implemented) | at each subscription period boundary | the Plan's `billingCadence` |
  > | `anchored` (specified, NOT implemented) | on a fixed recurring calendar (anchor + interval) | the BillingProfile itself |
  >
  > `anchored` is exactly "invoice this account every quarter on the 15th regardless of
  > its subscriptions", which is what `invoiceFrequency`/`invoiceDayOfMonth` mean — so
  > this code is already written against the right field, and should start working
  > unchanged if OpenMeter implements it.
  >
  > Until then the only cadence that has any effect is the subscription's, via
  > `Plan.billingCadence` (with an optional per-`RateCard` override) — which lands with
  > the not-yet-built `OfferReconciler`/`BillingEntitlementReconciler`. Note that
  > openmeter-provider currently creates no Plans or Subscriptions at all, so accounts
  > have no billing period and **OpenMeter generates no invoices for them** — which is
  > why `ListInvoices` has been empty in every run so far, and why `reconcileInvoices`
  > has never had anything to sync end to end.
  >
  > Guardrail: `billingProfileNeedsUpdate` only compares the alignment when the server
  > actually stored an anchored one. Comparing against what we *sent* made the drift
  > check unsatisfiable, which issued an `UpdateBillingProfile` PUT on every single
  > reconcile of every account, forever (covered now by
  > `TestEnsureBillingProfile_ConvergesWhenServerNormalizesAlignment` and by the e2e
  > convergence step).

One field stays genuinely unsynced — not "deferred," there is nothing to sync per
`BillingAccount` for it:

- **`spec.taxIds[]`** — OpenMeter has a tax-identity shape (`BillingPartyTaxIdentity`),
  but confirmed by reading the OpenMeter server source: it only appears on
  `BillingProfile.Supplier` (the org itself) and as a per-invoice *snapshot*
  (`Invoice.Customer.TaxId`). There is no write path to persist a customer's tax ID
  outside of invoice generation — not even via the per-account `BillingProfile` above,
  since `Supplier` on that profile is also cloned from the org default (see above), not
  customer-controlled.

Two more are fully consumed by what's already built above, not gaps:

- **`spec.contactInfo.invoiceEmails`** — OpenMeter's `Customer` has a single
  `PrimaryEmail`, no multi-recipient list, and no invoice-delivery fan-out mechanism at
  all in the API surface explored so far.
- **`spec.defaultPaymentMethodRef`** — it's the input to Stripe app-data resolution
  above; there's no *additional* field on `Customer` it could map onto beyond that.

## Other deferred items

- **Prometheus metrics** (`amberflo_provider_reconcile_duration_seconds`-style
  reconcile-duration histogram, `{controller, result}` labels) — amberflo-provider has
  this via `internal/controller/metrics.go`; openmeter-provider has no metrics at all
  yet. Explicitly deferred early in the migration ("we can add metrics later, let's keep
  focus"). Worth doing once all four controllers exist, so the metric can carry every
  controller's label from day one instead of being retrofitted controller by controller.
- **`openmeter.MeterSlug` doesn't handle hyphens** — replaces only `.` and `/` with `_`;
  a `meterName` segment containing `-` produces a slug that fails OpenMeter's
  `^[a-z0-9]+(?:_[a-z0-9]+)*$` validation. Currently worked around in e2e fixtures by
  avoiding hyphens in `meterName` (see comments in `test/e2e/meterdefinition/*/`.yaml`
  files). Should be fixed in `MeterSlug` itself (e.g. also replacing `-` with `_`) once
  it's clear that won't collide with an existing meter naming convention.
- **`stripepaymentmethods.stripe.billing.miloapis.com` CRD isn't installed by
  `task dev:install-external-crds`** — that task only installs `milo-os/billing`'s CRDs.
  `BillingAccountReconciler` now watches `StripePaymentMethod` (stripe-provider's CRD),
  so a fresh dev cluster will log a `Failed to watch ... no matches for kind` error at
  manager startup the same way the `billingaccountbindings` RBAC gap did earlier in this
  migration, until that CRD is installed too (amberflo-provider's own dev setup has the
  same gap — it doesn't install this CRD either, so there's no existing pattern to copy
  verbatim). Not fatal (the watch just keeps retrying), but worth fixing before relying
  on the Stripe app-data sync path in a dev/e2e cluster.


