// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// DesiredBillingProfile is the controller-facing representation of a
// per-BillingAccount BillingProfile. Unlike DesiredCustomer, the caller
// does not supply Apps or Supplier: those describe the platform operator
// issuing invoices — identical for every account — so CreateBillingProfile
// clones them from the org's existing default profile rather than having
// every caller assemble them.
type DesiredBillingProfile struct {
	// AccountKey is the owning BillingAccount's customer key (its UID).
	// BillingProfile has no dedicated external-key field the way
	// Customer.Key/Meter.Slug do, so EnsureBillingProfile stores this in
	// Metadata at create time and searches for it before creating again —
	// this is what makes profile creation idempotent under retry (see
	// findBillingProfileByAccountKey). Required; EnsureBillingProfile
	// returns a PermanentError if empty.
	AccountKey string
	// Name is a human-readable, not-necessarily-unique label. It is not
	// used to look up the profile — AccountKey is.
	Name string
	// NetDays is the number of days after invoice issuance that payment is
	// due, mapped onto Workflow.Invoicing.DueAfter as an ISO8601 duration
	// ("P<NetDays>D").
	NetDays int32
	// InvoiceFrequency is Monthly, Quarterly, or Annual, mapped onto
	// Workflow.Collection.Alignment (anchored) as an ISO8601 duration
	// ("P1M"/"P3M"/"P1Y") rather than RecurringPeriodIntervalEnum, which has
	// no Quarterly value.
	//
	// NOTE: OpenMeter accepts this and then falls back to its default —
	// sending an anchored alignment reads back as {type: "subscription",
	// interval: "PT1H"}, verified live. Not version skew (SDK, chart and
	// server are all v1.0.0-beta.232) and not a bad payload: the server
	// validates the discriminator (a bogus type is rejected, "anchored" is
	// not), and the schema itself documents "Defaults to subscription". So
	// anchored alignment is specified but unimplemented in this release.
	//
	// The mapping is nonetheless the right one, not a misuse of the API:
	// OpenMeter's own schema documents alignment as the choice between
	// "subscription" (invoice at each subscription period boundary, cadence
	// coming from the Plan) and "anchored" (invoice on a fixed recurring
	// calendar, cadence coming from this profile). "Anchored" is exactly
	// "invoice quarterly on the 15th regardless of subscriptions", i.e.
	// what InvoiceFrequency/InvoiceDayOfMonth mean — so this should start
	// working unchanged once the server implements it.
	//
	// Note that Collection.Interval (a sibling of Alignment, left at its
	// PT1H default here) IS honored, but it is a grace period that delays
	// collection, not a cadence, so it is not a substitute.
	// See billingProfileNeedsUpdate and TODO.md.
	InvoiceFrequency string
	// InvoiceDayOfMonth (1-28) is encoded as the day-of-month of the
	// Collection.Alignment anchor date. Also discarded by OpenMeter today —
	// see InvoiceFrequency.
	InvoiceDayOfMonth int32
}

// ErrBillingProfileNotFound is the sentinel returned by GetBillingProfile
// on 404, and by GetDefaultBillingProfile when no profile is marked
// default.
var ErrBillingProfileNotFound = errors.New("openmeter: billing profile not found")

// billingProfileAccountKeyMetadataKey is the Metadata key under which
// createBillingProfile stores the owning BillingAccount's customer key, and
// findBillingProfileByAccountKey searches for it. BillingProfile has no
// external "key" field the way Customer/Meter do, so the caller's only
// locally-persisted pointer back to a created profile is the id it writes
// onto BillingProfileIDAnnotation — if that write fails or the process
// crashes after create but before it lands, a naive retry would call
// createBillingProfile again and orphan the first profile. Tagging the
// profile with its account key at create time lets EnsureBillingProfile
// self-heal from exactly that window instead.
const billingProfileAccountKeyMetadataKey = "billingAccountKey"

