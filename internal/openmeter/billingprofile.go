// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// DesiredBillingProfile is the controller-facing representation of a
// per-BillingAccount BillingProfile. Unlike DesiredCustomer, the caller
// does not supply Apps, Supplier, or most of the workflow: those describe
// how the platform operator invoices — identical for every account — so
// EnsureBillingProfile copies them from the org's default profile and
// overrides only the settings below (see desiredWorkflow).
type DesiredBillingProfile struct {
	// AccountKey is the owning BillingAccount's customer key (its UID).
	// BillingProfile has no dedicated external-key field the way
	// Customer.Key/Meter.Slug do, so EnsureBillingProfile stores this in
	// Metadata at create time and searches for it before creating again —
	// this is what makes profile creation idempotent under retry (see
	// findBillingProfileByAccountKey). It is also the ownership proof
	// EnsureBillingProfile checks before adopting a profile by id (see
	// BillingProfileOwnershipMismatch). Required; EnsureBillingProfile
	// returns a PermanentError if empty.
	AccountKey CustomerKey
	// Name is a human-readable, not-necessarily-unique label. It is not
	// used to look up the profile — AccountKey is.
	Name string
	// NetDays is the number of days after invoice issuance that payment is
	// due, mapped onto Workflow.Invoicing.DueAfter as an ISO8601 duration
	// ("P<NetDays>D").
	NetDays int32
	// CollectionMethod, when set, overrides the default profile's
	// Workflow.Payment.CollectionMethod (e.g. send_invoice for an account
	// billed by invoice under a BillingArrangement). Empty keeps the
	// default profile's method.
	CollectionMethod om.CollectionMethod
	// InvoiceFrequency is Monthly, Quarterly, or Annual, mapped onto
	// Workflow.Collection.Alignment (anchored) as an ISO8601 duration
	// ("P1M"/"P3M"/"P1Y") rather than RecurringPeriodIntervalEnum, which has
	// no Quarterly value.
	//
	// NOTE: OpenMeter accepts this and then falls back to its default —
	// sending an anchored alignment reads back as {type: "subscription"},
	// verified live and confirmed in the server source: the v1 HTTP driver's
	// fromAPIBillingWorkflow always stores billing.DefaultWorkflowConfig's
	// alignment and ignores the submitted one. So anchored alignment is
	// specified but unimplemented in this release.
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
	// Note that Collection.Interval (a sibling of Alignment, copied from the
	// default profile) IS honored, but it is a grace period that delays
	// collection, not a cadence, so it is not a substitute.
	// See billingProfileNeedsUpdate and TODO.md.
	InvoiceFrequency string
	// InvoiceDayOfMonth (1-28) is encoded as the day-of-month of the
	// Collection.Alignment anchor date. Also discarded by OpenMeter today —
	// see InvoiceFrequency.
	InvoiceDayOfMonth int32
}

