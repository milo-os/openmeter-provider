// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

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

// tick advances the fake clock by a second and returns it, so every write
// gets a distinct UpdatedAt.
func (f *fakeServer) tick() time.Time {
	f.clockTicks++
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(f.clockTicks) * time.Second)
}

// conflictingSubjectKey mirrors OpenMeter's unique index on active customer
// subject keys: it returns the first of keys already attributed to a
// customer other than ownerKey, or "".
func (f *fakeServer) conflictingSubjectKey(ownerKey string, keys []string) string {
	for otherKey, other := range f.customers {
		if otherKey == ownerKey || other.UsageAttribution == nil {
			continue
		}
		for _, k := range keys {
			if slices.Contains(other.UsageAttribution.SubjectKeys, k) {
				return k
			}
		}
	}
	return ""
}

// writeSubjectKeyConflict answers the way OpenMeter does for
// customer.SubjectKeyConflictError: a 409 problem document whose detail is
// the only thing that tells it apart from the other customer 409s.
func writeSubjectKeyConflict(w http.ResponseWriter, keys []string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "about:blank",
		"title":  "Conflict",
		"status": http.StatusConflict,
		"detail": "one or multiple subject keys of [" + strings.Join(keys, ", ") +
			"] are already associated with an different customer in the namespace default",
	})
}

// mergeAddress applies a replace-PUT's billingAddress the way OpenMeter's
// customer adapter does: an omitted address clears every field, while a
// present one is applied field by field with SetNillable* — a nil field
// keeps the stored value, a pointer (to "" included) overwrites it. The fake
// used to just assign the body's address, which hid that a nil field never
// clears anything on the real server.
func mergeAddress(stored, body *om.Address) *om.Address {
	if body == nil {
		return nil
	}
	merged := om.Address{}
	if stored != nil {
		merged = *stored
	}
	set := func(dst **string, src *string) {
		if src != nil {
			v := *src
			*dst = &v
		}
	}
	if body.Country != nil {
		c := *body.Country
		merged.Country = &c
	}
	set(&merged.Line1, body.Line1)
	set(&merged.Line2, body.Line2)
	set(&merged.City, body.City)
	set(&merged.State, body.State)
	set(&merged.PostalCode, body.PostalCode)
	set(&merged.PhoneNumber, body.PhoneNumber)
	if merged == (om.Address{}) {
		// The real GET only returns an address when a column is non-NULL.
		return nil
	}
	return &merged
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
		if body.UsageAttribution != nil && f.conflictingSubjectKey(bodyKey, body.UsageAttribution.SubjectKeys) != "" {
			writeSubjectKeyConflict(w, body.UsageAttribution.SubjectKeys)
			return true
		}
		now := f.tick()
		m := om.Customer{
			Id:               "id-" + bodyKey,
			Key:              body.Key,
			Name:             body.Name,
			Description:      body.Description,
			Metadata:         body.Metadata,
			PrimaryEmail:     body.PrimaryEmail,
			Currency:         body.Currency,
			BillingAddress:   mergeAddress(nil, body.BillingAddress),
			UsageAttribution: body.UsageAttribution,
			CreatedAt:        now,
			UpdatedAt:        now,
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
		var subjectKeys []string
		if body.UsageAttribution != nil {
			subjectKeys = body.UsageAttribution.SubjectKeys
		}
		if f.conflictingSubjectKey(key, subjectKeys) != "" {
			writeSubjectKeyConflict(w, subjectKeys)
			return true
		}
		f.customerUpdates++
		f.customerUpdateBodies = append(f.customerUpdateBodies, body)
		m.Name = body.Name
		m.UpdatedAt = f.tick()
		// Field by field, the way OpenMeter's customer adapter applies a
		// replace-PUT (openmeter/customer/adapter/customer.go
		// UpdateCustomer): description and metadata are set-or-cleared,
		// an omitted usageAttribution removes every subject key, and the
		// address follows mergeAddress.
		m.Description = body.Description
		m.Metadata = body.Metadata
		m.BillingAddress = mergeAddress(m.BillingAddress, body.BillingAddress)
		m.UsageAttribution = &om.CustomerUsageAttribution{SubjectKeys: nonNilStrings(subjectKeys)}
		// Despite being a REPLACE, the real OpenMeter PRESERVES these when
		// omitted from the body (SetNillable*) rather than clearing them —
		// verified live. The fake used to clear them, which is exactly what
		// hid the bug where omitting an empty primaryEmail meant a cleared
		// email never propagated AND customerNeedsUpdate saw drift forever.
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
	if got.Key == nil || *got.Key != string(desired.Key) {
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
	if _, ok := f.customers[string(desired.Key)]; !ok {
		t.Errorf("customer %q not stored", desired.Key)
	}
}

func TestEnsureCustomer_NoopWhenUpToDate(t *testing.T) {
	c, f := newTestClient(t)
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
	if f.customerUpdates != 0 {
		t.Errorf("no-drift EnsureCustomer issued %d update(s), want 0", f.customerUpdates)
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
	if stored := f.customers[string(desired.Key)]; stored.UsageAttribution == nil || len(stored.UsageAttribution.SubjectKeys) != 1 {
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
	if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
		t.Fatalf("second EnsureCustomer: %v", err)
	}
	if f.customerUpdates != 0 {
		t.Errorf("no-drift EnsureCustomer with an Address set issued %d update(s), want 0", f.customerUpdates)
	}
}

// TestEnsureCustomer_ClearsAddressWhenUnset: removing a BillingAccount's
// contactInfo.address must actually clear OpenMeter's billingAddress, and
// then stay converged. OpenMeter only clears the address columns when the
// whole billingAddress is omitted; an object of nil fields (what the client
// used to send) is a no-op there, so the address lingered and every
// reconcile re-sent the update.
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
	stored := f.customers[string(desired.Key)]
	if stored.BillingAddress != nil {
		t.Errorf("BillingAddress = %+v, want cleared", stored.BillingAddress)
	}

	assertCustomerConverged(t, c, f, desired)
}

// TestEnsureCustomer_ClearsSingleAddressField: emptying one optional
// address field (here line2) must clear it server-side, and converge. A nil
// field is a no-op in OpenMeter's SetNillable* update, so the client sends
// an explicit "" for it.
func TestEnsureCustomer_ClearsSingleAddressField(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredCustomer()
	desired.Address = &Address{Country: "US", Line1: "1 Infinite Loop", Line2: "Suite 100", City: "Cupertino"}
	if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
		t.Fatalf("first EnsureCustomer: %v", err)
	}

	desired.Address.Line2 = ""
	if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
		t.Fatalf("second EnsureCustomer: %v", err)
	}
	stored := f.customers[string(desired.Key)].BillingAddress
	if stored == nil || derefOrEmpty(stored.Line2) != "" {
		t.Errorf("BillingAddress = %+v, want line2 cleared", stored)
	}
	if stored == nil || derefOrEmpty(stored.Line1) != "1 Infinite Loop" || derefOrEmpty(stored.City) != "Cupertino" {
		t.Errorf("BillingAddress = %+v, want the other fields kept", stored)
	}

	assertCustomerConverged(t, c, f, desired)
}

