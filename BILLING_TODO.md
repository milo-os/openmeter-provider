# Billing-side follow-ups

Fixes needed in [`milo-os/billing`](../billing) that the OpenMeter migration
uncovered. **Finish the openmeter-provider work first, then do these in billing.**
Each item says what is wrong, why it matters for billing correctness, where to
change it in billing, and what to change back here once billing ships the fix.

Line references are against billing `main` as checked out at
`/Users/joseszycho/billing` (v0.3.10, the version this repo pins).

---

## 1. Validate rates against each other, not just one at a time (high priority)

**Problem.** Billing validates each `PricingRate` on its own (flat XOR tiered,
`upTo` on every band but the last — `internal/validation/servicepricing.go:77`
`validatePricingRates`). It never checks rates against each other, so it
accepts pricings that cannot be billed without counting usage twice.
openmeter-provider rejects these Offers with
`OpenMeterPlanSynced=False/InvalidPricing` and creates no plan. Because billing
still publishes them as GA and they are assignable through
`BillingEntitlement`, **a customer can be entitled to an Offer that never
bills.** The Amberflo provider rejects the same shapes
(`amberflo-provider/internal/amberflo/product_plan.go`, `buildWirePrice`), so
the gap predates OpenMeter.

**Why these shapes double-bill.** Each rate becomes an independent OpenMeter
feature with its own filter. An event that matches two features is billed by
both.

**Rules to add to `validatePricingRates`** (Usage pricings):

| Rule | Example rejected | Why |
| --- | --- | --- |
| All `match` entries share one `dimension` | `region=us-east` + `tier=premium` | An event with both values matches both rates. |
| No `match.value` repeated | `region=us-east` twice | Ambiguous price for the same usage. |
| At most one unmatched (catch-all) rate | two rates with no `match` | Both would bill all usage. |

**Where.** `internal/validation/servicepricing.go` (`validatePricingRates`),
with table tests in `servicepricing_test.go`. The ServicePricing webhook
(`internal/webhook/v1alpha1/servicepricing_webhook.go`) already calls this
path, so bad pricings are rejected on create and update.

**Done when** a ServicePricing with any of the shapes above is denied by the
webhook, and existing fan-out output from service-catalog still passes (check
with the service-catalog owners before merging — if fan-out emits any of these
shapes today, it must change first).

## 2. Reject an Offer that prices the same metric twice

**Problem.** Two Usage ServicePricings on the same `metric` inside one Offer
bill the same usage twice. Each ServicePricing is valid on its own, so item 1
cannot catch this — only something that sees the whole Offer can.

**Where.** The snapshot is built in the Offer controller
(`internal/controller/offer_controller.go:152` `buildSnapshots`), which
resolves `servicePricingRefs`. Either:

- (preferred) the Offer webhook validates refs on the Draft→GA transition
  (needs a ServicePricing lookup), so the publish is refused; or
- `buildSnapshots` refuses to snapshot and sets `Ready=False` with a clear
  reason, so the Offer is never assignable (`OfferIsAssignable` requires a
  non-empty snapshot).

**Done when** publishing such an Offer fails visibly and it never becomes
assignable.

## 3. Check that a match dimension is declared on the meter

**Problem.** A `match.dimension` that is not in the MeterDefinition's
`measurement.dimensions` (or the system `project_name` dimension) can never
match any usage. OpenMeter rejects the feature filter
(`FeatureInvalidFiltersError`), so openmeter-provider reports
`InvalidPricing`.

**Where.** ServicePricing webhook: look up the MeterDefinition whose
`spec.meterName == spec.metric` and check each `rates[].match.dimension`.
Ordering caveat: the MeterDefinition may not exist yet when fan-out creates the
ServicePricing, so this may fit better as an Offer publish-time check (with
item 2) than as a ServicePricing admission check.

**Done when** an Offer whose pricing matches an undeclared dimension cannot be
published.

## 4. Fix the `upTo` documentation: inclusive, not exclusive

**Problem.** `api/v1alpha1/pricing_types.go:84` and `:87` document `upTo` as
the *exclusive* upper bound of a band. Both providers treat it as
**inclusive**: OpenMeter's `upToAmount` is "up to and including", and Amberflo
uses `startAfterUnit = upTo`. With count meters the difference is one unit
billed at the wrong band rate at every boundary.

**Change.** Update the doc comments (and regenerate CRDs/docs) to say
inclusive, unless product wants exclusive — in which case both providers must
change instead. Decide explicitly; don't leave the doc and the behavior
disagreeing.

## 5. Validate that tier bounds increase

**Problem.** `validatePricingTiers` (`servicepricing.go:112`) only checks that
`upTo` is present on every band but the last. It accepts out-of-order bands
(`upTo: 100`, then `upTo: 50`), which have no sensible graduated meaning and
which billing systems reject when the plan is synced.

**Change.** Require every `upTo` to be strictly greater than the previous
band's (compare as decimals, not strings).

## 6. Pin down catch-all semantics in the API docs

**Problem.** `PricingRate` says "the last unmatched entry is the default
catch-all". "Last" suggests several unmatched entries are allowed, with
the last one winning; item 1 forbids that. Also spell out what the catch-all
covers: *usage not matched by any other rate* (openmeter-provider implements it
as `$nin` of the matched values), and that matched-only rates leave unmatched
usage unbilled.

**Change.** Update the `PricingRate` / `DimensionMatch` doc comments in
`api/v1alpha1/pricing_types.go` to match the rules from item 1.

---

## Back in openmeter-provider, once billing ships the above

- [ ] Bump `go.miloapis.com/billing` in `go.mod` and `BILLING_REPO_REF` in
      `Taskfile.yaml` to the release with the fixes (both are on v0.3.10 now).
- [ ] Keep the `InvalidPricing` rejection in
      `internal/controller/offer_mapping.go` as a backstop: Offers published
      before billing enforced these rules are immutable and can still exist.
      Update its comments to say billing now validates the same rules.
- [ ] If item 4 is decided as *exclusive*, change `usagePrice` in
      `offer_mapping.go` (and its tests) accordingly.

## Related openmeter-provider work (not billing changes, tracked here for context)

- [x] **e2e follows billing's publish flow.** Fixtures create real
      ServicePricings and Draft Offers with `servicePricingRefs`;
      `test/e2e/offer/publish.sh` performs billing's GA flip and snapshot fill
      (the billing operator does not run in e2e). If the e2e cluster ever runs
      the billing operator, drop `publish.sh` and flip only `launchStage`.
- [ ] **Once item 1 lands, the `invalid-pricing` e2e fixture will be rejected
      at ServicePricing admission** (if the billing webhook runs in e2e).
      Move that coverage to the unit tests and keep the e2e only for Offers
      published before the validation existed, or delete it.
- [ ] **No BillingEntitlement → subscription sync yet.** Plans are published
      correctly, but nothing attaches customers to them (see
      `TODO.md`, controller migration status).
- [ ] **Unverified OpenMeter behavior:** whether a `$nin` catch-all counts
      events that have no value for the dimension, and whether OpenMeter allows
      deleting an archived plan version that still has subscriptions.
