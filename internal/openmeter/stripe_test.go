// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// serveStripeAppData handles PUT /api/v1/customers/{key}/apps. Returns false
// when the request isn't that route, so ServeHTTP can fall through to other
// resources.
func (f *fakeServer) serveStripeAppData(w http.ResponseWriter, r *http.Request) bool {
	const prefix = "/api/v1/customers/"
	const suffix = "/apps"

	// GET /api/v1/customers/{key}/stripe backs EnsureCustomerStripeAppData's
	// drift check. Writing this app data is unusually expensive — OpenMeter
	// validates the ids against the real Stripe API on every upsert — so the
	// client reads first and skips a write that would change nothing.
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, prefix) && strings.HasSuffix(r.URL.Path, "/stripe") {
		key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/stripe")
		if f.stripeAppNotConfigured {
			writeStripeAppNotConfigured(w, key)
			return true
		}
		existing, ok := f.stripeAppData[key]
		if !ok {
			// What OpenMeter answers (app/stripe/adapter/customer.go
			// GetStripeCustomerData): a 412, not a 404.
			writePreconditionFailed(w, "customer has no data for stripe app [namespace=default customer.id="+key+"]")
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(existing)
		return true
	}

	// DELETE /api/v1/customers/{key}/apps/{appId}
	if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, prefix) && strings.Contains(r.URL.Path, suffix+"/") {
		key, appID, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, prefix), suffix+"/")
		f.stripeAppDataDeletes = append(f.stripeAppDataDeletes, key+"/"+appID)
		if existing, ok := f.stripeAppData[key]; ok && derefOrEmpty(existing.Id) == appID {
			delete(f.stripeAppData, key)
		}
		w.WriteHeader(http.StatusNoContent)
		return true
	}

	if r.Method != http.MethodPut || !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
		return false
	}
	key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), suffix)
	if key == "" {
		w.WriteHeader(http.StatusNotFound)
		return true
	}
	if f.stripeAppNotConfigured {
		writeStripeAppNotConfigured(w, key)
		return true
	}
	f.stripeAppDataWrites++

	var items []om.CustomerAppDataCreateOrUpdateItem
	if err := json.NewDecoder(r.Body).Decode(&items); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return true
	}
	for _, item := range items {
		stripeItem, err := item.AsStripeCustomerAppDataCreateOrUpdateItem()
		if err != nil {
			continue
		}
		f.stripeAppData[key] = om.StripeCustomerAppData{
			// The real server reports the Stripe app's id here.
			Id:                           ptr(fakeStripeAppID),
			Type:                         om.StripeCustomerAppDataType(stripeItem.Type),
			StripeCustomerId:             stripeItem.StripeCustomerId,
			StripeDefaultPaymentMethodId: stripeItem.StripeDefaultPaymentMethodId,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode([]om.StripeCustomerAppData{f.stripeAppData[key]})
	return true
}

// fakeStripeAppID is the id the fake server gives its Stripe app.
const fakeStripeAppID = "app-stripe-1"

// writePreconditionFailed answers with a 412 problem document carrying
// detail, the shape OpenMeter uses for GenericPreConditionFailedError.
func writePreconditionFailed(w http.ResponseWriter, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusPreconditionFailed)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "about:blank",
		"title":  "Precondition Failed",
		"status": http.StatusPreconditionFailed,
		"detail": detail,
	})
}

// writeStripeAppNotConfigured answers the way OpenMeter does when the
// customer's merged billing profile has no Stripe app
// (billing/service/customeroverride.go GetCustomerApp).
func writeStripeAppNotConfigured(w http.ResponseWriter, customerID string) {
	writePreconditionFailed(w, "error getting customer app: no stripe app found in billing profile for customer "+customerID)
}

func TestEnsureCustomerStripeAppData_Success(t *testing.T) {
	c, f := newTestClient(t)

	if err := c.EnsureCustomerStripeAppData(context.Background(), "cust-1", "cus_stripe123", "pm_stripe456"); err != nil {
		t.Fatalf("EnsureCustomerStripeAppData: %v", err)
	}
	stored, ok := f.stripeAppData["cust-1"]
	if !ok {
		t.Fatal("stripe app data not stored")
	}
	if stored.StripeCustomerId != "cus_stripe123" {
		t.Errorf("StripeCustomerId = %q, want cus_stripe123", stored.StripeCustomerId)
	}
	if stored.StripeDefaultPaymentMethodId == nil || *stored.StripeDefaultPaymentMethodId != "pm_stripe456" {
		t.Errorf("StripeDefaultPaymentMethodId = %v, want pm_stripe456", stored.StripeDefaultPaymentMethodId)
	}
}