// EnsureBillingProfile creates the per-account BillingProfile identified by
// existingID if it is empty or no longer exists (a 404 on Get is treated
// the same as "never created" — e.g. an operator deleted it out of band),
// or updates it in place to match desired's mutable Workflow fields
// (Apps/Supplier/Default/Name are preserved from whatever was already
// there; only Workflow is ours to manage per-account).
//
// Before falling back to create, it always checks
// findBillingProfileByAccountKey first: existingID can be legitimately
// empty either because no profile has ever been created, or because one
// was created but the caller failed to persist its id (see
// billingProfileAccountKeyMetadataKey) — the two are indistinguishable from
// existingID alone, so the check runs unconditionally rather than trusting
// that an empty existingID means "never created".
func (c *client) EnsureBillingProfile(ctx context.Context, existingID string, desired DesiredBillingProfile) (om.BillingProfile, error) {
	if desired.AccountKey == "" {
		return om.BillingProfile{}, &PermanentError{Err: errors.New("desired.AccountKey is required")}
	}

	if existingID != "" {
		existing, err := c.GetBillingProfile(ctx, existingID)
		switch {
		case errors.Is(err, ErrBillingProfileNotFound):
			// Fall through: look up by account key before creating.
		case err != nil:
			return om.BillingProfile{}, err
		default:
			if !billingProfileNeedsUpdate(existing, desired) {
				return existing, nil
			}
			return c.updateBillingProfile(ctx, existing, desired)
		}
	}

	found, err := c.findBillingProfileByAccountKey(ctx, desired.AccountKey)
	if err != nil {
		return om.BillingProfile{}, err
	}
	if found != nil {
		if !billingProfileNeedsUpdate(*found, desired) {
			return *found, nil
		}
		return c.updateBillingProfile(ctx, *found, desired)
	}

	return c.createBillingProfile(ctx, desired)
}

// billingProfileListPageSize caps each ListBillingProfiles page fetched by
// findBillingProfile. Kept well under a realistic org's profile count per
// page so a full walk stays a small handful of requests even at scale (one
// profile is created per BillingAccount, so this can legitimately grow
// into the thousands).
const billingProfileListPageSize = 200

// findBillingProfile walks every page of ListBillingProfiles — via
// TotalCount, not just a short first page — and returns the first profile
// for which match returns true, or (nil, nil) if none do. Shared by
// findBillingProfileByAccountKey and GetDefaultBillingProfile: both need
// to find one specific profile among a set that can plausibly outgrow a
// single page (one BillingProfile is created per BillingAccount).
func (c *client) findBillingProfile(ctx context.Context, match func(om.BillingProfile) bool) (*om.BillingProfile, error) {
	pageSize := om.PaginationPageSize(billingProfileListPageSize)
	for page := 1; ; page++ {
		pageNum := om.PaginationPage(page)
		resp, err := c.api.ListBillingProfilesWithResponse(ctx, &om.ListBillingProfilesParams{
			Page:     &pageNum,
			PageSize: &pageSize,
		})
		if err != nil {
			return nil, classify(nil, nil, err)
		}
		if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
			return nil, err
		}
		if resp.JSON200 == nil {
			return nil, &PermanentError{Err: errors.New("list billing profiles: empty response body")}
		}

		for i, p := range resp.JSON200.Items {
			if match(p) {
				return &resp.JSON200.Items[i], nil
			}
		}

		// Page off the size the server reports, not the size requested — a
		// server-side clamp below billingProfileListPageSize would
		// otherwise end the walk early and silently miss later pages.
		seen := resp.JSON200.PageSize
		if seen <= 0 {
			seen = len(resp.JSON200.Items)
		}
		if len(resp.JSON200.Items) == 0 || page*seen >= resp.JSON200.TotalCount {
			return nil, nil
		}
	}
}

// findBillingProfileByAccountKey searches for the billing profile tagged
// with accountKey in Metadata[billingProfileAccountKeyMetadataKey],
// returning (nil, nil) when none matches.
func (c *client) findBillingProfileByAccountKey(ctx context.Context, accountKey string) (*om.BillingProfile, error) {
	return c.findBillingProfile(ctx, func(p om.BillingProfile) bool {
		return p.Metadata != nil && (*p.Metadata)[billingProfileAccountKeyMetadataKey] == accountKey
	})
}

