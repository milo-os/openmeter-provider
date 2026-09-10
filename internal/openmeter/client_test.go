// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"net/http"
	"net/http/httptest"
	"testing"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	om "github.com/openmeterio/openmeter/api/client/go"
)

// fakeServer is a minimal in-memory stand-in for the OpenMeter API. Each
// resource type owns its own routing (see serveMeters in meter_test.go,
// serveCustomers in customer_test.go): ServeHTTP just dispatches to them in
// turn, so adding a new resource type's tests never requires touching an
// existing resource's test file — only this file (one dispatch line, plus
// the new resource's storage map below) and the new resource's own _test.go.
type fakeServer struct {
	meters    map[string]om.Meter
	customers map[string]om.Customer
	// invoices is keyed by customer key, mirroring how ListInvoices is
	// always called.
	invoices map[string][]om.Invoice
	// stripeAppData is keyed by customer key.
	stripeAppData map[string]om.StripeCustomerAppData
	// billingProfiles is keyed by profile id.
	billingProfiles map[string]om.BillingProfile
	// billingProfileOverrides is keyed by customer key.
	billingProfileOverrides map[string]om.BillingProfileCustomerOverrideCreate

	// statusOverride, when non-zero, short-circuits every request with that
	// status code. Tests use it to inject wire-level failures (429s, 5xxs)
	// that aren't specific to any one resource's routing.
	statusOverride int
	// emptyBodyOnWrite reproduces what was observed against a real OpenMeter
	// instance right after a fresh install: a write applies (the resource is
	// stored) but the response comes back 2xx with no body, so the SDK can't
	// populate the typed response field. Shared across resources since every
	// resource's create/update implements the same fall-back-to-GET fix.
	emptyBodyOnWrite bool

	// Write counters. Every Ensure* is supposed to converge: once the remote
	// state matches, re-running it must issue NO further writes. Nothing
	// enforced that before, which let two separate never-converging drift
	// checks ship (see TestEnsureBillingProfile_ConvergesWhenServerNormalizes
	// AlignmentD and TestEnsureCustomer_ConvergesAfterClearingEmail), each
	// silently issuing a PUT on every single reconcile forever.
	customerUpdates       int
	billingProfileUpdates int
	stripeAppDataWrites   int

	// ingestedEvents accumulates every CloudEvent ever POSTed to the
	// ingest route, in submission order, across all calls.
	ingestedEvents []cloudevents.Event
}

func newTestClient(t *testing.T) (Client, *fakeServer) {
	t.Helper()
	f := &fakeServer{
		meters:                  map[string]om.Meter{},
		customers:               map[string]om.Customer{},
		invoices:                map[string][]om.Invoice{},
		stripeAppData:           map[string]om.StripeCustomerAppData{},
		billingProfiles:         map[string]om.BillingProfile{},
		billingProfileOverrides: map[string]om.BillingProfileCustomerOverrideCreate{},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, f
}

// ServeHTTP dispatches to each resource's own handler in turn; the first one
// that recognizes the request path handles it and returns true.
func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.statusOverride != 0 {
		w.WriteHeader(f.statusOverride)
		return
	}
	if f.serveMeters(w, r) {
		return
	}
	// serveStripeAppData must be checked before serveCustomers: its route
	// (/api/v1/customers/{key}/apps) shares the /api/v1/customers prefix
	// serveCustomers matches on, so the more specific route goes first.
	if f.serveStripeAppData(w, r) {
		return
	}
	if f.serveCustomers(w, r) {
		return
	}
	if f.serveInvoices(w, r) {
		return
	}
	if f.serveBillingProfiles(w, r) {
		return
	}
	if f.serveIngest(w, r) {
		return
	}
	w.WriteHeader(http.StatusNotFound)
}
