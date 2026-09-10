// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// validateUsageAttribution mirrors OpenMeter's own server-side rule
// (observed directly against a real instance): usageAttribution.subjectKeys
// must never be JSON null, even though an empty array is fine. A nil Go
// slice on the wire marshals to null, so this catches a client regression
// (e.g. a BillingAccount with no bound projects producing a nil
// desired.SubjectKeys that reaches the wire unguarded) the same way the
// real API would reject it, instead of the fake silently accepting it.
func validateUsageAttribution(u *om.CustomerUsageAttribution) error {
	if u != nil && u.SubjectKeys == nil {
		return errors.New(`Error at "/usageAttribution/subjectKeys": Value is not nullable`)
	}
	return nil
}

// validateCurrency mirrors OpenMeter's ISO4217 schema constraint: an
// explicitly-sent empty currency is rejected with "minimum string length is
// 3" (verified live). Enforcing it here keeps the client honest about only
// ever sending a currency it actually has — the asymmetry with
// primaryEmail, which DOES accept an explicit empty string, is the whole
// reason customerNeedsUpdate treats the two fields differently.
func validateCurrency(c *om.CurrencyCode) error {
	if c != nil && len(*c) != 3 {
		return errors.New(`Error at "/currency": minimum string length is 3`)
	}
	return nil
}