// GetBillingProfile fetches a billing profile by id. Returns
// ErrBillingProfileNotFound on 404, and equally for a soft-deleted profile.
//
// OpenMeter soft-deletes billing profiles: a deleted one keeps answering GET
// with 200 and a populated deletedAt rather than 404 (verified live against
// a profile the finalizer had just removed — unlike customers, whose
// key-based lookups genuinely do 404). Reporting that as a live profile
// would make EnsureBillingProfile try to UPDATE a deleted record instead of
// creating a fresh one, wedging the account. Treating it as absent lets the
// caller fall through to findBillingProfileByAccountKey — which the list
// endpoint already excludes soft-deleted profiles from — and then create.
func (c *client) GetBillingProfile(ctx context.Context, id string) (om.BillingProfile, error) {
	if id == "" {
		return om.BillingProfile{}, &PermanentError{Err: errors.New("id is required")}
	}
	resp, err := c.api.GetBillingProfileWithResponse(ctx, id, nil)
	if err != nil {
		return om.BillingProfile{}, classify(nil, nil, err)
	}
	if resp.StatusCode() == 404 {
		return om.BillingProfile{}, fmt.Errorf("%w: %s", ErrBillingProfileNotFound, id)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return om.BillingProfile{}, err
	}
	if resp.JSON200 == nil {
		return om.BillingProfile{}, &PermanentError{Err: fmt.Errorf("get billing profile %q: empty response body", id)}
	}
	if resp.JSON200.DeletedAt != nil {
		return om.BillingProfile{}, fmt.Errorf("%w: %s (soft-deleted at %s)",
			ErrBillingProfileNotFound, id, resp.JSON200.DeletedAt.Format(time.RFC3339))
	}
	return *resp.JSON200, nil
}

// GetDefaultBillingProfile returns the org's default BillingProfile — the
// one every customer without an explicit override falls back to.
// Provisioning it (and its invoicing App) is an operator/deployment
// precondition, not something this client creates; ErrBillingProfileNotFound
// means that precondition hasn't been met yet.
func (c *client) GetDefaultBillingProfile(ctx context.Context) (om.BillingProfile, error) {
	found, err := c.findBillingProfile(ctx, func(p om.BillingProfile) bool { return p.Default })
	if err != nil {
		return om.BillingProfile{}, err
	}
	if found == nil {
		return om.BillingProfile{}, ErrBillingProfileNotFound
	}
	return *found, nil
}

// DeleteBillingProfile removes a billing profile by id. NotFound is
// treated as success.
func (c *client) DeleteBillingProfile(ctx context.Context, id string) error {
	if id == "" {
		return &PermanentError{Err: errors.New("id is required")}
	}
	resp, err := c.api.DeleteBillingProfileWithResponse(ctx, id)
	if err != nil {
		return classify(nil, nil, err)
	}
	if resp.StatusCode() == 404 {
		return nil
	}
	return classify(resp.HTTPResponse, resp.Body, nil)
}

// UpsertBillingProfileCustomerOverride points customerID at billingProfileID.
//
// customerID must be OpenMeter's *internal* customer id (a ULID), not the
// external key used everywhere else in this package (Customer/Meter/Stripe
// app data all accept a ULID-or-external-key union — this endpoint does
// not). Confirmed both by the generated SDK, where GetCustomer and
// UpsertCustomerAppData take a customerIdOrKey ULIDOrExternalKey but this
// operation takes a plain customerId string, and by the server itself:
// passing an external key here 400s with "parameter \"customerId\" in path
// has an error: string doesn't match the regular expression
// \"^[0-7[][0-9A-HJKMNP-TV-Za-hjkmnp-tv-z]{25}$\"" (OpenMeter's ULID
// pattern). Callers get the internal id from the om.Customer EnsureCustomer/
// GetCustomer already returns — see billingprofile_sync.go's
// reconcileBillingProfile and billingProfileFinalizer.Finalize.
func (c *client) UpsertBillingProfileCustomerOverride(ctx context.Context, customerID string, billingProfileID string) error {
	if customerID == "" {
		return &PermanentError{Err: errors.New("customerID is required")}
	}
	if billingProfileID == "" {
		return &PermanentError{Err: errors.New("billingProfileID is required")}
	}
	resp, err := c.api.UpsertBillingProfileCustomerOverrideWithResponse(ctx, customerID, om.BillingProfileCustomerOverrideCreate{
		BillingProfileId: &billingProfileID,
	})
	if err != nil {
		return classify(nil, nil, err)
	}
	return classify(resp.HTTPResponse, resp.Body, nil)
}

// DeleteBillingProfileCustomerOverride removes customerID's override
// (reverting it to the org default). NotFound is treated as success.
//
// customerID must be OpenMeter's internal customer id (a ULID), not the
// external key — see UpsertBillingProfileCustomerOverride's doc comment.
func (c *client) DeleteBillingProfileCustomerOverride(ctx context.Context, customerID string) error {
	if customerID == "" {
		return &PermanentError{Err: errors.New("customerID is required")}
	}
	resp, err := c.api.DeleteBillingProfileCustomerOverrideWithResponse(ctx, customerID)
	if err != nil {
		return classify(nil, nil, err)
	}
	if resp.StatusCode() == 404 {
		return nil
	}
	return classify(resp.HTTPResponse, resp.Body, nil)
}