// EnsureBillingProfileResult is what EnsureBillingProfile converged to,
// plus the non-fatal findings the caller should surface to operators.
type EnsureBillingProfileResult struct {
	// Profile is the account's billing profile as OpenMeter now stores it.
	Profile om.BillingProfile
	// IgnoredExistingID is the existingID EnsureBillingProfile refused to
	// adopt because it does not belong to this account (see
	// IgnoredExistingReason). Empty when existingID was adopted, empty, or
	// simply gone.
	IgnoredExistingID string
	// IgnoredExistingReason explains why IgnoredExistingID was refused.
	IgnoredExistingReason string
	// AppsDrift is non-empty when Profile's apps differ from the default
	// profile's. OpenMeter cannot change apps on an existing profile (the
	// update body has no apps field), and recreating the profile is not
	// safe either — issued invoices reference it — so this is reported for
	// an operator to resolve rather than fixed automatically.
	AppsDrift string
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
//
// Metadata is only writable through the OpenMeter API, never by Milo
// customers, which is why it — and not the customer-writable annotation —
// is what proves a profile belongs to an account.
const billingProfileAccountKeyMetadataKey = "billingAccountKey"

// BillingProfileOwnershipMismatch returns a non-empty reason when p must
// not be treated as accountKey's private profile: it is the org default
// profile (which every customer falls back to and must never be modified
// or deleted on one account's behalf), or its account-key metadata tag
// names a different account (or none). An empty result means p belongs to
// accountKey.
//
// Callers use it before adopting, updating, or deleting a profile whose id
// came from BillingProfileIDAnnotation: annotations on a BillingAccount are
// writable by the account's own users, so an id found there proves
// nothing on its own.
func BillingProfileOwnershipMismatch(p om.BillingProfile, accountKey CustomerKey) string {
	if p.Default {
		return fmt.Sprintf("billing profile %s is the org default profile", p.Id)
	}
	tagged := ""
	if p.Metadata != nil {
		tagged = (*p.Metadata)[billingProfileAccountKeyMetadataKey]
	}
	if tagged == "" {
		return fmt.Sprintf("billing profile %s carries no %s metadata tag", p.Id, billingProfileAccountKeyMetadataKey)
	}
	if tagged != string(accountKey) {
		return fmt.Sprintf("billing profile %s belongs to billing account %s", p.Id, tagged)
	}
	return ""
}

// EnsureBillingProfile converges the per-account BillingProfile.
//
// existingID (the id the caller last persisted) is adopted only after
// BillingProfileOwnershipMismatch confirms it belongs to desired.AccountKey;
// a foreign or default profile is reported in the result and otherwise
// ignored. When existingID is empty, gone (a 404 or soft-deleted record is
// treated the same as "never created" — e.g. an operator deleted it out of
// band), or refused, findBillingProfileByAccountKey is consulted before
// creating: existingID can legitimately be empty either because no profile
// has ever been created, or because one was created but the caller failed
// to persist its id (see billingProfileAccountKeyMetadataKey).
//
// Create and update both send the org default profile's full workflow with
// only this account's settings overridden (see desiredWorkflow), and the
// default profile's supplier. OpenMeter fills every omitted workflow field
// with billing.DefaultWorkflowConfig — on update too, since the PUT is a
// full replace — so sending a partial workflow would silently revert the
// operator's settings (progressive billing, draft period, tax, ...) to
// OpenMeter's built-in defaults.
func (c *client) EnsureBillingProfile(ctx context.Context, existingID string, desired DesiredBillingProfile) (EnsureBillingProfileResult, error) {
	var result EnsureBillingProfileResult
	if desired.AccountKey == "" {
		return result, &PermanentError{Err: errors.New("desired.AccountKey is required")}
	}

	def, err := c.GetDefaultBillingProfile(ctx)
	if err != nil {
		if errors.Is(err, ErrBillingProfileNotFound) {
			return result, &PermanentError{Err: fmt.Errorf(
				"billing profile %q: no default billing profile provisioned on this OpenMeter instance: %w", desired.Name, err)}
		}
		return result, fmt.Errorf("get default billing profile: %w", err)
	}

	var current *om.BillingProfile
	if existingID != "" {
		existing, err := c.GetBillingProfile(ctx, existingID)
		switch {
		case errors.Is(err, ErrBillingProfileNotFound):
			// Fall through: look up by account key before creating.
		case err != nil:
			return result, err
		default:
			if reason := BillingProfileOwnershipMismatch(existing, desired.AccountKey); reason != "" {
				result.IgnoredExistingID = existingID
				result.IgnoredExistingReason = reason
			} else {
				current = &existing
			}
		}
	}

	if current == nil {
		found, err := c.findBillingProfileByAccountKey(ctx, desired.AccountKey)
		if err != nil {
			return result, err
		}
		current = found
	}

	if current == nil {
		created, err := c.createBillingProfile(ctx, def, desired)
		if err != nil {
			return result, err
		}
		result.Profile = created
		return result, nil
	}

	result.AppsDrift = billingProfileAppsDrift(*current, def)
	if !billingProfileNeedsUpdate(*current, def, desired) {
		result.Profile = *current
		return result, nil
	}
	updated, err := c.updateBillingProfile(ctx, *current, def, desired)
	if err != nil {
		return result, err
	}
	result.Profile = updated
	return result, nil
}

// billingProfileListPageSize caps each ListBillingProfiles page fetched by
// listBillingProfiles. Kept well under a realistic org's profile count per
// page so a full walk stays a small handful of requests even at scale (one
// profile is created per BillingAccount, so this can legitimately grow
// into the thousands).
const billingProfileListPageSize = 200

// billingProfileListAttempts bounds how many times listBillingProfiles
// restarts a walk that a concurrent deletion invalidated.
const billingProfileListAttempts = 3

// listBillingProfiles walks every live billing profile and returns those
// for which match returns true (stopping at the first one when firstOnly).
//
// The walk is ordered by createdAt ascending. Profiles are created
// constantly (one per BillingAccount), and under the server's default
// createdAt-DESCENDING order every create during the walk pushes earlier
// items onto later pages, so the walk re-reads some profiles and skips
// none — but deletions (which shift items towards the front) make it skip
// some. Ascending order makes creates harmless (they append to the last
// page); a deletion shows up as a smaller TotalCount and restarts the walk.
func (c *client) listBillingProfiles(ctx context.Context, firstOnly bool, match func(om.BillingProfile) bool) ([]om.BillingProfile, error) {
	for range billingProfileListAttempts {
		found, stable, err := c.walkBillingProfiles(ctx, firstOnly, match)
		if err != nil || stable {
			return found, err
		}
	}
	return nil, &TransientError{Err: errors.New("list billing profiles: profiles kept being deleted during the walk")}
}

// walkBillingProfiles is one pass of listBillingProfiles. stable is false
// when TotalCount shrank mid-walk, i.e. a deletion may have shifted an
// unseen profile onto a page already read.
func (c *client) walkBillingProfiles(ctx context.Context, firstOnly bool, match func(om.BillingProfile) bool) (found []om.BillingProfile, stable bool, err error) {
	pageSize := om.PaginationPageSize(billingProfileListPageSize)
	total := -1
	for page := 1; ; page++ {
		pageNum := om.PaginationPage(page)
		resp, err := c.api.ListBillingProfilesWithResponse(ctx, &om.ListBillingProfilesParams{
			Page:     &pageNum,
			PageSize: &pageSize,
			Order:    ptr(om.SortOrderASC),
			OrderBy:  ptr(om.BillingProfileOrderByCreatedAt),
		})
		if err != nil {
			return nil, false, classify(nil, nil, err)
		}
		if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
			return nil, false, err
		}
		if resp.JSON200 == nil {
			return nil, false, &PermanentError{Err: errors.New("list billing profiles: empty response body")}
		}
		if total >= 0 && resp.JSON200.TotalCount < total {
			return nil, false, nil
		}
		total = resp.JSON200.TotalCount

		for _, p := range resp.JSON200.Items {
			if match(p) {
				found = append(found, p)
				if firstOnly {
					return found, true, nil
				}
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
			return found, true, nil
		}
	}
}

// isAccountProfile reports whether p is accountKey's private profile.
func isAccountProfile(accountKey CustomerKey) func(om.BillingProfile) bool {
	return func(p om.BillingProfile) bool {
		return BillingProfileOwnershipMismatch(p, accountKey) == ""
	}
}

// findBillingProfileByAccountKey returns the oldest live billing profile
// tagged with accountKey in Metadata[billingProfileAccountKeyMetadataKey],
// or (nil, nil) when none matches. The default profile never matches.
func (c *client) findBillingProfileByAccountKey(ctx context.Context, accountKey CustomerKey) (*om.BillingProfile, error) {
	found, err := c.listBillingProfiles(ctx, true, isAccountProfile(accountKey))
	if err != nil || len(found) == 0 {
		return nil, err
	}
	return &found[0], nil
}

// FindBillingProfilesByAccountKey returns every live billing profile tagged
// with accountKey, oldest first — normally at most one, but a create whose
// response was lost can leave a duplicate behind, and the deletion path
// should clean those up too. The default profile is never returned.
func (c *client) FindBillingProfilesByAccountKey(ctx context.Context, accountKey CustomerKey) ([]om.BillingProfile, error) {
	if accountKey == "" {
		return nil, &PermanentError{Err: errors.New("accountKey is required")}
	}
	return c.listBillingProfiles(ctx, false, isAccountProfile(accountKey))
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
//
// The list endpoint has no "default" filter, but it can order by the
// default flag: ordered descending, the default profile (at most one
// exists) is the first item of the first page, so this is a single
// one-item request instead of a walk over every per-account profile. The
// full walk remains as a fallback should a server ever ignore the order.
func (c *client) GetDefaultBillingProfile(ctx context.Context) (om.BillingProfile, error) {
	pageNum := om.PaginationPage(1)
	pageSize := om.PaginationPageSize(1)
	resp, err := c.api.ListBillingProfilesWithResponse(ctx, &om.ListBillingProfilesParams{
		Page:     &pageNum,
		PageSize: &pageSize,
		Order:    ptr(om.SortOrderDESC),
		OrderBy:  ptr(om.BillingProfileOrderByDefault),
	})
	if err != nil {
		return om.BillingProfile{}, classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return om.BillingProfile{}, err
	}
	if resp.JSON200 == nil {
		return om.BillingProfile{}, &PermanentError{Err: errors.New("list billing profiles: empty response body")}
	}
	if len(resp.JSON200.Items) > 0 && resp.JSON200.Items[0].Default {
		return resp.JSON200.Items[0], nil
	}
	if resp.JSON200.TotalCount <= len(resp.JSON200.Items) {
		return om.BillingProfile{}, ErrBillingProfileNotFound
	}

	found, err := c.listBillingProfiles(ctx, true, func(p om.BillingProfile) bool { return p.Default })
	if err != nil {
		return om.BillingProfile{}, err
	}
	if len(found) == 0 {
		return om.BillingProfile{}, ErrBillingProfileNotFound
	}
	return found[0], nil
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
func (c *client) UpsertBillingProfileCustomerOverride(ctx context.Context, customerID CustomerID, billingProfileID string) error {
	if customerID == "" {
		return &PermanentError{Err: errors.New("customerID is required")}
	}
	if billingProfileID == "" {
		return &PermanentError{Err: errors.New("billingProfileID is required")}
	}
	resp, err := c.api.UpsertBillingProfileCustomerOverrideWithResponse(ctx, string(customerID), om.BillingProfileCustomerOverrideCreate{
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
// It works for a soft-deleted customer too: OpenMeter does not cascade a
// customer deletion to its override, and the override endpoint looks the
// override up by customer id without checking the customer itself.
func (c *client) DeleteBillingProfileCustomerOverride(ctx context.Context, customerID CustomerID) error {
	if customerID == "" {
		return &PermanentError{Err: errors.New("customerID is required")}
	}
	resp, err := c.api.DeleteBillingProfileCustomerOverrideWithResponse(ctx, string(customerID))
	if err != nil {
		return classify(nil, nil, err)
	}
	if resp.StatusCode() == 404 {
		return nil
	}
	return classify(resp.HTTPResponse, resp.Body, nil)
}

// customerListPageSize caps each ListCustomers page fetched by
// CustomerIDsByKey.
const customerListPageSize = 100

// CustomerIDsByKey returns the internal id of every customer — live or
// soft-deleted — whose external key is exactly key.
//
// It exists for the billing-profile cleanup: the override to remove is
// keyed by the internal id, the only locally persisted copy of that id is a
// customer-writable annotation, and by the time the cleanup runs the
// customer has usually been deleted already, so GetCustomer (which
// excludes soft-deleted customers) cannot vouch for it. OpenMeter keeps the
// key on a soft-deleted customer, and the list endpoint can include
// deleted ones; its key filter is a case-insensitive partial match, so the
// exact comparison happens here.
func (c *client) CustomerIDsByKey(ctx context.Context, key CustomerKey) ([]CustomerID, error) {
	if key == "" {
		return nil, &PermanentError{Err: errors.New("key is required")}
	}
	pageSize := om.PaginationPageSize(customerListPageSize)
	var out []CustomerID
	for page := 1; ; page++ {
		pageNum := om.PaginationPage(page)
		resp, err := c.api.ListCustomersWithResponse(ctx, &om.ListCustomersParams{
			Page:           &pageNum,
			PageSize:       &pageSize,
			Key:            ptr(string(key)),
			IncludeDeleted: ptr(true),
		})
		if err != nil {
			return nil, classify(nil, nil, err)
		}
		if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
			return nil, err
		}
		if resp.JSON200 == nil {
			return nil, &PermanentError{Err: fmt.Errorf("list customers by key %q: empty response body", key)}
		}
		for _, cust := range resp.JSON200.Items {
			if cust.Key != nil && *cust.Key == string(key) && cust.Id != "" {
				out = append(out, CustomerID(cust.Id))
			}
		}
		seen := resp.JSON200.PageSize
		if seen <= 0 {
			seen = len(resp.JSON200.Items)
		}
		if len(resp.JSON200.Items) == 0 || page*seen >= resp.JSON200.TotalCount {
			return out, nil
		}
	}
}

// createBillingProfile clones Apps/Supplier and the full workflow from the
// org default profile (a real per-account profile must not be marked
// Default — only the operator-provisioned org profile is), overriding the
// account's own settings (see desiredWorkflow).
func (c *client) createBillingProfile(ctx context.Context, def om.BillingProfile, desired DesiredBillingProfile) (om.BillingProfile, error) {
	workflow, err := desiredWorkflow(def, desired)
	if err != nil {
		return om.BillingProfile{}, err
	}
	body := om.BillingProfileCreate{
		Name:     desired.Name,
		Default:  false,
		Supplier: def.Supplier,
		Metadata: &om.Metadata{billingProfileAccountKeyMetadataKey: string(desired.AccountKey)},
		Workflow: om.BillingWorkflowCreate(workflow),
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

// updateBillingProfile PUTs a full replacement of the profile. OpenMeter's
// update is a true replace (omitted workflow fields reset to
// billing.DefaultWorkflowConfig, an omitted description is cleared), so
// every field is sent: Name/Description/Default from the existing record,
// Metadata from the existing record with the account-key tag guaranteed
// (dropping it would break findBillingProfileByAccountKey for good),
// Supplier from the default profile, and the workflow from desiredWorkflow.
// Apps cannot be changed post-create (no Apps field on
// BillingProfileReplaceUpdateWithWorkflow) and are not sent — see
// EnsureBillingProfileResult.AppsDrift.
func (c *client) updateBillingProfile(ctx context.Context, existing, def om.BillingProfile, desired DesiredBillingProfile) (om.BillingProfile, error) {
	workflow, err := desiredWorkflow(def, desired)
	if err != nil {
		return om.BillingProfile{}, err
	}
	metadata := om.Metadata{}
	if existing.Metadata != nil {
		for k, v := range *existing.Metadata {
			metadata[k] = v
		}
	}
	metadata[billingProfileAccountKeyMetadataKey] = string(desired.AccountKey)

	body := om.BillingProfileReplaceUpdateWithWorkflow{
		Name:        existing.Name,
		Description: existing.Description,
		Default:     existing.Default,
		Supplier:    def.Supplier,
		Metadata:    &metadata,
		Workflow:    workflow,
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

// desiredWorkflow is the workflow this account's profile should carry: a
// deep copy of the default profile's complete workflow (collection
// interval, invoicing — autoAdvance, draftPeriod, progressiveBilling,
// defaultTaxConfig, subscriptionEndProrationMode —, payment, and tax),
// with only what the account controls overridden:
//
//   - Invoicing.DueAfter from NetDays;
//   - Payment.CollectionMethod, when desired sets one;
//   - Collection.Alignment, the anchored alignment carrying
//     InvoiceFrequency/InvoiceDayOfMonth (see DesiredBillingProfile).
func desiredWorkflow(def om.BillingProfile, desired DesiredBillingProfile) (om.BillingWorkflow, error) {
	var wf om.BillingWorkflow
	raw, err := json.Marshal(def.Workflow)
	if err != nil {
		return wf, &PermanentError{Err: fmt.Errorf("default billing profile %q: encode workflow: %w", def.Id, err)}
	}
	if err := json.Unmarshal(raw, &wf); err != nil {
		return wf, &PermanentError{Err: fmt.Errorf("default billing profile %q: decode workflow: %w", def.Id, err)}
	}

	if wf.Invoicing == nil {
		wf.Invoicing = &om.BillingWorkflowInvoicingSettings{}
	}
	dueAfter := netDaysToISODuration(desired.NetDays)
	wf.Invoicing.DueAfter = &dueAfter

	if desired.CollectionMethod != "" {
		if wf.Payment == nil {
			wf.Payment = &om.BillingWorkflowPaymentSettings{}
		}
		method := desired.CollectionMethod
		wf.Payment.CollectionMethod = &method
	}

	if wf.Collection == nil {
		wf.Collection = &om.BillingWorkflowCollectionSettings{}
	}
	if alignment := desiredCollectionAlignment(desired); alignment != nil {
		wf.Collection.Alignment = alignment
	}
	return wf, nil
}

// billingProfileNeedsUpdate reports whether existing differs from what
// EnsureBillingProfile would send (see updateBillingProfile) in any way
// OpenMeter actually round-trips: the workflow, the supplier, or the
// account-key metadata tag. Comparing exactly what is sent — and nothing
// the server rewrites — is what lets the reconcile converge instead of
// PUTting forever.
//
// The collection alignment is the one field compared differently.
// OpenMeter accepts the anchored alignment desiredCollectionAlignment
// sends but stores {type: subscription} (confirmed live and in the server
// source), so comparing against what was sent would report drift no update
// can ever resolve, i.e. a PUT on every reconcile forever. (An earlier
// version hit this doubly: AsBillingWorkflowCollectionAlignmentAnchored is
// a bare json.Unmarshal that does NOT check the discriminator, so a
// "subscription" payload decodes into the anchored struct without error,
// leaving a zero RecurringPeriod whose Interval union then fails to
// decode.) The alignment is therefore compared only when OpenMeter
// genuinely stored an anchored one, so this keeps working if a future
// version starts honoring it — the discriminator, not the decode error, is
// what gates that.
func billingProfileNeedsUpdate(existing, def om.BillingProfile, desired DesiredBillingProfile) bool {
	if existing.Metadata == nil || (*existing.Metadata)[billingProfileAccountKeyMetadataKey] != string(desired.AccountKey) {
		return true
	}
	if supplierFingerprint(existing.Supplier) != supplierFingerprint(def.Supplier) {
		return true
	}

	want, err := desiredWorkflow(def, desired)
	if err != nil {
		return true
	}
	if !workflowsEqualIgnoringAlignment(existing.Workflow, want) {
		return true
	}

	if existing.Workflow.Collection == nil || existing.Workflow.Collection.Alignment == nil {
		// Nothing stored to compare against; desiredWorkflow always sends
		// one, so let the update run once and converge on whatever
		// OpenMeter decides to keep.
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
	return anchored.RecurringPeriod.Anchor.Day() != int(max(desired.InvoiceDayOfMonth, 1))
}

// workflowsEqualIgnoringAlignment compares two workflows field by field
// (via their JSON encoding, so nil and absent sections compare the way the
// wire does) with Collection.Alignment blanked out on both sides.
func workflowsEqualIgnoringAlignment(a, b om.BillingWorkflow) bool {
	encode := func(w om.BillingWorkflow) []byte {
		if w.Collection != nil {
			collection := *w.Collection
			collection.Alignment = nil
			w.Collection = &collection
		}
		raw, err := json.Marshal(w)
		if err != nil {
			return nil
		}
		return raw
	}
	ea, eb := encode(a), encode(b)
	return ea != nil && eb != nil && bytes.Equal(ea, eb)
}

// supplierFingerprint is the part of a BillingParty a profile update
// actually stores: OpenMeter keeps only the name, the tax code, and the
// first address (see the v1 HTTP driver's apiBillingPartyToSupplierContact);
// ids and keys are not round-tripped.
func supplierFingerprint(p om.BillingParty) string {
	type fingerprint struct {
		Name    *string     `json:"name,omitempty"`
		TaxCode *string     `json:"taxCode,omitempty"`
		Address *om.Address `json:"address,omitempty"`
	}
	fp := fingerprint{Name: p.Name}
	if p.Addresses != nil && len(*p.Addresses) > 0 {
		addr := (*p.Addresses)[0]
		fp.Address = &addr
		// The tax code is only stored alongside an address.
		if p.TaxId != nil {
			fp.TaxCode = p.TaxId.Code
		}
	}
	raw, err := json.Marshal(fp)
	if err != nil {
		return ""
	}
	return string(raw)
}

// billingProfileAppsDrift describes how p's apps differ from def's, or
// returns "" when they match (or either side cannot be decoded, which is
// not something an operator could act on).
func billingProfileAppsDrift(p, def om.BillingProfile) string {
	have, err := p.Apps.AsBillingProfileAppReferences()
	if err != nil {
		return ""
	}
	want, err := def.Apps.AsBillingProfileAppReferences()
	if err != nil {
		return ""
	}
	if have.Invoicing.Id == want.Invoicing.Id && have.Payment.Id == want.Payment.Id && have.Tax.Id == want.Tax.Id {
		return ""
	}
	return fmt.Sprintf(
		"billing profile %s uses apps invoicing=%s payment=%s tax=%s but the default profile %s uses invoicing=%s payment=%s tax=%s; OpenMeter cannot change apps on an existing profile",
		p.Id, have.Invoicing.Id, have.Payment.Id, have.Tax.Id,
		def.Id, want.Invoicing.Id, want.Payment.Id, want.Tax.Id,
	)
}

// desiredCollectionAlignment renders desired's InvoiceFrequency/
// InvoiceDayOfMonth as an anchored collection alignment. The anchor's
// calendar date is arbitrary (2024-01-<day>) — only its day-of-month and
// the recurring interval matter to OpenMeter's alignment calculation.
// Returns nil if the union payloads somehow fail to marshal, so the caller
// keeps the default profile's alignment rather than an empty/half-built
// one. (These marshal plain structs and realistically cannot fail — but
// given how much subtle behavior in this file turns on these union types,
// silently shipping a zero-value alignment is not a failure mode worth
// leaving open.)
func desiredCollectionAlignment(desired DesiredBillingProfile) *om.BillingWorkflowCollectionAlignment {
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
	return &alignment
}

// netDaysToISODuration renders a day count as an ISO8601 duration
// ("P30D"). n <= 0 falls back to 30 (billing's standard net terms).
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
