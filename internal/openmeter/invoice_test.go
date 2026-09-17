// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// serveInvoices handles GET /api/v1/billing/invoices. Returns false when the
// request isn't that route, so ServeHTTP can fall through to other
// resources. Ignores the Customers query param filter and instead returns
// every invoice seeded across all customers — ListInvoices' own client-side
// filter against Invoice.Customer.Id is what these tests actually exercise.
// Honors page/pageSize so multi-page ListInvoices behavior can actually be
// tested, with a deterministic sort-by-Id order across pages.
func (f *fakeServer) serveInvoices(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/api/v1/billing/invoices") {
		return false
	}
	var items []om.Invoice
	for _, invs := range f.invoices {
		items = append(items, invs...)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Id < items[j].Id })

	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	pageSize := invoiceListPageSize
	if v := r.URL.Query().Get("pageSize"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pageSize = n
		}
	}
	total := len(items)
	start := min((page-1)*pageSize, total)
	end := min(start+pageSize, total)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(om.InvoicePaginatedResponse{
		Items:      items[start:end],
		Page:       page,
		PageSize:   pageSize,
		TotalCount: total,
	})
	return true
}

// baseInvoice's customerID represents OpenMeter's internal customer id (a
// ULID in reality — an opaque test string here), matching what ListInvoices
// now requires and filters on via Invoice.Customer.Id.
func baseInvoice(customerID string, status om.InvoiceStatus) om.Invoice {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	return om.Invoice{
		Id:        "inv_" + customerID,
		Currency:  "USD",
		CreatedAt: now,
		UpdatedAt: now,
		Status:    status,
		Customer:  om.BillingInvoiceCustomerExtendedDetails{Id: &customerID},
		Period:    &om.Period{From: now, To: now.AddDate(0, 1, 0)},
		Totals:    om.InvoiceTotals{Total: "100.00"},
	}
}

func TestListInvoices_ReturnsCustomerInvoices(t *testing.T) {
	c, f := newTestClient(t)
	f.invoices["cust-1"] = []om.Invoice{baseInvoice("cust-1", om.InvoiceStatusIssued)}
	f.invoices["cust-2"] = []om.Invoice{baseInvoice("cust-2", om.InvoiceStatusPaid)}

	got, err := c.ListInvoices(context.Background(), "cust-1")
	if err != nil {
		t.Fatalf("ListInvoices: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (must not leak cust-2's invoice)", len(got))
	}
	if got[0].Id != "inv_cust-1" {
		t.Errorf("Id = %q, want inv_cust-1", got[0].Id)
	}
}

func TestListInvoices_NoInvoicesYetIsNotAnError(t *testing.T) {
	c, _ := newTestClient(t)

	got, err := c.ListInvoices(context.Background(), "cust-with-no-invoices")
	if err != nil {
		t.Fatalf("ListInvoices: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

func TestListInvoices_WalksBeyondFirstPage(t *testing.T) {
	// A long-lived BillingAccount can accumulate more invoices than fit in
	// a single page. Seed more than invoiceListPageSize for one customer
	// and confirm ListInvoices returns all of them, not just page 1's
	// worth.
	c, f := newTestClient(t)
	const want = invoiceListPageSize + 5
	invs := make([]om.Invoice, want)
	for i := range want {
		inv := baseInvoice("cust-many", om.InvoiceStatusIssued)
		inv.Id = fmt.Sprintf("inv_cust-many_%03d", i)
		invs[i] = inv
	}
	f.invoices["cust-many"] = invs

	got, err := c.ListInvoices(context.Background(), "cust-many")
	if err != nil {
		t.Fatalf("ListInvoices: %v", err)
	}
	if len(got) != want {
		t.Errorf("len(got) = %d, want %d (did the walk stop after page 1?)", len(got), want)
	}
}

func TestListInvoices_RequiresKey(t *testing.T) {
	c, _ := newTestClient(t)
	if _, err := c.ListInvoices(context.Background(), ""); !IsPermanent(err) {
		t.Errorf("expected a PermanentError for an empty key, got %T: %v", err, err)
	}
}