func TestEnsureCustomerStripeAppData_EmptyPaymentMethodIDOmitted(t *testing.T) {
	c, f := newTestClient(t)

	if err := c.EnsureCustomerStripeAppData(context.Background(), "cust-1", "cus_stripe123", ""); err != nil {
		t.Fatalf("EnsureCustomerStripeAppData: %v", err)
	}
	stored := f.stripeAppData["cust-1"]
	if stored.StripeDefaultPaymentMethodId != nil {
		t.Errorf("StripeDefaultPaymentMethodId = %v, want nil", *stored.StripeDefaultPaymentMethodId)
	}
}

func TestEnsureCustomerStripeAppData_RequiresCustomerIDAndStripeCustomerID(t *testing.T) {
	c, _ := newTestClient(t)

	if err := c.EnsureCustomerStripeAppData(context.Background(), "", "cus_x", ""); !IsPermanent(err) {
		t.Errorf("empty customerID: expected a PermanentError, got %T: %v", err, err)
	}
	if err := c.EnsureCustomerStripeAppData(context.Background(), "cust-1", "", ""); !IsPermanent(err) {
		t.Errorf("empty stripeCustomerID: expected a PermanentError, got %T: %v", err, err)
	}
}

func TestEnsureCustomerStripeAppData_ServerErrorIsClassified(t *testing.T) {
	c, f := newTestClient(t)
	f.statusOverride = http.StatusInternalServerError

	err := c.EnsureCustomerStripeAppData(context.Background(), "cust-1", "cus_x", "")
	if !IsTransient(err) {
		t.Errorf("expected a TransientError for 500, got %T: %v", err, err)
	}
	var perm *PermanentError
	if errors.As(err, &perm) {
		t.Errorf("500 should not classify as permanent: %v", err)
	}
}

// TestEnsureCustomerStripeAppData_SkipsRedundantWrite covers the drift check
// added in review. This is the one Ensure* whose write is genuinely
// expensive: OpenMeter validates the ids against the real Stripe account on
// every upsert (that validation is what surfaces "stripe customer ... not
// found in stripe account"), so an unconditional write meant an outbound
// third-party call on every single reconcile.
func TestEnsureCustomerStripeAppData_SkipsRedundantWrite(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()

	if err := c.EnsureCustomerStripeAppData(ctx, "cust-1", "cus_x", "pm_x"); err != nil {
		t.Fatalf("first EnsureCustomerStripeAppData: %v", err)
	}
	if f.stripeAppDataWrites != 1 {
		t.Fatalf("first call issued %d write(s), want 1", f.stripeAppDataWrites)
	}

	for i := range 3 {
		if err := c.EnsureCustomerStripeAppData(ctx, "cust-1", "cus_x", "pm_x"); err != nil {
			t.Fatalf("EnsureCustomerStripeAppData #%d: %v", i+2, err)
		}
	}
	if f.stripeAppDataWrites != 1 {
		t.Errorf("unchanged app data issued %d total write(s), want 1 — each redundant write costs a real Stripe API round trip", f.stripeAppDataWrites)
	}
}

// TestEnsureCustomerStripeAppData_WritesOnRealDrift makes sure the skip
// above didn't turn the sync inert: a changed payment method must still be
// pushed.
func TestEnsureCustomerStripeAppData_WritesOnRealDrift(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()

	if err := c.EnsureCustomerStripeAppData(ctx, "cust-1", "cus_x", "pm_old"); err != nil {
		t.Fatalf("first EnsureCustomerStripeAppData: %v", err)
	}
	if err := c.EnsureCustomerStripeAppData(ctx, "cust-1", "cus_x", "pm_new"); err != nil {
		t.Fatalf("second EnsureCustomerStripeAppData: %v", err)
	}
	if f.stripeAppDataWrites != 2 {
		t.Errorf("payment-method drift issued %d write(s), want 2", f.stripeAppDataWrites)
	}
	if got := f.stripeAppData["cust-1"]; got.StripeDefaultPaymentMethodId == nil || *got.StripeDefaultPaymentMethodId != "pm_new" {
		t.Errorf("stored payment method = %v, want pm_new", got.StripeDefaultPaymentMethodId)
	}
}

// TestEnsureCustomerStripeAppData_EmptyPaymentMethodClearsStored: once the
// default payment method goes away, re-upserting the same Stripe customer
// without one must clear the stored id — otherwise OpenMeter keeps putting
// a detached card on every Stripe invoice.
func TestEnsureCustomerStripeAppData_EmptyPaymentMethodClearsStored(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()

	if err := c.EnsureCustomerStripeAppData(ctx, "cust-1", "cus_x", "pm_old"); err != nil {
		t.Fatalf("first EnsureCustomerStripeAppData: %v", err)
	}
	if err := c.EnsureCustomerStripeAppData(ctx, "cust-1", "cus_x", ""); err != nil {
		t.Fatalf("clearing EnsureCustomerStripeAppData: %v", err)
	}
	got := f.stripeAppData["cust-1"]
	if got.StripeCustomerId != "cus_x" || got.StripeDefaultPaymentMethodId != nil {
		t.Errorf("stored = %+v, want cus_x with no payment method", got)
	}
	if err := c.EnsureCustomerStripeAppData(ctx, "cust-1", "cus_x", ""); err != nil {
		t.Fatalf("repeat EnsureCustomerStripeAppData: %v", err)
	}
	if f.stripeAppDataWrites != 2 {
		t.Errorf("issued %d write(s), want 2 (set, clear; the repeat is a no-op)", f.stripeAppDataWrites)
	}
}

