// SPDX-License-Identifier: AGPL-3.0-only

// Package openmeter is a typed client for the OpenMeter metering backend.
// It wraps the generated OpenMeter Go SDK (github.com/openmeterio/openmeter/api/client/go)
// with the Milo-friendly shapes used by the reconcilers: Ensure/Get/Delete
// per resource type, plus permanent-vs-transient error classification.
// Mirrors the amberflo-provider client's architecture.
//
// Each resource type owns its own file (meter.go, customer.go, ...): its
// DesiredX struct, wire-shape translation, and the *client methods that
// implement its slice of the Client interface below. This file holds only
// what every resource type shares — the interface itself, the concrete
// client, and its constructor — so adding a new resource type never
// requires touching an existing one's file, only adding its own and
// extending the interface here.
package openmeter

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	om "github.com/openmeterio/openmeter/api/client/go"
)

// CustomerKey is OpenMeter's external customer key. In this codebase it is
// always the Milo BillingAccount's metadata.uid (see DesiredCustomer.Key).
// Accepted by EnsureCustomer, GetCustomer, DeleteCustomer, and
// EnsureCustomerStripeAppData.
//
// Distinct from CustomerID: OpenMeter's customerIdOrKey path parameter
// matches on either its own server-assigned id OR this external key for
// those endpoints, but several others (UpsertBillingProfileCustomerOverride,
// DeleteBillingProfileCustomerOverride, ListInvoices' Customers filter, and
// the meter query endpoint's filterCustomerId) accept ONLY the internal id
// and 400 if handed this key instead. A single plain `string` parameter
// used for both was a real source of confusion (and one live bug caught
// this session, in an e2e test script) — these named types make the two
// impossible to swap by accident; the compiler rejects it.
type CustomerKey string

// CustomerID is OpenMeter's own internal customer id — a server-assigned
// ULID (e.g. "01M25WVCC7N39X1F6MWTQ8SH1H"), found on om.Customer.Id.
// Required by UpsertBillingProfileCustomerOverride,
// DeleteBillingProfileCustomerOverride, ListInvoices, and meter queries'
// filterCustomerId — see CustomerKey's doc comment for why this is a
// distinct type rather than another plain string.
type CustomerID string

// StripeCustomerID is Stripe's own customer id (e.g. "cus_..."), a third,
// unrelated identifier domain that happens to share a bare "customer id"
// name with CustomerID above. Kept as its own type so the two are never
// confused at a call site (see stripe.go's EnsureCustomerStripeAppData).
type StripeCustomerID string

