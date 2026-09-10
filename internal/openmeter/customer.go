// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"
	"slices"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// DesiredCustomer is the controller-facing representation of a customer the
// reconciler wants to exist in OpenMeter. Mirrors DesiredMeter: callers
// assemble this struct; wire encoding lives here.
type DesiredCustomer struct {
	// Key is OpenMeter's external customer key. For the openmeter-provider
	// this is always the Milo BillingAccount.metadata.uid — stable across
	// renames, mirroring the amberflo-provider's customerId convention.
	// OpenMeter's customerIdOrKey path parameter matches on either its own
	// server-assigned id OR this key, so Get/Ensure/Delete all address the
	// customer by Key alone; the server-assigned id never needs tracking.
	Key string
	// Name is the customer's display name. Callers should follow
	// billingv1alpha1.BillingContactInfo's own documented convention:
	// BusinessName if set, else ContactInfo.Name, else the BillingAccount's
	// k8s object name as a guaranteed-non-empty fallback (OpenMeter requires
	// Name).
	Name string
	// Email is the primary contact email. May be empty.
	Email string
	// Currency is an ISO 4217 code. May be empty.
	Currency string
	// Address is the billing postal address. Nil when the caller has none
	// to report (e.g. BillingContactInfo.Address is unset).
	Address *Address
	// SubjectKeys is the canonical list of Milo projects bound to this
	// billing account, attributed to it via OpenMeter's usageAttribution so
	// metered usage tagged with these subjects rolls up to this customer.
	// Sent as an explicit (possibly empty) list rather than omitted so a
	// project being unbound actually clears the attribution.
	SubjectKeys []string
}

// Address is the controller-facing representation of a billing postal
// address. Field names mirror billingv1alpha1.BillingAddress (Region, not
// State) so callers don't need OpenMeter SDK knowledge; the client
// translates Region to om.Address.State on the wire.
type Address struct {
	// Country is required whenever Address is non-nil (mirrors
	// BillingAddress.Country's own +kubebuilder:validation:Required).
	Country    string
	Line1      string
	Line2      string
	City       string
	Region     string
	PostalCode string
}

// wireAddress renders a into the OpenMeter wire shape. Always returns a
// non-nil *om.Address (a nil a renders as an all-nil-fields object) rather
// than letting CustomerCreate/CustomerReplaceUpdate's own `omitempty` on
// BillingAddress drop the key entirely — mirrors the SubjectKeys handling
// below: an explicit empty value is what actually clears a previously-set
// address on update, whereas an omitted key's effect on OpenMeter's REPLACE
// semantics is untested and not something to rely on silently.
func wireAddress(a *Address) *om.Address {
	wire := &om.Address{}
	if a == nil {
		return wire
	}
	if a.Country != "" {
		country := a.Country
		wire.Country = &country
	}
	wire.Line1 = stringPtrOrNil(a.Line1)
	wire.Line2 = stringPtrOrNil(a.Line2)
	wire.City = stringPtrOrNil(a.City)
	wire.State = stringPtrOrNil(a.Region)
	wire.PostalCode = stringPtrOrNil(a.PostalCode)
	return wire
}

// addressFromWire is wireAddress's inverse, normalizing both a nil pointer
// and a non-nil-but-all-empty-fields object to the same zero Address value
// so customerNeedsUpdate can compare regardless of which shape the server
// (or our own wireAddress) happens to use for "no address".
func addressFromWire(a *om.Address) Address {
	if a == nil {
		return Address{}
	}
	var out Address
	if a.Country != nil {
		out.Country = *a.Country
	}
	if a.Line1 != nil {
		out.Line1 = *a.Line1
	}
	if a.Line2 != nil {
		out.Line2 = *a.Line2
	}
	if a.City != nil {
		out.City = *a.City
	}
	if a.State != nil {
		out.Region = *a.State
	}
	if a.PostalCode != nil {
		out.PostalCode = *a.PostalCode
	}
	return out
}