// serveCustomers handles /api/v1/customers[/{key}] requests. Returns false
// when the request path isn't a customers route, so ServeHTTP can fall
// through to other resources. Customers are keyed by their external Key,
// matching how this test client always addresses them — mirrors OpenMeter's
// own customerIdOrKey Or-query semantics closely enough for these tests.
func (f *fakeServer) serveCustomers(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/v1/customers") {
		return false
	}
	key := strings.TrimPrefix(r.URL.Path, "/api/v1/customers/")
	key = strings.TrimSuffix(key, "/")

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/customers":
		var body om.CustomerCreate
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		bodyKey := ""
		if body.Key != nil {
			bodyKey = *body.Key
		}
		if _, exists := f.customers[bodyKey]; bodyKey != "" && exists {
			w.WriteHeader(http.StatusConflict)
			return true
		}
		if err := validateUsageAttribution(body.UsageAttribution); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return true
		}
		m := om.Customer{
			Id:               "id-" + bodyKey,
			Key:              body.Key,
			Name:             body.Name,
			PrimaryEmail:     body.PrimaryEmail,
			Currency:         body.Currency,
			BillingAddress:   body.BillingAddress,
			UsageAttribution: body.UsageAttribution,
		}
		f.customers[bodyKey] = m
		if !f.emptyBodyOnWrite {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(http.StatusCreated)
		if !f.emptyBodyOnWrite {
			_ = json.NewEncoder(w).Encode(m)
		}

	case r.Method == http.MethodGet && key != "":
		m, ok := f.customers[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(m)

	case r.Method == http.MethodPut && key != "":
		m, ok := f.customers[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		var body om.CustomerReplaceUpdate
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		if err := validateUsageAttribution(body.UsageAttribution); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return true
		}
		if err := validateCurrency(body.Currency); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return true
		}
		f.customerUpdates++
		m.Name = body.Name
		m.BillingAddress = body.BillingAddress
		m.UsageAttribution = body.UsageAttribution
		// Despite being a REPLACE, the real OpenMeter PRESERVES fields
		// omitted from the body rather than clearing them — verified live.
		// The fake used to clear them, which is exactly what hid the bug
		// where omitting an empty primaryEmail meant a cleared email never
		// propagated AND customerNeedsUpdate saw drift forever.
		if body.PrimaryEmail != nil {
			m.PrimaryEmail = body.PrimaryEmail
		}
		if body.Currency != nil {
			m.Currency = body.Currency
		}
		f.customers[key] = m
		if !f.emptyBodyOnWrite {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(http.StatusOK)
		if !f.emptyBodyOnWrite {
			_ = json.NewEncoder(w).Encode(m)
		}

	case r.Method == http.MethodDelete && key != "":
		if _, ok := f.customers[key]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		delete(f.customers, key)
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
	return true
}

func baseDesiredCustomer() DesiredCustomer {
	return DesiredCustomer{
		Key:         "ba-uid-1",
		Name:        "acme-prod",
		Email:       "billing@acme.example",
		Currency:    "USD",
		SubjectKeys: []string{"project-a", "project-b"},
	}
}

func TestEnsureCustomer_CreatesWhenAbsent(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredCustomer()

	got, err := c.EnsureCustomer(context.Background(), desired)
	if err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	if got.Key == nil || *got.Key != desired.Key {
		t.Errorf("Key = %v, want %q", got.Key, desired.Key)
	}
	if got.Name != desired.Name {
		t.Errorf("Name = %q, want %q", got.Name, desired.Name)
	}
	if got.PrimaryEmail == nil || *got.PrimaryEmail != desired.Email {
		t.Errorf("PrimaryEmail = %v, want %q", got.PrimaryEmail, desired.Email)
	}
	if got.UsageAttribution == nil || len(got.UsageAttribution.SubjectKeys) != 2 {
		t.Errorf("UsageAttribution = %v, want SubjectKeys=%v", got.UsageAttribution, desired.SubjectKeys)
	}
	if _, ok := f.customers[desired.Key]; !ok {
		t.Errorf("customer %q not stored", desired.Key)
	}
}

func TestEnsureCustomer_NoopWhenUpToDate(t *testing.T) {
	c, _ := newTestClient(t)
	desired := baseDesiredCustomer()

	first, err := c.EnsureCustomer(context.Background(), desired)
	if err != nil {
		t.Fatalf("first EnsureCustomer: %v", err)
	}
	second, err := c.EnsureCustomer(context.Background(), desired)
	if err != nil {
		t.Fatalf("second EnsureCustomer: %v", err)
	}
	if first.Id != second.Id {
		t.Errorf("Id changed across a no-drift EnsureCustomer: %q -> %q", first.Id, second.Id)
	}
}

func TestEnsureCustomer_UpdatesMutableFields(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredCustomer()
	if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
		t.Fatalf("first EnsureCustomer: %v", err)
	}

	desired.Name = "acme-prod-renamed"
	desired.Email = "new-billing@acme.example"
	desired.SubjectKeys = []string{"project-a"}
	got, err := c.EnsureCustomer(context.Background(), desired)
	if err != nil {
		t.Fatalf("second EnsureCustomer: %v", err)
	}
	if got.Name != desired.Name {
		t.Errorf("Name = %q, want %q", got.Name, desired.Name)
	}
	if stored := f.customers[desired.Key]; stored.UsageAttribution == nil || len(stored.UsageAttribution.SubjectKeys) != 1 {
		t.Errorf("SubjectKeys not updated: %v", stored.UsageAttribution)
	}
}

func TestEnsureCustomer_SetsAddress(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredCustomer()
	desired.Address = &Address{Country: "US", Line1: "1 Infinite Loop", City: "Cupertino", Region: "CA", PostalCode: "95014"}

	got, err := c.EnsureCustomer(context.Background(), desired)
	if err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	if got.BillingAddress == nil {
		t.Fatal("BillingAddress is nil")
	}
	if got.BillingAddress.Country == nil || *got.BillingAddress.Country != "US" {
		t.Errorf("Country = %v, want US", got.BillingAddress.Country)
	}
	if got.BillingAddress.State == nil || *got.BillingAddress.State != "CA" {
		t.Errorf("State = %v, want CA (from Address.Region)", got.BillingAddress.State)
	}

	// A second EnsureCustomer with the same Address must be a no-op — the
	// wire round-trip (Address -> om.Address -> back via addressFromWire)
	// has to compare equal to itself, not spuriously trigger an update.
	before := f.customers[desired.Key].UpdatedAt
	if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
		t.Fatalf("second EnsureCustomer: %v", err)
	}
	if after := f.customers[desired.Key].UpdatedAt; !before.Equal(after) {
		t.Errorf("UpdateCustomer was called on a no-drift EnsureCustomer with an Address set")
	}
}