// Client is the typed interface over the OpenMeter API used by the
// reconcilers. It is the seam the controllers depend on.
type Client interface {
	// Meters (see meter.go).

	// EnsureMeter creates the meter if absent, or updates it in place to
	// match desired's mutable fields (name, description, groupBy); changed
	// reports whether either write happened. Returns a PermanentError if an
	// existing meter's immutable fields (eventType/aggregation/
	// valueProperty) disagree with desired.
	EnsureMeter(ctx context.Context, desired DesiredMeter) (meter om.Meter, changed bool, err error)
	// GetMeter fetches a meter by slug. Returns ErrMeterNotFound when absent.
	GetMeter(ctx context.Context, slug string) (om.Meter, error)
	// DeleteMeter removes a meter by slug. OpenMeter's own not-found answer
	// is treated as success; a 404 from anything else is a PermanentError.
	DeleteMeter(ctx context.Context, slug string) error

	// Customers (see customer.go).

	// EnsureCustomer creates the customer if absent, or updates it in place
	// to match desired's fields.
	EnsureCustomer(ctx context.Context, desired DesiredCustomer) (om.Customer, error)
	// GetCustomer fetches a customer by key. Returns ErrCustomerNotFound
	// when absent.
	GetCustomer(ctx context.Context, key CustomerKey) (om.Customer, error)
	// DeleteCustomer removes a customer by key. NotFound is treated as
	// success.
	DeleteCustomer(ctx context.Context, key CustomerKey) error
	// ReleaseCustomerSubjects clears the customer's usage attribution
	// (subject keys), leaving every other field unchanged. A missing
	// customer or one with no subject keys is a no-op. OpenMeter refuses
	// while a subscription is active, so cancel subscriptions first.
	ReleaseCustomerSubjects(ctx context.Context, key CustomerKey) error

	// Invoices (see invoice.go).

	// ListInvoices returns every invoice for the customer identified by
	// customerID. An empty slice with a nil error means no invoices exist
	// yet. customerID must be OpenMeter's internal customer id (a ULID),
	// not the external key — same caveat as
	// UpsertBillingProfileCustomerOverride.
	ListInvoices(ctx context.Context, customerID CustomerID) ([]om.Invoice, error)
	// PrepareInvoicesForCustomerDeletion invoices pending lines and deletes
	// zero-total drafts so the customer can be deleted. Returns an error
	// wrapping ErrInvoicesOutstanding while invoices with a balance are not
	// yet paid, voided, or uncollectible.
	PrepareInvoicesForCustomerDeletion(ctx context.Context, customerID CustomerID) error

	// Stripe app data (see stripe.go).

	// EnsureCustomerStripeAppData upserts the customer's Stripe app data
	// (customer id and, when known, default payment method id) so
	// OpenMeter's own billing engine can charge through Stripe. Takes
	// CustomerID rather than CustomerKey: the underlying endpoint accepts
	// either (OpenMeter's customerIdOrKey union), and every caller reaches
	// this after EnsureCustomer has already resolved the internal id — using
	// it consistently here, like UpsertBillingProfileCustomerOverride and
	// ListInvoices, means CustomerKey only appears where it's actually
	// required (the initial GetCustomer/EnsureCustomer lookup and the
	// delete finalizer, which has no id to read without persisting one).
	EnsureCustomerStripeAppData(ctx context.Context, customerID CustomerID, stripeCustomerID StripeCustomerID, stripeDefaultPaymentMethodID string) error
	// GetCustomerStripeAppData reads the customer's stored Stripe app data.
	// Returns ErrStripeAppDataNotFound when there is none, and
	// ErrStripeAppNotConfigured when the customer's billing profile has no
	// Stripe app.
	GetCustomerStripeAppData(ctx context.Context, customerID CustomerID) (om.StripeCustomerAppData, error)
	// DeleteCustomerStripeAppData removes the customer's Stripe app data,
	// so its Stripe customer can be linked to another customer (OpenMeter
	// links each Stripe customer to one live customer only). Nothing
	// stored, no Stripe app, or no such customer is success.
	DeleteCustomerStripeAppData(ctx context.Context, customerKey CustomerKey) error

	// Billing profiles (see billingprofile.go).

	// EnsureBillingProfile adopts the billing profile identified by
	// existingID once its metadata proves it belongs to
	// desired.AccountKey (falling back to a lookup by account key, then to
	// creating one), and converges it to the default profile's settings
	// with desired's overrides. The result reports a refused existingID
	// and app drift for the caller to surface.
	EnsureBillingProfile(ctx context.Context, existingID string, desired DesiredBillingProfile) (EnsureBillingProfileResult, error)
	// GetBillingProfile fetches a billing profile by id. Returns
	// ErrBillingProfileNotFound when absent.
	GetBillingProfile(ctx context.Context, id string) (om.BillingProfile, error)
	// DeleteBillingProfile removes a billing profile by id. NotFound is
	// treated as success.
	DeleteBillingProfile(ctx context.Context, id string) error
	// UpsertBillingProfileCustomerOverride points the customer identified
	// by customerID at billingProfileID. customerID must be OpenMeter's
	// internal customer id (a ULID) — unlike EnsureCustomer/GetCustomer/
	// EnsureCustomerStripeAppData, this endpoint does not accept the
	// external key (see billingprofile.go for why).
	UpsertBillingProfileCustomerOverride(ctx context.Context, customerID CustomerID, billingProfileID string) error
	// DeleteBillingProfileCustomerOverride removes the customer's
	// override, reverting it to the org default. NotFound is treated as
	// success. customerID must be OpenMeter's internal customer id (a
	// ULID), same caveat as UpsertBillingProfileCustomerOverride.
	DeleteBillingProfileCustomerOverride(ctx context.Context, customerID CustomerID) error

	// Features (see feature.go).

	// EnsureFeature creates the feature if absent. An existing active feature
	// under the same key must already match desired (meter and group-by
	// filters); features are immutable in OpenMeter, so a mismatch is a
	// PermanentError rather than a silent reuse of the wrong definition.
	EnsureFeature(ctx context.Context, desired DesiredFeature) (om.Feature, error)
	// ListFeatures returns every active (non-archived) feature, optionally
	// filtered to the features bound to a single meter.
	ListFeatures(ctx context.Context, meterSlug *string) ([]om.Feature, error)
	// DeleteFeature archives the feature identified by id or key. OpenMeter
	// has no hard feature delete in this API generation — DELETE sets
	// archivedAt. NotFound is treated as success.
	DeleteFeature(ctx context.Context, idOrKey string) error
	// ArchiveFeaturesIfUnreferenced archives each member of candidateKeys
	// that no non-deleted plan version references. Features still referenced
	// by a plan (including archived versions that may carry subscriptions)
	// are left active.
	ArchiveFeaturesIfUnreferenced(ctx context.Context, candidateKeys []string) error
	// ArchiveUnreferencedMeterFeatures archives every active feature bound to
	// meterSlug that no non-deleted plan version references. Used by the
	// MeterDefinition finalizer so the meter can be deleted (OpenMeter rejects
	// deleting a meter with active features).
	ArchiveUnreferencedMeterFeatures(ctx context.Context, meterSlug string) error

	// Plans (see plan.go).

	// EnsurePlan converges the plan identified by desired.Key to a published
	// (active) version whose content matches desired. A matching active
	// version is a no-op; a pending draft is updated and published; any
	// other difference creates and publishes a new version, which makes
	// OpenMeter archive the previous active one.
	EnsurePlan(ctx context.Context, desired DesiredPlan) (om.Plan, error)
	// ListPlanVersions returns every version of the plan identified by key,
	// ordered by ascending version. Deleted versions are included only when
	// includeDeleted is set. An empty slice means no version exists.
	ListPlanVersions(ctx context.Context, key string, includeDeleted bool) ([]om.Plan, error)
	// DeletePlan deletes a single plan version, archiving it first when it
	// is active (OpenMeter refuses to delete an active plan). Already-deleted
	// versions and NotFound are treated as success.
	DeletePlan(ctx context.Context, plan om.Plan) error

	// Subscriptions (see subscription.go).

	// EnsureSubscription converges the customer to exactly one live
	// subscription on the active version of desired.PlanKey: it creates one,
	// changes another plan's subscription to it, or migrates an older
	// version, both immediately. Returns ErrCustomerNotFound,
	// ErrPlanNotPublished, or ErrCustomerBillingNotReady when it cannot.
	EnsureSubscription(ctx context.Context, desired DesiredSubscription) (SubscriptionState, error)
	// ListSubscriptions returns every subscription of the customer, live or
	// ended. Returns ErrCustomerNotFound when the customer does not exist.
	ListSubscriptions(ctx context.Context, customerKey CustomerKey) ([]om.Subscription, error)
	// GetSubscription returns a subscription with its phases and every
	// version of their items (itemTimelines), each with its price and
	// feature.
	GetSubscription(ctx context.Context, id string) (om.SubscriptionExpanded, error)
	// CancelSubscriptions ends every live subscription of the customer
	// immediately. A missing customer is treated as success.
	CancelSubscriptions(ctx context.Context, customerKey CustomerKey) error

	// Usage ingestion (see ingest.go).

	// SubmitUsageBatch ingests already-validated CloudEvents into OpenMeter.
	SubmitUsageBatch(ctx context.Context, events []cloudevents.Event) error

	// Usage queries (see usage.go).

	// QueryMeter reads a meter's aggregated usage, optionally windowed,
	// grouped and filtered by customer and group-by values. Returns
	// ErrMeterNotFound when the meter does not exist.
	QueryMeter(ctx context.Context, q MeterQuery) ([]MeterQueryRow, error)

	// Billing-account cleanup and invoice projection lookups (see
	// billingprofile.go and invoice.go).

	// FindBillingProfilesByAccountKey returns every live, non-default
	// billing profile whose metadata tags it as accountKey's, oldest first.
	FindBillingProfilesByAccountKey(ctx context.Context, accountKey CustomerKey) ([]om.BillingProfile, error)
	// CustomerIDsByKey returns the internal id of every customer, live or
	// soft-deleted, whose external key is exactly key.
	CustomerIDsByKey(ctx context.Context, key CustomerKey) ([]CustomerID, error)
	// ListInvoicesIncludingDeleted is ListInvoices plus the customer's
	// deleted invoices, so a caller can tell "deleted" from "gone".
	ListInvoicesIncludingDeleted(ctx context.Context, customerID CustomerID) ([]om.Invoice, error)
}