// normalizeAddress collapses a nil *Address to the same zero Address value
// addressFromWire produces for "no address", so the two are comparable with
// plain equality regardless of which side is nil.
func normalizeAddress(a *Address) Address {
	if a == nil {
		return Address{}
	}
	return *a
}

// stringPtrOrNil returns nil for an empty string, else a pointer to s.
func stringPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// nonNilStrings returns in, or an empty (non-nil) slice if in is nil.
// CustomerUsageAttribution.SubjectKeys has no `omitempty` and OpenMeter's
// schema explicitly rejects `null` for it ("Value is not nullable") even
// though an empty array is fine — a nil desired.SubjectKeys (e.g. a
// BillingAccount with no bound projects) would otherwise marshal to
// `"subjectKeys":null` and get a permanent 400 on every reconcile.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// EnsureCustomer creates or updates the customer so OpenMeter matches
// desired. The call is idempotent: if OpenMeter already agrees, no write
// happens and only the GET is issued.
func (c *client) EnsureCustomer(ctx context.Context, desired DesiredCustomer) (om.Customer, error) {
	if desired.Key == "" {
		return om.Customer{}, &PermanentError{Err: errors.New("DesiredCustomer.Key is required")}
	}
	if desired.Name == "" {
		return om.Customer{}, &PermanentError{Err: errors.New("DesiredCustomer.Name is required")}
	}

	existing, err := c.GetCustomer(ctx, desired.Key)
	switch {
	case errors.Is(err, ErrCustomerNotFound):
		return c.createCustomer(ctx, desired)
	case err != nil:
		return om.Customer{}, err
	}

	if !customerNeedsUpdate(existing, desired) {
		return existing, nil
	}
	return c.updateCustomer(ctx, desired)
}

// customerNeedsUpdate reports whether existing's mutable fields (name,
// email, currency, usage attribution) disagree with desired. Key is not
// compared: existing was fetched by desired.Key, so it always matches.
func customerNeedsUpdate(existing om.Customer, desired DesiredCustomer) bool {
	if existing.Name != desired.Name {
		return true
	}
	existingEmail := ""
	if existing.PrimaryEmail != nil {
		existingEmail = *existing.PrimaryEmail
	}
	if existingEmail != desired.Email {
		return true
	}
	// Currency is only compared when the caller actually has one. OpenMeter
	// rejects an empty currency outright ("minimum string length is 3",
	// confirmed live), so updateCustomer cannot clear it and comparing
	// ""-vs-set would report drift that no update could ever resolve — an
	// update on every reconcile, forever. Not a real limitation: currency
	// is immutable past Provisioning per BillingAccount's own CRD docs.
	if desired.Currency != "" {
		existingCurrency := ""
		if existing.Currency != nil {
			existingCurrency = *existing.Currency
		}
		if existingCurrency != desired.Currency {
			return true
		}
	}
	if addressFromWire(existing.BillingAddress) != normalizeAddress(desired.Address) {
		return true
	}
	var existingSubjectKeys []string
	if existing.UsageAttribution != nil {
		existingSubjectKeys = existing.UsageAttribution.SubjectKeys
	}
	return !slices.Equal(existingSubjectKeys, desired.SubjectKeys)
}

// createCustomer POSTs a new customer.
func (c *client) createCustomer(ctx context.Context, desired DesiredCustomer) (om.Customer, error) {
	body := om.CustomerCreate{
		Name:             desired.Name,
		Key:              &desired.Key,
		BillingAddress:   wireAddress(desired.Address),
		UsageAttribution: &om.CustomerUsageAttribution{SubjectKeys: nonNilStrings(desired.SubjectKeys)},
	}
	if desired.Email != "" {
		body.PrimaryEmail = &desired.Email
	}
	if desired.Currency != "" {
		body.Currency = &desired.Currency
	}

	resp, err := c.api.CreateCustomerWithResponse(ctx, body)
	if err != nil {
		return om.Customer{}, classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return om.Customer{}, err
	}
	if resp.JSON201 != nil {
		return *resp.JSON201, nil
	}
	// See meter.go's createMeter for why a 2xx with an unparseable body
	// falls back to a GET instead of failing closed.
	created, err := c.GetCustomer(ctx, desired.Key)
	if err != nil {
		return om.Customer{}, &PermanentError{Err: fmt.Errorf(
			"create customer %q: got %s with an unparseable body, and the follow-up GetCustomer also failed: %w",
			desired.Key, resp.Status(), err)}
	}
	return created, nil
}

