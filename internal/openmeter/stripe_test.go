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
		existing, ok := f.stripeAppData[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(existing)
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