func TestGetCustomerStripeAppData(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()

	if _, err := c.GetCustomerStripeAppData(ctx, "cust-1"); !errors.Is(err, ErrStripeAppDataNotFound) {
		t.Errorf("no app data: err = %v, want ErrStripeAppDataNotFound", err)
	}

	if err := c.EnsureCustomerStripeAppData(ctx, "cust-1", "cus_x", "pm_x"); err != nil {
		t.Fatalf("EnsureCustomerStripeAppData: %v", err)
	}
	got, err := c.GetCustomerStripeAppData(ctx, "cust-1")
	if err != nil {
		t.Fatalf("GetCustomerStripeAppData: %v", err)
	}
	if got.StripeCustomerId != "cus_x" || derefOrEmpty(got.StripeDefaultPaymentMethodId) != "pm_x" {
		t.Errorf("got %+v, want cus_x/pm_x", got)
	}

	f.stripeAppNotConfigured = true
	if _, err := c.GetCustomerStripeAppData(ctx, "cust-1"); !errors.Is(err, ErrStripeAppNotConfigured) {
		t.Errorf("no Stripe app: err = %v, want ErrStripeAppNotConfigured", err)
	}
}

// TestEnsureCustomerStripeAppData_NoStripeAppIsRecognizable: when the
// customer's billing profile uses another app (Sandbox), the 412 must be
// distinguishable so the controller can report it once instead of failing
// every sync.
func TestEnsureCustomerStripeAppData_NoStripeAppIsRecognizable(t *testing.T) {
	c, f := newTestClient(t)
	f.stripeAppNotConfigured = true

	err := c.EnsureCustomerStripeAppData(context.Background(), "cust-1", "cus_x", "pm_x")
	if !errors.Is(err, ErrStripeAppNotConfigured) {
		t.Fatalf("err = %v, want ErrStripeAppNotConfigured", err)
	}
	if !IsPermanent(err) {
		t.Errorf("err = %T, want it still classified permanent", err)
	}
}

// TestEnsureCustomerStripeAppData_OtherPreconditionFailuresStayGeneric: a
// 412 for any other reason (e.g. an unknown Stripe customer) is a real sync
// failure and must not be mistaken for "no Stripe app".
func TestEnsureCustomerStripeAppData_OtherPreconditionFailuresStayGeneric(t *testing.T) {
	srv := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writePreconditionFailed(w, "stripe customer cus_x not found in stripe account: acct_1")
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	c, err := NewClient(ts.URL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	err = c.EnsureCustomerStripeAppData(context.Background(), "cust-1", "cus_x", "")
	if !IsPermanent(err) {
		t.Fatalf("err = %v, want a PermanentError", err)
	}
	if errors.Is(err, ErrStripeAppNotConfigured) || errors.Is(err, ErrStripeAppDataNotFound) {
		t.Errorf("err = %v, want neither sentinel", err)
	}
}

// TestDeleteCustomerStripeAppData: deleting is per app, so the client reads
// which app holds the data and deletes it there. Nothing stored, or no Stripe
// app at all, is already the wanted state.
func TestDeleteCustomerStripeAppData(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()

	if err := c.EnsureCustomerStripeAppData(ctx, "cust-1", "cus_x", "pm_x"); err != nil {
		t.Fatalf("EnsureCustomerStripeAppData: %v", err)
	}
	if err := c.DeleteCustomerStripeAppData(ctx, "cust-1"); err != nil {
		t.Fatalf("DeleteCustomerStripeAppData: %v", err)
	}
	if want := []string{"cust-1/" + fakeStripeAppID}; !slices.Equal(f.stripeAppDataDeletes, want) {
		t.Errorf("deletes = %v, want %v", f.stripeAppDataDeletes, want)
	}
	if _, err := c.GetCustomerStripeAppData(ctx, "cust-1"); !errors.Is(err, ErrStripeAppDataNotFound) {
		t.Errorf("after delete: err = %v, want ErrStripeAppDataNotFound", err)
	}

	// Nothing stored any more: no delete is sent.
	if err := c.DeleteCustomerStripeAppData(ctx, "cust-1"); err != nil {
		t.Errorf("second delete: %v", err)
	}
	f.stripeAppNotConfigured = true
	if err := c.DeleteCustomerStripeAppData(ctx, "cust-1"); err != nil {
		t.Errorf("no Stripe app: %v", err)
	}
	if len(f.stripeAppDataDeletes) != 1 {
		t.Errorf("deletes = %v, want only the first", f.stripeAppDataDeletes)
	}
}