// updateCustomer PUTs a full replacement of the customer's mutable fields.
func (c *client) updateCustomer(ctx context.Context, desired DesiredCustomer) (om.Customer, error) {
	// PrimaryEmail is sent unconditionally, including as an explicit empty
	// string. Despite being a REPLACE, OpenMeter preserves fields omitted
	// from the body (confirmed live: PUT without primaryEmail left the old
	// address in place), so omitting it when the caller has no email means
	// clearing contactInfo.email never propagates — and customerNeedsUpdate
	// then sees stale-vs-empty forever, updating on every single reconcile.
	// An explicit "" is accepted and does clear it (also confirmed live).
	// Currency gets the opposite treatment: OpenMeter rejects "" with
	// "minimum string length is 3", so it stays omitted (see
	// customerNeedsUpdate for how that's kept from looping).
	body := om.CustomerReplaceUpdate{
		Name:             desired.Name,
		Key:              &desired.Key,
		PrimaryEmail:     &desired.Email,
		BillingAddress:   wireAddress(desired.Address),
		UsageAttribution: &om.CustomerUsageAttribution{SubjectKeys: nonNilStrings(desired.SubjectKeys)},
	}
	if desired.Currency != "" {
		body.Currency = &desired.Currency
	}

	resp, err := c.api.UpdateCustomerWithResponse(ctx, desired.Key, body)
	if err != nil {
		return om.Customer{}, classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return om.Customer{}, err
	}
	if resp.JSON200 != nil {
		return *resp.JSON200, nil
	}
	updated, err := c.GetCustomer(ctx, desired.Key)
	if err != nil {
		return om.Customer{}, &PermanentError{Err: fmt.Errorf(
			"update customer %q: got %s with an unparseable body, and the follow-up GetCustomer also failed: %w",
			desired.Key, resp.Status(), err)}
	}
	return updated, nil
}

// GetCustomer fetches a customer by its key. Returns ErrCustomerNotFound
// when no such customer exists (including one that was previously deleted —
// OpenMeter's key lookup excludes soft-deleted records).
func (c *client) GetCustomer(ctx context.Context, key string) (om.Customer, error) {
	if key == "" {
		return om.Customer{}, &PermanentError{Err: errors.New("key is required")}
	}

	resp, err := c.api.GetCustomerWithResponse(ctx, key, nil)
	if err != nil {
		return om.Customer{}, classify(nil, nil, err)
	}
	if resp.StatusCode() == 404 {
		return om.Customer{}, fmt.Errorf("%w: %s", ErrCustomerNotFound, key)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return om.Customer{}, err
	}
	if resp.JSON200 == nil {
		return om.Customer{}, &PermanentError{Err: fmt.Errorf("get customer %q: empty response body", key)}
	}
	return *resp.JSON200, nil
}

// DeleteCustomer removes a customer keyed by key. NotFound is treated as
// success — the desired end state is "no customer in OpenMeter", and
// absence satisfies that goal.
func (c *client) DeleteCustomer(ctx context.Context, key string) error {
	if key == "" {
		return &PermanentError{Err: errors.New("key is required")}
	}

	resp, err := c.api.DeleteCustomerWithResponse(ctx, key)
	if err != nil {
		return classify(nil, nil, err)
	}
	if resp.StatusCode() == 404 {
		return nil
	}
	return classify(resp.HTTPResponse, resp.Body, nil)
}
