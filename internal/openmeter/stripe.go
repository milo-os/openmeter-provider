// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// ErrStripeAppNotConfigured marks a Stripe app-data call OpenMeter refused
// because the customer's billing profile has no Stripe app at all — e.g. a
// dev/sandbox install whose default profile uses the Sandbox app. OpenMeter
// resolves the app from the customer's merged billing profile
// (billing.Service.GetCustomerApp) and answers 412 "no stripe app found in
// billing profile for customer ...". Nothing this controller does can fix
// that, so callers report it once rather than retrying it as a failure.
var ErrStripeAppNotConfigured = errors.New("openmeter: customer's billing profile has no Stripe app")

// ErrStripeAppDataNotFound is returned by GetCustomerStripeAppData when the
// customer has no Stripe app data yet (OpenMeter's 412 "customer has no
// data for stripe app"), or no longer exists.
var ErrStripeAppDataNotFound = errors.New("openmeter: customer has no Stripe app data")

// Fixed parts of the 412 messages OpenMeter returns for the two conditions
// above (billing/service/customeroverride.go GetCustomerApp, and
// app/stripe/adapter/customer.go GetStripeCustomerData). Both are 412s with
// no machine-readable code that tells them apart from the other Stripe
// precondition failures (an unknown Stripe customer, a payment method that
// isn't the customer's), so the message is the only discriminator.
const (
	stripeAppNotConfiguredMarker = "no stripe app found in billing profile"
	stripeAppDataNotFoundMarker  = "customer has no data for stripe app"
)

// classifyStripeAppData is classify, plus detection of the two 412s above.
// Both stay PermanentErrors (so nothing treats them as retryable by
// accident); the sentinel is wrapped in so callers can tell them apart.
func classifyStripeAppData(resp *http.Response, body []byte) error {
	err := classify(resp, body, nil)
	var perm *PermanentError
	if !errors.As(err, &perm) || perm.StatusCode != http.StatusPreconditionFailed {
		return err
	}
	switch {
	case strings.Contains(perm.ResponseBody, stripeAppNotConfiguredMarker):
		perm.Err = fmt.Errorf("%w: %s", ErrStripeAppNotConfigured, perm.ResponseBody)
	case strings.Contains(perm.ResponseBody, stripeAppDataNotFoundMarker):
		perm.Err = fmt.Errorf("%w: %s", ErrStripeAppDataNotFound, perm.ResponseBody)
	}
	return perm
}

// EnsureCustomerStripeAppData upserts the customer's Stripe app data so
// OpenMeter's own billing engine can charge through Stripe. Unlike
// amberflo-provider's scheduleStripePaymentSwitch, this is an immediate
// assignment, not a future-dated transition: OpenMeter has no concept of a
// scheduled payment-method switch, and its billing engine simply uses
// whatever Stripe customer/payment-method id is current at its own next
// invoicing cycle.
//
// stripeDefaultPaymentMethodID may be empty, and an empty one CLEARS any
// previously stored default payment method: the upsert's ON CONFLICT
// updates stripe_default_payment_method_id from the inserted row, which is
// NULL when the field is omitted (app/stripe/adapter/customer.go
// UpsertStripeCustomerData).
//
// The app id is omitted from the request, so OpenMeter resolves the Stripe
// app from the customer's merged billing profile (app/httpdriver
// resolveCustomerApp → billing GetCustomerApp) — the per-account profile
// this controller creates, which inherits its apps from the org default
// profile. If that profile has no Stripe app, the error wraps
// ErrStripeAppNotConfigured.
func (c *client) EnsureCustomerStripeAppData(ctx context.Context, customerID CustomerID, stripeCustomerID StripeCustomerID, stripeDefaultPaymentMethodID string) error {
	if customerID == "" {
		return &PermanentError{Err: errors.New("customerID is required")}
	}
	if stripeCustomerID == "" {
		return &PermanentError{Err: errors.New("stripeCustomerID is required")}
	}

	// Compare before writing. Unlike the other Ensure* calls this one is not
	// cheap: OpenMeter validates the ids against the *real Stripe API* on
	// every upsert (that validation is what produces "stripe customer ...
	// not found in stripe account" / "payment method ... does not belong
	// to"), so an unconditional write means an outbound third-party call on
	// every single reconcile. A NotFound (no app data yet) or any read
	// failure just falls through to the write — the upsert is the source of
	// truth, this is only an optimization.
	if current, err := c.GetCustomerStripeAppData(ctx, customerID); err == nil &&
		current.StripeCustomerId == string(stripeCustomerID) &&
		derefOrEmpty(current.StripeDefaultPaymentMethodId) == stripeDefaultPaymentMethodID {
		return nil
	}

	item := om.StripeCustomerAppDataCreateOrUpdateItem{
		Type:             om.StripeCustomerAppDataCreateOrUpdateItemTypeStripe,
		StripeCustomerId: string(stripeCustomerID),
	}
	if stripeDefaultPaymentMethodID != "" {
		item.StripeDefaultPaymentMethodId = &stripeDefaultPaymentMethodID
	}
	var entry om.CustomerAppDataCreateOrUpdateItem
	if err := entry.FromStripeCustomerAppDataCreateOrUpdateItem(item); err != nil {
		return &PermanentError{Err: err}
	}

	resp, err := c.api.UpsertCustomerAppDataWithResponse(ctx, string(customerID), []om.CustomerAppDataCreateOrUpdateItem{entry})
	if err != nil {
		return classify(nil, nil, err)
	}
	return classifyStripeAppData(resp.HTTPResponse, resp.Body)
}