// TestEnsureCustomer_ClearsAddressWhenUnset guards wireAddress's "always
// send an explicit object, never omit the key" choice: if a
// BillingAccount's contactInfo.address is removed, the previously-set
// OpenMeter billingAddress must actually clear, not linger because the
// update request omitted the field entirely.
func TestEnsureCustomer_ClearsAddressWhenUnset(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredCustomer()
	desired.Address = &Address{Country: "US", City: "Cupertino"}
	if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
		t.Fatalf("first EnsureCustomer: %v", err)
	}

	desired.Address = nil
	if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
		t.Fatalf("second EnsureCustomer: %v", err)
	}
	stored := f.customers[desired.Key]
	if stored.BillingAddress != nil && (stored.BillingAddress.Country != nil || stored.BillingAddress.City != nil) {
		t.Errorf("BillingAddress = %+v, want cleared", stored.BillingAddress)
	}
}

// TestEnsureCustomer_ClearsSubjectKeysWhenUnbound guards the "explicit
// empty list, not omitted" choice in createCustomer/updateCustomer: if a
// BillingAccount's last project binding is removed, SubjectKeys must
// actually clear server-side rather than leaving the stale attribution in
// place because an empty slice was omitted from the request.
func TestEnsureCustomer_ClearsSubjectKeysWhenUnbound(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredCustomer()
	if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
		t.Fatalf("first EnsureCustomer: %v", err)
	}

	desired.SubjectKeys = nil
	if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
		t.Fatalf("second EnsureCustomer: %v", err)
	}
	stored := f.customers[desired.Key]
	if stored.UsageAttribution == nil {
		t.Fatal("UsageAttribution is nil, want an explicit empty SubjectKeys")
	}
	if len(stored.UsageAttribution.SubjectKeys) != 0 {
		t.Errorf("SubjectKeys = %v, want empty", stored.UsageAttribution.SubjectKeys)
	}
}

// TestEnsureCustomer_CreateWithNilSubjectKeys guards the exact regression
// observed against a real OpenMeter instance: a brand-new BillingAccount
// with zero project bindings produces DesiredCustomer.SubjectKeys == nil
// (sortedCopy(nil) returns nil, not []string{}). OpenMeter's schema rejects
// a JSON null for usageAttribution.subjectKeys ("Value is not nullable")
// even though it accepts an empty array — a nil Go slice marshals to null,
// so createCustomer/updateCustomer must normalize it before it reaches the
// wire. The fake server's validateUsageAttribution enforces the same rule
// the real API does, so this fails the same way a regression would in
// production.
func TestEnsureCustomer_CreateWithNilSubjectKeys(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredCustomer()
	desired.SubjectKeys = nil

	if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	stored := f.customers[desired.Key]
	if stored.UsageAttribution == nil || stored.UsageAttribution.SubjectKeys == nil {
		t.Errorf("UsageAttribution = %+v, want a non-nil empty SubjectKeys", stored.UsageAttribution)
	}
}

// TestEnsureCustomer_CreateFallsBackToGetOnEmptyBody is the Customer
// analogue of TestEnsureMeter_CreateFallsBackToGetOnEmptyBody.
func TestEnsureCustomer_CreateFallsBackToGetOnEmptyBody(t *testing.T) {
	c, f := newTestClient(t)
	f.emptyBodyOnWrite = true
	desired := baseDesiredCustomer()

	got, err := c.EnsureCustomer(context.Background(), desired)
	if err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	if got.Key == nil || *got.Key != desired.Key {
		t.Errorf("Key = %v, want %q", got.Key, desired.Key)
	}
}

