// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// EnsureCustomerStripeAppData upserts the customer's Stripe app data so
// OpenMeter's own billing engine can charge through Stripe. Unlike
// amberflo-provider's scheduleStripePaymentSwitch, this is an immediate
// assignment, not a future-dated transition: OpenMeter has no concept of a
// scheduled payment-method switch, and its billing engine simply uses
// whatever Stripe customer/payment-method id is current at its own next
// invoicing cycle. stripeDefaultPaymentMethodID may be empty; the app id
// itself is omitted so OpenMeter uses the instance's default Stripe app
// (an operator/deployment precondition, not something this reconciler
// provisions).
func (c *client) EnsureCustomerStripeAppData(ctx context.Context, key string, stripeCustomerID string, stripeDefaultPaymentMethodID string) error {
	if key == "" {
		return &PermanentError{Err: errors.New("key is required")}
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
	if current, err := c.getCustomerStripeAppData(ctx, key); err == nil &&
		current.StripeCustomerId == stripeCustomerID &&
		derefOrEmpty(current.StripeDefaultPaymentMethodId) == stripeDefaultPaymentMethodID {
		return nil
	}

	item := om.StripeCustomerAppDataCreateOrUpdateItem{
		Type:             om.StripeCustomerAppDataCreateOrUpdateItemTypeStripe,
		StripeCustomerId: stripeCustomerID,
	}
	if stripeDefaultPaymentMethodID != "" {
		item.StripeDefaultPaymentMethodId = &stripeDefaultPaymentMethodID
	}
	var entry om.CustomerAppDataCreateOrUpdateItem
	if err := entry.FromStripeCustomerAppDataCreateOrUpdateItem(item); err != nil {
		return &PermanentError{Err: err}
	}

	resp, err := c.api.UpsertCustomerAppDataWithResponse(ctx, key, []om.CustomerAppDataCreateOrUpdateItem{entry})
	if err != nil {
		return classify(nil, nil, err)
	}
	return classify(resp.HTTPResponse, resp.Body, nil)
}

// getCustomerStripeAppData reads the customer's currently-stored Stripe app
// data. Only used as a drift check by EnsureCustomerStripeAppData, so any
// error (including "no app data yet") simply means "cannot prove it already
// matches" and the caller proceeds with the write.
func (c *client) getCustomerStripeAppData(ctx context.Context, key string) (om.StripeCustomerAppData, error) {
	resp, err := c.api.GetCustomerStripeAppDataWithResponse(ctx, key)
	if err != nil {
		return om.StripeCustomerAppData{}, classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return om.StripeCustomerAppData{}, err
	}
	if resp.JSON200 == nil {
		return om.StripeCustomerAppData{}, errors.New("get customer stripe app data: empty response body")
	}
	return *resp.JSON200, nil
}

// derefOrEmpty returns the pointed-to string, or "" when the pointer is nil.
func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