// assertCustomerConverged runs EnsureCustomer a few more times and fails if
// any of them wrote.
func assertCustomerConverged(t *testing.T, c Client, f *fakeServer, desired DesiredCustomer) {
	t.Helper()
	f.customerUpdates = 0
	for i := range 3 {
		if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
			t.Fatalf("EnsureCustomer #%d: %v", i+1, err)
		}
	}
	if f.customerUpdates != 0 {
		t.Errorf("converged EnsureCustomer issued %d update(s) across 3 calls, want 0", f.customerUpdates)
	}
}

// TestEnsureCustomer_SubjectKeyConflictIsTransient: a project still
// attributed to another customer (it moved accounts and the old account
// hasn't let go yet) clears on its own, so it must not be reported as a
// permanent 409.
func TestEnsureCustomer_SubjectKeyConflictIsTransient(t *testing.T) {
	c, _ := newTestClient(t)
	other := baseDesiredCustomer()
	other.Key = "ba-uid-old"
	other.SubjectKeys = []string{"projects/moved"}
	if _, err := c.EnsureCustomer(context.Background(), other); err != nil {
		t.Fatalf("EnsureCustomer(old): %v", err)
	}

	t.Run("create", func(t *testing.T) {
		desired := baseDesiredCustomer()
		desired.Key = "ba-uid-new"
		desired.SubjectKeys = []string{"projects/moved"}
		_, err := c.EnsureCustomer(context.Background(), desired)
		if !errors.Is(err, ErrSubjectKeyConflict) {
			t.Fatalf("err = %v, want ErrSubjectKeyConflict", err)
		}
		if !IsTransient(err) || IsPermanent(err) {
			t.Errorf("err = %T %v, want a TransientError only", err, err)
		}
	})

	t.Run("update", func(t *testing.T) {
		desired := baseDesiredCustomer()
		desired.Key = "ba-uid-existing"
		desired.SubjectKeys = nil
		if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
			t.Fatalf("EnsureCustomer(existing): %v", err)
		}
		desired.SubjectKeys = []string{"projects/moved"}
		_, err := c.EnsureCustomer(context.Background(), desired)
		if !errors.Is(err, ErrSubjectKeyConflict) || !IsTransient(err) {
			t.Fatalf("err = %v, want a transient ErrSubjectKeyConflict", err)
		}
	})
}