// GetCustomerStripeAppData reads the customer's currently-stored Stripe app
// data. Returns an error wrapping ErrStripeAppDataNotFound when there is
// none (or the customer is gone), and one wrapping ErrStripeAppNotConfigured
// when the customer's billing profile has no Stripe app. Reading is a
// database lookup on OpenMeter's side — unlike the upsert, it makes no
// Stripe API call.
func (c *client) GetCustomerStripeAppData(ctx context.Context, customerID CustomerID) (om.StripeCustomerAppData, error) {
	if customerID == "" {
		return om.StripeCustomerAppData{}, &PermanentError{Err: errors.New("customerID is required")}
	}
	resp, err := c.api.GetCustomerStripeAppDataWithResponse(ctx, string(customerID))
	if err != nil {
		return om.StripeCustomerAppData{}, classify(nil, nil, err)
	}
	if resp.StatusCode() == http.StatusNotFound {
		return om.StripeCustomerAppData{}, fmt.Errorf("%w: customer %s not found", ErrStripeAppDataNotFound, customerID)
	}
	if err := classifyStripeAppData(resp.HTTPResponse, resp.Body); err != nil {
		return om.StripeCustomerAppData{}, err
	}
	if resp.JSON200 == nil {
		return om.StripeCustomerAppData{}, &TransientError{Err: errors.New("get customer stripe app data: empty response body")}
	}
	return *resp.JSON200, nil
}

// DeleteCustomerStripeAppData implements Client. It reads the stored data
// first because the delete endpoint is per app, and the read is what names
// the Stripe app the data belongs to (its id field is the app id).
func (c *client) DeleteCustomerStripeAppData(ctx context.Context, customerKey CustomerKey) error {
	if customerKey == "" {
		return &PermanentError{Err: errors.New("customerKey is required")}
	}
	// The endpoint takes an id or a key (customerIdOrKey).
	data, err := c.GetCustomerStripeAppData(ctx, CustomerID(customerKey))
	switch {
	case errors.Is(err, ErrStripeAppDataNotFound), errors.Is(err, ErrStripeAppNotConfigured):
		return nil
	case err != nil:
		return err
	case data.Id == nil || *data.Id == "":
		return &TransientError{Err: fmt.Errorf("stripe app data of customer %s names no app", customerKey)}
	}

	resp, err := c.api.DeleteCustomerAppDataWithResponse(ctx, string(customerKey), *data.Id)
	if err != nil {
		return classify(nil, nil, err)
	}
	if resp.StatusCode() == http.StatusNotFound {
		return nil
	}
	return classify(resp.HTTPResponse, resp.Body, nil)
}

// derefOrEmpty returns the pointed-to string, or "" when the pointer is nil.
func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
