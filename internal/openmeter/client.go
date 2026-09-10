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

	om "github.com/openmeterio/openmeter/api/client/go"
)

// Client is the typed interface over the OpenMeter API used by the
// reconcilers. It is the seam the controllers depend on.
type Client interface {
	// Meters (see meter.go).

	// EnsureMeter creates the meter if absent, or updates it in place to
	// match desired's mutable fields (description, groupBy). Returns a
	// PermanentError if an existing meter's immutable fields
	// (eventType/aggregation/valueProperty) disagree with desired.
	EnsureMeter(ctx context.Context, desired DesiredMeter) (om.Meter, error)
	// GetMeter fetches a meter by slug. Returns ErrMeterNotFound when absent.
	GetMeter(ctx context.Context, slug string) (om.Meter, error)
	// DeleteMeter removes a meter by slug. NotFound is treated as success.
	DeleteMeter(ctx context.Context, slug string) error

	// Customers (see customer.go).

	// EnsureCustomer creates the customer if absent, or updates it in place
	// to match desired's fields.
	EnsureCustomer(ctx context.Context, desired DesiredCustomer) (om.Customer, error)
	// GetCustomer fetches a customer by key. Returns ErrCustomerNotFound
	// when absent.
	GetCustomer(ctx context.Context, key string) (om.Customer, error)
	// DeleteCustomer removes a customer by key. NotFound is treated as
	// success.
	DeleteCustomer(ctx context.Context, key string) error

	// Invoices (see invoice.go).

	// ListInvoices returns every invoice for the customer identified by
	// customerID. An empty slice with a nil error means no invoices exist
	// yet. customerID must be OpenMeter's internal customer id (a ULID),
	// not the external key — same caveat as
	// UpsertBillingProfileCustomerOverride.
	ListInvoices(ctx context.Context, customerID string) ([]om.Invoice, error)

	// Stripe app data (see stripe.go).

	// EnsureCustomerStripeAppData upserts the customer's Stripe app data
	// (customer id and, when known, default payment method id) so
	// OpenMeter's own billing engine can charge through Stripe.
	EnsureCustomerStripeAppData(ctx context.Context, key string, stripeCustomerID string, stripeDefaultPaymentMethodID string) error

	// Billing profiles (see billingprofile.go).

	// EnsureBillingProfile creates the billing profile identified by
	// existingID if it is empty or no longer exists, or updates it in
	// place to match desired's Workflow fields.
	EnsureBillingProfile(ctx context.Context, existingID string, desired DesiredBillingProfile) (om.BillingProfile, error)
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
	UpsertBillingProfileCustomerOverride(ctx context.Context, customerID string, billingProfileID string) error
	// DeleteBillingProfileCustomerOverride removes the customer's
	// override, reverting it to the org default. NotFound is treated as
	// success. customerID must be OpenMeter's internal customer id (a
	// ULID), same caveat as UpsertBillingProfileCustomerOverride.
	DeleteBillingProfileCustomerOverride(ctx context.Context, customerID string) error
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