// TestReleaseCustomerSubjects_ClearsOnlySubjectKeys: the finalizer's release
// must drop the subject keys (freeing them for another customer) and leave
// everything else, including fields a replace-PUT would clear if omitted.
func TestReleaseCustomerSubjects_ClearsOnlySubjectKeys(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredCustomer()
	desired.Address = &Address{Country: "US", City: "Cupertino"}
	if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	stored := f.customers[string(desired.Key)]
	description := "set outside this controller"
	stored.Description = &description
	stored.Metadata = &om.Metadata{"k": "v"}
	f.customers[string(desired.Key)] = stored

	if err := c.ReleaseCustomerSubjects(context.Background(), desired.Key); err != nil {
		t.Fatalf("ReleaseCustomerSubjects: %v", err)
	}
	after := f.customers[string(desired.Key)]
	if after.UsageAttribution == nil || len(after.UsageAttribution.SubjectKeys) != 0 {
		t.Errorf("UsageAttribution = %+v, want empty", after.UsageAttribution)
	}
	if after.Name != desired.Name || derefOrEmpty(after.PrimaryEmail) != desired.Email ||
		after.Currency == nil || *after.Currency != desired.Currency {
		t.Errorf("customer = %+v, want name/email/currency kept", after)
	}
	if after.BillingAddress == nil || derefOrEmpty(after.BillingAddress.City) != "Cupertino" {
		t.Errorf("BillingAddress = %+v, want kept", after.BillingAddress)
	}
	if derefOrEmpty(after.Description) != description || after.Metadata == nil || (*after.Metadata)["k"] != "v" {
		t.Errorf("description/metadata = %v/%v, want kept", after.Description, after.Metadata)
	}
	// The body must carry an explicit empty list: an omitted
	// usageAttribution would bypass OpenMeter's active-subscription check.
	last := f.customerUpdateBodies[len(f.customerUpdateBodies)-1]
	if last.UsageAttribution == nil || last.UsageAttribution.SubjectKeys == nil {
		t.Errorf("PUT usageAttribution = %+v, want an explicit empty list", last.UsageAttribution)
	}

	// The released key is free for another customer now.
	other := baseDesiredCustomer()
	other.Key = "ba-uid-other"
	if _, err := c.EnsureCustomer(context.Background(), other); err != nil {
		t.Errorf("EnsureCustomer(other) after release: %v", err)
	}
}

func TestReleaseCustomerSubjects_NoopCases(t *testing.T) {
	c, f := newTestClient(t)
	if err := c.ReleaseCustomerSubjects(context.Background(), "missing"); err != nil {
		t.Errorf("missing customer: %v", err)
	}
	desired := baseDesiredCustomer()
	desired.SubjectKeys = nil
	if _, err := c.EnsureCustomer(context.Background(), desired); err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	if err := c.ReleaseCustomerSubjects(context.Background(), desired.Key); err != nil {
		t.Errorf("no subject keys: %v", err)
	}
	if f.customerUpdates != 0 {
		t.Errorf("no-op release issued %d update(s), want 0", f.customerUpdates)
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
	stored := f.customers[string(desired.Key)]
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
	stored := f.customers[string(desired.Key)]
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
	if got.Key == nil || *got.Key != string(desired.Key) {
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
	if _, ok := f.customers[string(desired.Key)]; ok {
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
	if stored := f.customers[string(desired.Key)]; stored.Currency == nil || *stored.Currency != "USD" {
		t.Errorf("stored currency = %v, want the original USD left untouched", stored.Currency)
	}
}
