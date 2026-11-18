// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	omv3 "github.com/openmeterio/openmeter/api/v3"
)

// fakeTaxCodes serves OpenMeter's v3 tax code list and create. created
// counts creates. conflictOnce makes the first create fail with 409, as
// when another reconcile created the tax code first, and adds conflictCode
// as if that reconcile had.
type fakeTaxCodes struct {
	mu           sync.Mutex
	codes        []omv3.BillingTaxCode
	created      []omv3.CreateTaxCodeRequest
	conflictOnce bool
	conflictCode string
}

func (f *fakeTaxCodes) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/api/v3/openmeter/tax-codes" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		number, _ := strconv.Atoi(r.URL.Query().Get("page[number]"))
		size, _ := strconv.Atoi(r.URL.Query().Get("page[size]"))
		start := min((number-1)*size, len(f.codes))
		end := min(start+size, len(f.codes))
		_ = json.NewEncoder(w).Encode(omv3.TaxCodePagePaginatedResponse{Data: f.codes[start:end]})
	case http.MethodPost:
		var req omv3.CreateTaxCodeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if f.conflictOnce {
			f.conflictOnce = false
			f.add(f.conflictCode)
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"title":"Conflict","status":409}`))
			return
		}
		f.created = append(f.created, req)
		tc := f.add(req.AppMappings[0].TaxCode)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(tc)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (f *fakeTaxCodes) add(stripeCode string) omv3.BillingTaxCode {
	tc := omv3.BillingTaxCode{
		Id:          fmt.Sprintf("taxcode-%d", len(f.codes)+1),
		Key:         taxCodeKey(stripeCode),
		Name:        stripeCode,
		AppMappings: []omv3.BillingTaxCodeAppMapping{{AppType: omv3.BillingAppTypeStripe, TaxCode: stripeCode}},
	}
	f.codes = append(f.codes, tc)
	return tc
}

func newTaxCodeClient(t *testing.T, f *fakeTaxCodes) Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestEnsureTaxCode_CreatesOneMappedToStripe(t *testing.T) {
	f := &fakeTaxCodes{}
	id, err := newTaxCodeClient(t, f).EnsureTaxCode(context.Background(), "txcd_10103001")
	if err != nil {
		t.Fatalf("EnsureTaxCode: %v", err)
	}
	if id != "taxcode-1" {
		t.Errorf("id = %q, want taxcode-1", id)
	}
	if len(f.created) != 1 {
		t.Fatalf("want one create, got %d", len(f.created))
	}
	got := f.created[0]
	if got.Key != "stripe_txcd_10103001" || got.Name != "txcd_10103001" {
		t.Errorf("created key %q name %q", got.Key, got.Name)
	}
	if len(got.AppMappings) != 1 || got.AppMappings[0].AppType != omv3.BillingAppTypeStripe ||
		got.AppMappings[0].TaxCode != "txcd_10103001" {
		t.Errorf("app mappings = %+v", got.AppMappings)
	}
}

func TestEnsureTaxCode_FindsAnExistingOne(t *testing.T) {
	// One that isn't ours, mapping the same Stripe code under another key,
	// is used rather than duplicated. A deleted one is skipped.
	f := &fakeTaxCodes{}
	for i := range taxCodePageSize {
		f.add(fmt.Sprintf("txcd_9%07d", i))
	}
	deleted := f.add("txcd_10103001")
	f.codes[len(f.codes)-1].DeletedAt = &deleted.CreatedAt
	f.codes = append(f.codes, omv3.BillingTaxCode{
		Id:          "theirs",
		Key:         "saas",
		AppMappings: []omv3.BillingTaxCodeAppMapping{{AppType: omv3.BillingAppTypeStripe, TaxCode: "txcd_10103001"}},
	})

	id, err := newTaxCodeClient(t, f).EnsureTaxCode(context.Background(), "txcd_10103001")
	if err != nil {
		t.Fatalf("EnsureTaxCode: %v", err)
	}
	if id != "theirs" {
		t.Errorf("id = %q, want the existing tax code on the second page", id)
	}
	if len(f.created) != 0 {
		t.Errorf("want no create, got %+v", f.created)
	}
}

func TestEnsureTaxCode_UsesTheOneAnotherReconcileCreated(t *testing.T) {
	f := &fakeTaxCodes{conflictOnce: true, conflictCode: "txcd_20030000"}
	id, err := newTaxCodeClient(t, f).EnsureTaxCode(context.Background(), "txcd_20030000")
	if err != nil {
		t.Fatalf("EnsureTaxCode: %v", err)
	}
	if id != "taxcode-1" {
		t.Errorf("id = %q, want the conflicting tax code", id)
	}
}

func TestEnsureTaxCode_EmptyIsPermanent(t *testing.T) {
	_, err := newTaxCodeClient(t, &fakeTaxCodes{}).EnsureTaxCode(context.Background(), "")
	var perm *PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("err = %v, want a PermanentError", err)
	}
}

func TestTaxCodeKey(t *testing.T) {
	for in, want := range map[string]string{
		"txcd_10103001": "stripe_txcd_10103001",
		"TXCD-1010.30":  "stripe_txcd_1010_30",
		"__a__b__":      "stripe_a_b",
	} {
		if got := taxCodeKey(in); got != want {
			t.Errorf("taxCodeKey(%q) = %q, want %q", in, got, want)
		}
	}
	long := taxCodeKey("txcd_1234567890123456789012345678901234567890123456789012345678901234567890")
	if len(long) > 64 {
		t.Errorf("key %q is longer than 64", long)
	}
}