// TestEnsureCustomer_UpdateFallsBackToGetOnEmptyBody is the Customer
// analogue of TestEnsureMeter_UpdateFallsBackToGetOnEmptyBody.
func TestEnsureCustomer_UpdateFallsBackToGetOnEmptyBody(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredCustomer()
	if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
		t.Fatalf("first EnsureCustomer: %v", err)
	}

	f.emptyBodyOnWrite = true
	desired.Name = "acme-prod-renamed"
	got, err := c.EnsureCustomer(context.Background(), desired)
	if err != nil {
		t.Fatalf("second EnsureCustomer: %v", err)
	}
	if got.Name != desired.Name {
		t.Errorf("Name = %q, want %q", got.Name, desired.Name)
	}
}

func TestGetCustomer_NotFound(t *testing.T) {
	c, _ := newTestClient(t)
	_, err := c.GetCustomer(context.Background(), "missing")
	if !errors.Is(err, ErrCustomerNotFound) {
		t.Errorf("GetCustomer error = %v, want ErrCustomerNotFound", err)
	}
}

func TestDeleteCustomer_NotFoundIsSuccess(t *testing.T) {
	c, _ := newTestClient(t)
	if err := c.DeleteCustomer(context.Background(), "missing"); err != nil {
		t.Errorf("DeleteCustomer on missing key: %v", err)
	}
}

func TestDeleteCustomer_RemovesExisting(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredCustomer()
	if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}

	if err := c.DeleteCustomer(context.Background(), desired.Key); err != nil {
		t.Fatalf("DeleteCustomer: %v", err)
	}
	if _, ok := f.customers[desired.Key]; ok {
		t.Errorf("customer %q still present after delete", desired.Key)
	}
}

// TestEnsureCustomer_ClearingEmailPropagatesAndConverges is the regression
// test for the second never-converging drift check found in review.
//
// OpenMeter preserves fields omitted from a replace-PUT (verified live), so
// omitting primaryEmail when the caller has no email meant (a) clearing
// contactInfo.email never reached OpenMeter, and (b) customerNeedsUpdate
// compared stale-vs-empty forever, issuing a PUT on every reconcile. An
// explicit empty string is accepted and does clear it, so that's what gets
// sent. The fake now preserves-on-omit like the real server, without which
// this test would pass even against the broken client.
func TestEnsureCustomer_ClearingEmailPropagatesAndConverges(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredCustomer()
	if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
		t.Fatalf("first EnsureCustomer: %v", err)
	}

	desired.Email = ""
	got, err := c.EnsureCustomer(context.Background(), desired)
	if err != nil {
		t.Fatalf("EnsureCustomer after clearing email: %v", err)
	}
	if got.PrimaryEmail != nil && *got.PrimaryEmail != "" {
		t.Errorf("PrimaryEmail = %q, want cleared — omitting it from the replace leaves the stale value behind", *got.PrimaryEmail)
	}

	f.customerUpdates = 0
	for i := range 3 {
		if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
			t.Fatalf("EnsureCustomer #%d: %v", i+2, err)
		}
	}
	if f.customerUpdates != 0 {
		t.Errorf("converged EnsureCustomer issued %d update(s) across 3 no-op calls, want 0 — the email drift check never settles", f.customerUpdates)
	}
}

// TestEnsureCustomer_EmptyDesiredCurrencyDoesNotLoop covers the mirror
// image. OpenMeter REJECTS an empty currency ("minimum string length is 3",
// verified live and enforced by the fake), so unlike email it cannot be
// cleared — meaning comparing ""-vs-set would report drift that no update
// could ever resolve. An empty desired currency is therefore skipped.
func TestEnsureCustomer_EmptyDesiredCurrencyDoesNotLoop(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredCustomer()
	if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
		t.Fatalf("first EnsureCustomer: %v", err)
	}

	desired.Currency = ""
	f.customerUpdates = 0
	for i := range 3 {
		if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
			t.Fatalf("EnsureCustomer #%d with empty currency: %v", i+1, err)
		}
	}
	if f.customerUpdates != 0 {
		t.Errorf("empty desired currency issued %d update(s) across 3 calls, want 0", f.customerUpdates)
	}
	if stored := f.customers[desired.Key]; stored.Currency == nil || *stored.Currency != "USD" {
		t.Errorf("stored currency = %v, want the original USD left untouched", stored.Currency)
	}
}