// client is the concrete implementation of Client. Its methods are spread
// across each resource type's own file.
type client struct {
	api *om.ClientWithResponses
}

// requestTimeout bounds every OpenMeter HTTP call. The generated SDK
// defaults to a bare &http.Client{}, which has NO timeout — and reconcile
// contexts carry no deadline of their own. With a single worker per
// controller, one hung connection would otherwise stall every reconcile
// indefinitely, with nothing logged and no error to retry on. Generous
// enough for a slow list-and-page walk, short enough that a wedged
// connection surfaces as a retryable transient failure instead.
const requestTimeout = 30 * time.Second

// NewClient builds an OpenMeter client against the given server URL,
// authenticating with a bearer token when apiSecret is non-empty.
func NewClient(server string, apiSecret string) (Client, error) {
	if strings.TrimSpace(server) == "" {
		return nil, fmt.Errorf("openmeter: empty server URL")
	}

	httpClient := om.WithHTTPClient(&http.Client{Timeout: requestTimeout})

	var api *om.ClientWithResponses
	var err error
	if apiSecret != "" {
		api, err = om.NewAuthClientWithResponses(server, apiSecret, httpClient)
	} else {
		api, err = om.NewClientWithResponses(server, httpClient)
	}
	if err != nil {
		return nil, fmt.Errorf("openmeter: new client: %w", err)
	}
	return &client{api: api}, nil
}
