// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"
	"net/http"

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
	Key CustomerKey
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
}

// Usage attribution: OpenMeter attributes a usage event to the customer
// whose key, or one of whose usageAttribution.subjectKeys, equals the
// event's subject (streaming.CustomerUsageAttribution.GetValues). The
// submission consumer sets every event's subject to its BillingAccount's
// UID — the customer key — so attribution needs no subject keys at all, and
// binding or unbinding projects never changes the customer. That matters
// because OpenMeter refuses any change to subjectKeys while the customer has
// a live subscription. New customers are therefore created with none, and
// updates send back exactly the keys OpenMeter already holds: customers
// created before this change keep their legacy "projects/<name>" keys, so
// usage ingested under those subjects stays attributed to them.

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

// wireAddress renders a into the OpenMeter wire shape, shaped around how
// OpenMeter's customer adapter (openmeter/customer/adapter/customer.go,
// UpdateCustomer) actually applies a replace-PUT:
//
//   - billingAddress omitted (nil) clears EVERY address column. So a nil a
//     renders as nil, which is the only way to remove an address.
//   - billingAddress present writes each field with SetNillable*: a nil
//     field is a no-op that keeps the stored value, while a pointer to ""
//     stores "". Sending nil for an emptied field therefore never cleared
//     it, and customerNeedsUpdate saw the drift on every reconcile, forever.
//     Optional fields are sent as explicit pointers, "" included; the
//     OpenAPI Address schema puts no length/pattern constraint on them.
//
// Country is the exception: CountryCode is pattern ^[A-Z]{2}$, so "" would
// be rejected — it's only sent when set (BillingAddress.Country is required
// by the CRD, so that's always). PhoneNumber has no Milo counterpart and is
// left nil, i.e. untouched.
func wireAddress(a *Address) *om.Address {
	if a == nil {
		return nil
	}
	wire := &om.Address{
		Line1:      stringPtr(a.Line1),
		Line2:      stringPtr(a.Line2),
		City:       stringPtr(a.City),
		State:      stringPtr(a.Region),
		PostalCode: stringPtr(a.PostalCode),
	}
	if a.Country != "" {
		country := a.Country
		wire.Country = &country
	}
	return wire
}

// addressFromWire is wireAddress's inverse, normalizing a nil pointer, a nil
// field, and a field stored as "" to the same zero value, so
// customerNeedsUpdate compares what the address means rather than which
// shape the server happens to return it in: a cleared field comes back as
// "" (stored by wireAddress's explicit empty pointer), a never-set one as
// absent. PhoneNumber is ignored — wireAddress never writes it.
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

// stringPtr returns a pointer to a copy of s, "" included.
func stringPtr(s string) *string {
	return &s
}

// nonNilStrings returns in, or an empty (non-nil) slice if in is nil.
// CustomerUsageAttribution.SubjectKeys has no `omitempty` and OpenMeter's
// schema explicitly rejects `null` for it ("Value is not nullable") even
// though an empty array is fine.
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
	return c.updateCustomer(ctx, existing, desired)
}

// customerNeedsUpdate reports whether existing's mutable fields (name,
// email, currency, address) disagree with desired. Subject keys are not
// compared: they are never changed (see the usage-attribution note on
// DesiredCustomer). Key is not
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
	return addressFromWire(existing.BillingAddress) != normalizeAddress(desired.Address)
}

// existingSubjectKeys returns the subject keys OpenMeter holds for c, so an
// update can send them back unchanged (the PUT is a replace; omitting
// usageAttribution would drop them).
func existingSubjectKeys(c om.Customer) []string {
	if c.UsageAttribution == nil {
		return []string{}
	}
	return nonNilStrings(c.UsageAttribution.SubjectKeys)
}