// createBillingProfile clones Apps/Supplier/Default from the org default
// profile (a real per-account profile must not be marked Default — only
// the operator-provisioned org profile is) and sets Workflow from desired.
func (c *client) createBillingProfile(ctx context.Context, desired DesiredBillingProfile) (om.BillingProfile, error) {
	def, err := c.GetDefaultBillingProfile(ctx)
	if err != nil {
		if errors.Is(err, ErrBillingProfileNotFound) {
			return om.BillingProfile{}, &PermanentError{Err: fmt.Errorf(
				"create billing profile %q: no default billing profile provisioned on this OpenMeter instance: %w", desired.Name, err)}
		}
		return om.BillingProfile{}, fmt.Errorf("get default billing profile: %w", err)
	}

	body := om.BillingProfileCreate{
		Name:     desired.Name,
		Default:  false,
		Supplier: def.Supplier,
		Metadata: &om.Metadata{billingProfileAccountKeyMetadataKey: desired.AccountKey},
		Workflow: om.BillingWorkflowCreate{
			Invoicing:  desiredWorkflowInvoicing(desired),
			Collection: desiredWorkflowCollection(desired),
		},
	}
	apps, err := def.Apps.AsBillingProfileAppReferences()
	if err != nil {
		return om.BillingProfile{}, &PermanentError{Err: fmt.Errorf("default billing profile %q: apps not expanded to references: %w", def.Id, err)}
	}
	body.Apps = om.BillingProfileAppsCreate{
		Invoicing: apps.Invoicing.Id,
		Payment:   apps.Payment.Id,
		Tax:       apps.Tax.Id,
	}

	resp, err := c.api.CreateBillingProfileWithResponse(ctx, body)
	if err != nil {
		return om.BillingProfile{}, classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return om.BillingProfile{}, err
	}
	if resp.JSON201 == nil {
		return om.BillingProfile{}, &PermanentError{Err: fmt.Errorf("create billing profile %q: empty response body", desired.Name)}
	}
	return *resp.JSON201, nil
}

// updateBillingProfile PUTs a full replacement of the profile's mutable
// fields, preserving Name/Default/Supplier/Metadata from the existing
// record and setting Workflow from desired. Metadata must be carried
// forward explicitly — this is a full replace, and dropping it would wipe
// out the billingProfileAccountKeyMetadataKey tag findBillingProfileByAccountKey
// depends on. Apps cannot be changed post-create (no Apps field on
// BillingProfileReplaceUpdateWithWorkflow) and is not sent.
func (c *client) updateBillingProfile(ctx context.Context, existing om.BillingProfile, desired DesiredBillingProfile) (om.BillingProfile, error) {
	body := om.BillingProfileReplaceUpdateWithWorkflow{
		Name:     existing.Name,
		Default:  existing.Default,
		Supplier: existing.Supplier,
		Metadata: existing.Metadata,
		Workflow: om.BillingWorkflow{
			Invoicing:  desiredWorkflowInvoicing(desired),
			Collection: desiredWorkflowCollection(desired),
		},
	}
	resp, err := c.api.UpdateBillingProfileWithResponse(ctx, existing.Id, body)
	if err != nil {
		return om.BillingProfile{}, classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return om.BillingProfile{}, err
	}
	if resp.JSON200 == nil {
		return om.BillingProfile{}, &PermanentError{Err: fmt.Errorf("update billing profile %q: empty response body", existing.Id)}
	}
	return *resp.JSON200, nil
}

