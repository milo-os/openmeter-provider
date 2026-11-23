// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// fakeCustomInvoicing is just enough of OpenMeter's apps API: it lists
// and installs Custom Invoicing apps, takes app updates, and records the
// sync calls.
type fakeCustomInvoicing struct {
	mu       sync.Mutex
	apps     []om.CustomInvoicingApp
	installs int
	updates  []om.CustomInvoicingAppReplaceUpdate
	calls    map[string]string // path -> body
}

func (f *fakeCustomInvoicing) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/apps":
		items := make([]om.App, 0, len(f.apps))
		for _, a := range f.apps {
			var item om.App
			raw, _ := json.Marshal(a)
			_ = item.UnmarshalJSON(raw)
			items = append(items, item)
		}
		_ = json.NewEncoder(w).Encode(om.AppPaginatedResponse{Items: items, Page: 1, PageSize: 100, TotalCount: len(items)})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/marketplace/listings/custom_invoicing/install":
		var req om.MarketplaceInstallRequestPayload
		_ = json.Unmarshal(body, &req)
		if req.CreateBillingProfile == nil || *req.CreateBillingProfile {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.installs++
		app := om.CustomInvoicingApp{Id: "app_ci", Name: *req.Name, Type: om.CustomInvoicingAppTypeCustomInvoicing, Status: om.AppStatusReady}
		f.apps = append(f.apps, app)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"app": app, "defaultForCapabilityTypes": []string{}})
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/apps/"):
		var req om.CustomInvoicingAppReplaceUpdate
		_ = json.Unmarshal(body, &req)
		f.updates = append(f.updates, req)
		for i := range f.apps {
			if "/api/v1/apps/"+f.apps[i].Id == r.URL.Path {
				f.apps[i].EnableDraftSyncHook = req.EnableDraftSyncHook
				f.apps[i].EnableIssuingSyncHook = req.EnableIssuingSyncHook
				_ = json.NewEncoder(w).Encode(f.apps[i])
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/apps/custom-invoicing/"):
		if f.calls == nil {
			f.calls = map[string]string{}
		}
		f.calls[r.URL.Path] = string(body)
		_, _ = w.Write([]byte(`{}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newCustomInvoicingClient(t *testing.T, f *fakeCustomInvoicing) Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestEnsureCustomInvoicingApp_InstallsWithHooksOn(t *testing.T) {
	f := &fakeCustomInvoicing{}
	c := newCustomInvoicingClient(t, f)

	id, err := c.EnsureCustomInvoicingApp(context.Background(), "Datum invoice writer")
	if err != nil {
		t.Fatalf("EnsureCustomInvoicingApp: %v", err)
	}
	if id != "app_ci" || f.installs != 1 {
		t.Fatalf("id %q after %d installs, want app_ci after one", id, f.installs)
	}
	if len(f.updates) != 1 || !f.updates[0].EnableDraftSyncHook || !f.updates[0].EnableIssuingSyncHook {
		t.Fatalf("hooks not turned on: %+v", f.updates)
	}

	// Installed and on: found, and left alone.
	if id, err = c.EnsureCustomInvoicingApp(context.Background(), "Datum invoice writer"); err != nil || id != "app_ci" {
		t.Fatalf("second EnsureCustomInvoicingApp = %q, %v", id, err)
	}
	if f.installs != 1 || len(f.updates) != 1 {
		t.Errorf("installed %d times, updated %d times; want once each", f.installs, len(f.updates))
	}
}

func TestEnsureCustomInvoicingApp_IgnoresOtherApps(t *testing.T) {
	f := &fakeCustomInvoicing{apps: []om.CustomInvoicingApp{
		{Id: "app_other", Name: "Someone else's", Type: om.CustomInvoicingAppTypeCustomInvoicing, EnableDraftSyncHook: true, EnableIssuingSyncHook: true},
	}}
	c := newCustomInvoicingClient(t, f)
	id, err := c.EnsureCustomInvoicingApp(context.Background(), "Datum invoice writer")
	if err != nil || id != "app_ci" || f.installs != 1 {
		t.Fatalf("got %q, %v after %d installs; want its own app installed", id, err, f.installs)
	}
}

func TestCustomInvoicingSyncCalls(t *testing.T) {
	f := &fakeCustomInvoicing{}
	c := newCustomInvoicingClient(t, f)
	ctx := context.Background()

	if err := c.DraftSynchronized(ctx, "inv_1", "in_stripe", []LineExternalID{{LineID: "line_1", ExternalID: "ii_1"}}); err != nil {
		t.Fatalf("DraftSynchronized: %v", err)
	}
	sentAt := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := c.IssuingSynchronized(ctx, "inv_1", "DATUM-0042", "in_stripe", &sentAt); err != nil {
		t.Fatalf("IssuingSynchronized: %v", err)
	}
	if err := c.UpdatePaymentStatus(ctx, "inv_1", PaymentTriggerPaid); err != nil {
		t.Fatalf("UpdatePaymentStatus: %v", err)
	}

	var draft om.CustomInvoicingDraftSynchronizedRequest
	_ = json.Unmarshal([]byte(f.calls["/api/v1/apps/custom-invoicing/inv_1/draft/synchronized"]), &draft)
	if draft.Invoicing == nil || *draft.Invoicing.ExternalId != "in_stripe" ||
		len(*draft.Invoicing.LineExternalIds) != 1 || (*draft.Invoicing.LineExternalIds)[0].ExternalId != "ii_1" {
		t.Errorf("draft synchronized body = %s", f.calls["/api/v1/apps/custom-invoicing/inv_1/draft/synchronized"])
	}
	var issuing om.CustomInvoicingFinalizedRequest
	_ = json.Unmarshal([]byte(f.calls["/api/v1/apps/custom-invoicing/inv_1/issuing/synchronized"]), &issuing)
	if issuing.Invoicing == nil || *issuing.Invoicing.InvoiceNumber != "DATUM-0042" || *issuing.Payment.ExternalId != "in_stripe" {
		t.Errorf("issuing synchronized body = %s", f.calls["/api/v1/apps/custom-invoicing/inv_1/issuing/synchronized"])
	}
	if got := f.calls["/api/v1/apps/custom-invoicing/inv_1/payment/status"]; !strings.Contains(got, `"paid"`) {
		t.Errorf("payment status body = %s", got)
	}

	if err := c.IssuingSynchronized(ctx, "inv_2", "", "in_stripe", nil); !IsPermanent(err) {
		t.Errorf("issuing without a number = %v, want a permanent error: OpenMeter would make one up", err)
	}
}

func TestEnsureBillingProfile_MovesToAndFromTheInvoiceWriterApp(t *testing.T) {
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	ctx := context.Background()

	desired := standardDesired("account-1")
	stripeProfile, err := c.EnsureBillingProfile(ctx, "", desired)
	if err != nil {
		t.Fatalf("EnsureBillingProfile: %v", err)
	}

	desired.AppID = "app_ci"
	moved, err := c.EnsureBillingProfile(ctx, stripeProfile.Profile.Id, desired)
	if err != nil {
		t.Fatalf("EnsureBillingProfile on the writer: %v", err)
	}
	if moved.Profile.Id == stripeProfile.Profile.Id {
		t.Fatal("the account kept its profile: OpenMeter can't change a profile's apps, so it needs a new one")
	}
	apps, _ := moved.Profile.Apps.AsBillingProfileAppReferences()
	if apps.Invoicing.Id != "app_ci" || apps.Payment.Id != "app_ci" || apps.Tax.Id != "app_ci" {
		t.Errorf("apps = %+v, want app_ci for all three", apps)
	}
	if moved.AppsDrift != "" || moved.IgnoredExistingID != "" {
		t.Errorf("moving isn't drift or a foreign profile: %+v", moved)
	}
	if _, ok := f.billingProfiles[stripeProfile.Profile.Id]; !ok {
		t.Error("the old profile was removed; its invoices still reference it")
	}

	// Its annotation lost: the writer's profile is found again, not made twice.
	again, err := c.EnsureBillingProfile(ctx, "", desired)
	if err != nil || again.Profile.Id != moved.Profile.Id {
		t.Fatalf("again = %q, %v; want %q", again.Profile.Id, err, moved.Profile.Id)
	}

	// Moved back: its Stripe profile again.
	desired.AppID = ""
	back, err := c.EnsureBillingProfile(ctx, moved.Profile.Id, desired)
	if err != nil || back.Profile.Id != stripeProfile.Profile.Id {
		t.Fatalf("back = %q, %v; want %q", back.Profile.Id, err, stripeProfile.Profile.Id)
	}
}