// createCustomer POSTs a new customer.
func (c *client) createCustomer(ctx context.Context, desired DesiredCustomer) (om.Customer, error) {
	key := string(desired.Key)
	body := om.CustomerCreate{
		Name:           desired.Name,
		Key:            &key,
		BillingAddress: wireAddress(desired.Address),
		// No subject keys: usage is attributed by the customer key.
		UsageAttribution: &om.CustomerUsageAttribution{SubjectKeys: []string{}},
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
func (c *client) updateCustomer(ctx context.Context, existing om.Customer, desired DesiredCustomer) (om.Customer, error) {
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
	key := string(desired.Key)
	body := om.CustomerReplaceUpdate{
		Name:           desired.Name,
		Key:            &key,
		PrimaryEmail:   &desired.Email,
		BillingAddress: wireAddress(desired.Address),
		// Sent back unchanged: OpenMeter refuses subject-key changes while
		// the customer has a live subscription.
		UsageAttribution: &om.CustomerUsageAttribution{SubjectKeys: existingSubjectKeys(existing)},
	}
	if desired.Currency != "" {
		body.Currency = &desired.Currency
	}

	resp, err := c.api.UpdateCustomerWithResponse(ctx, key, body)
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
func (c *client) GetCustomer(ctx context.Context, key CustomerKey) (om.Customer, error) {
	if key == "" {
		return om.Customer{}, &PermanentError{Err: errors.New("key is required")}
	}

	resp, err := c.api.GetCustomerWithResponse(ctx, string(key), nil)
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
func (c *client) DeleteCustomer(ctx context.Context, key CustomerKey) error {
	if key == "" {
		return &PermanentError{Err: errors.New("key is required")}
	}

	resp, err := c.api.DeleteCustomerWithResponse(ctx, string(key))
	if err != nil {
		return classify(nil, nil, err)
	}
	if resp.StatusCode() == 404 {
		return nil
	}
	return classify(resp.HTTPResponse, resp.Body, nil)
}

// ReleaseCustomerSubjects clears the customer's usageAttribution.subjectKeys
// and leaves every other field as it is. Used by the BillingAccount
// finalizer for customers created before usage was attributed by customer
// key, which still carry their projects' "projects/<name>" keys: a deleting
// account's customer can linger for weeks waiting on unpaid invoices, and
// should not keep holding subjects it no longer bills.
//
// A missing customer, or one with no subject keys, is a no-op. OpenMeter
// refuses to change subject keys while the customer has a subscription
// active at the time of the request (openmeter/subscription/validators/
// customer ValidateUpdateCustomer), so callers must cancel subscriptions
// first; an explicit empty list (rather than an omitted usageAttribution,
// which the adapter would also treat as "remove all") is sent so that check
// is applied rather than bypassed.
//
// The replace-PUT clears description and metadata when they're omitted
// (SetOrClearDescription / ClearMetadata in the customer adapter), so both
// are echoed back from the current customer along with the rest; the
// address is echoed whole, so wireAddress's nil-clears-everything rule
// keeps it exactly as stored.
func (c *client) ReleaseCustomerSubjects(ctx context.Context, key CustomerKey) error {
	existing, err := c.GetCustomer(ctx, key)
	switch {
	case errors.Is(err, ErrCustomerNotFound):
		return nil
	case err != nil:
		return err
	}
	if existing.UsageAttribution == nil || len(existing.UsageAttribution.SubjectKeys) == 0 {
		return nil
	}

	body := om.CustomerReplaceUpdate{
		Name:             existing.Name,
		Key:              existing.Key,
		Description:      existing.Description,
		Metadata:         existing.Metadata,
		PrimaryEmail:     existing.PrimaryEmail,
		Currency:         existing.Currency,
		BillingAddress:   existing.BillingAddress,
		UsageAttribution: &om.CustomerUsageAttribution{SubjectKeys: []string{}},
	}
	resp, err := c.api.UpdateCustomerWithResponse(ctx, string(key), body)
	if err != nil {
		return classify(nil, nil, err)
	}
	if resp.StatusCode() == http.StatusNotFound {
		return nil
	}
	return classify(resp.HTTPResponse, resp.Body, nil)
}