// billingProfileNeedsUpdate reports whether existing's Workflow disagrees
// with desired.
// Only fields OpenMeter actually round-trips are compared. It accepts the
// anchored collection alignment desiredWorkflowCollection sends but stores
// something else — confirmed live by sending
// {anchored, anchor 2024-01-15, interval P3M} and reading back
// {type: subscription, interval: PT1H}. Comparing against what we sent
// therefore reports drift no update can ever resolve, i.e. a PUT on every
// reconcile forever. (The old code hit this doubly: AsBillingWorkflow-
// CollectionAlignmentAnchored is a bare json.Unmarshal that does NOT check
// the discriminator, so a "subscription" payload decodes into the anchored
// struct without error, leaving a zero RecurringPeriod whose Interval union
// then fails to decode.)
//
// The alignment is still compared when OpenMeter genuinely stored an
// anchored one, so this keeps working if a future version starts honoring
// it — the discriminator, not the decode error, is what gates that.
func billingProfileNeedsUpdate(existing om.BillingProfile, desired DesiredBillingProfile) bool {
	wantDueAfter := netDaysToISODuration(desired.NetDays)
	existingDueAfter := ""
	if existing.Workflow.Invoicing != nil && existing.Workflow.Invoicing.DueAfter != nil {
		existingDueAfter = *existing.Workflow.Invoicing.DueAfter
	}
	if existingDueAfter != wantDueAfter {
		return true
	}

	if existing.Workflow.Collection == nil || existing.Workflow.Collection.Alignment == nil {
		// Nothing stored to compare against; desiredWorkflowCollection
		// always sends one, so let the update run once and converge on
		// whatever OpenMeter decides to keep.
		return true
	}
	discriminator, err := existing.Workflow.Collection.Alignment.Discriminator()
	if err != nil || discriminator != string(om.BillingWorkflowCollectionAlignmentAnchoredTypeAnchored) {
		// OpenMeter normalized the alignment away (today: to
		// "subscription"). There is nothing here we control, so treating it
		// as drift would loop forever — see this function's doc comment.
		return false
	}
	anchored, err := existing.Workflow.Collection.Alignment.AsBillingWorkflowCollectionAlignmentAnchored()
	if err != nil {
		return true
	}
	interval, err := anchored.RecurringPeriod.Interval.AsRecurringPeriodInterval0()
	if err != nil || interval != invoiceFrequencyToISODuration(desired.InvoiceFrequency) {
		return true
	}
	return anchored.RecurringPeriod.Anchor.Day() != int(desired.InvoiceDayOfMonth)
}

// desiredWorkflowInvoicing renders desired's NetDays as
// BillingWorkflowInvoicingSettings.
func desiredWorkflowInvoicing(desired DesiredBillingProfile) *om.BillingWorkflowInvoicingSettings {
	dueAfter := netDaysToISODuration(desired.NetDays)
	return &om.BillingWorkflowInvoicingSettings{DueAfter: &dueAfter}
}

// desiredWorkflowCollection renders desired's InvoiceFrequency/
// InvoiceDayOfMonth as an anchored BillingWorkflowCollectionSettings. The
// anchor's calendar date is arbitrary (2024-01-<day>) — only its day-of-month
// and the recurring interval matter to OpenMeter's alignment calculation.
// Returns nil if the union payloads somehow fail to marshal, so the caller
// sends no collection settings at all rather than an empty/half-built one.
// (These marshal plain structs and realistically cannot fail — but the
// errors were previously discarded outright, and given how much subtle
// behavior in this file turns on these union types, silently shipping a
// zero-value alignment is not a failure mode worth leaving open.)
func desiredWorkflowCollection(desired DesiredBillingProfile) *om.BillingWorkflowCollectionSettings {
	day := max(desired.InvoiceDayOfMonth, 1)
	anchor := time.Date(2024, 1, int(day), 0, 0, 0, 0, time.UTC)

	var interval om.RecurringPeriodInterval
	if err := interval.FromRecurringPeriodInterval0(invoiceFrequencyToISODuration(desired.InvoiceFrequency)); err != nil {
		return nil
	}

	var alignment om.BillingWorkflowCollectionAlignment
	if err := alignment.FromBillingWorkflowCollectionAlignmentAnchored(om.BillingWorkflowCollectionAlignmentAnchored{
		Type: om.BillingWorkflowCollectionAlignmentAnchoredTypeAnchored,
		RecurringPeriod: om.RecurringPeriodV2{
			Anchor:   anchor,
			Interval: interval,
		},
	}); err != nil {
		return nil
	}
	return &om.BillingWorkflowCollectionSettings{Alignment: &alignment}
}

// netDaysToISODuration renders a day count as an ISO8601 duration
// ("P30D"). n <= 0 falls back to 30 (matches billingv1alpha1.PaymentTerms'
// own +kubebuilder:default=30).
func netDaysToISODuration(n int32) string {
	if n <= 0 {
		n = 30
	}
	return fmt.Sprintf("P%dD", n)
}

// invoiceFrequencyToISODuration renders billingv1alpha1.PaymentTerms'
// InvoiceFrequency (Monthly/Quarterly/Annual) as an ISO8601 duration. Uses
// the raw-duration RecurringPeriodInterval variant rather than
// RecurringPeriodIntervalEnum, which has no Quarterly value.
func invoiceFrequencyToISODuration(freq string) string {
	switch freq {
	case "Quarterly":
		return "P3M"
	case "Annual":
		return "P1Y"
	default: // "Monthly" and unset (CRD default)
		return "P1M"
	}
}
