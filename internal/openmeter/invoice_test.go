// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
	if !strings.HasPrefix(r.URL.Path, "/api/v1/billing/invoices") {
		return false
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/billing/invoices/invoice":
		// Invoicing pending lines turns the customer's gathering invoice
		// into a draft, as OpenMeter does.
		var body om.InvoicePendingLinesActionInput
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.pendingLinesInvoiced++
		for key, invs := range f.invoices {
			for i := range invs {
				if invs[i].Customer.Id != nil && *invs[i].Customer.Id == body.CustomerId && invs[i].Status == om.InvoiceStatusGathering {
					invs[i].Status = om.InvoiceStatusDraft
				}
			}
			f.invoices[key] = invs
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("[]"))
		return true
	case r.Method == http.MethodDelete:
		// Only drafts (or earlier) can be deleted.
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/billing/invoices/")
		for key, invs := range f.invoices {
			for i, inv := range invs {
				if inv.Id != id {
					continue
				}
				if inv.Status != om.InvoiceStatusDraft && inv.Status != om.InvoiceStatusGathering {
					w.WriteHeader(http.StatusBadRequest)
					return true
				}
				f.invoices[key] = slices.Delete(invs, i, i+1)
				f.invoicesDeleted = append(f.invoicesDeleted, id)
				w.WriteHeader(http.StatusNoContent)
				return true
			}
		}
		w.WriteHeader(http.StatusNotFound)
		return true
	case r.Method != http.MethodGet:
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

func invoiceFor(customerID, id string, status om.InvoiceStatus, total string) om.Invoice {
	inv := baseInvoice(customerID, status)
	inv.Id = id
	inv.Totals.Total = total
	return inv
}

func TestPrepareInvoicesForCustomerDeletion(t *testing.T) {
	t.Run("zero-total drafts are deleted, final invoices kept", func(t *testing.T) {
		c, f := newTestClient(t)
		f.invoices["cust"] = []om.Invoice{
			invoiceFor("cust", "inv-draft-0", om.InvoiceStatusDraft, "0"),
			invoiceFor("cust", "inv-draft-000", om.InvoiceStatusDraft, "0.00"),
			invoiceFor("cust", "inv-paid", om.InvoiceStatusPaid, "100.00"),
			invoiceFor("cust", "inv-void", om.InvoiceStatusVoided, "5"),
		}
		if err := c.PrepareInvoicesForCustomerDeletion(context.Background(), "cust"); err != nil {
			t.Fatalf("PrepareInvoicesForCustomerDeletion: %v", err)
		}
		if got := strings.Join(f.invoicesDeleted, ","); got != "inv-draft-0,inv-draft-000" {
			t.Errorf("deleted = %q, want only the zero-total drafts", got)
		}
	})

	t.Run("invoices with a balance are outstanding and never deleted", func(t *testing.T) {
		c, f := newTestClient(t)
		f.invoices["cust"] = []om.Invoice{
			invoiceFor("cust", "inv-draft-owed", om.InvoiceStatusDraft, "12.50"),
			invoiceFor("cust", "inv-issued", om.InvoiceStatusIssued, "0"),
			invoiceFor("cust", "inv-draft-0", om.InvoiceStatusDraft, "0"),
		}
		err := c.PrepareInvoicesForCustomerDeletion(context.Background(), "cust")
		if !errors.Is(err, ErrInvoicesOutstanding) {
			t.Fatalf("err = %v, want ErrInvoicesOutstanding", err)
		}
		for _, id := range []string{"inv-draft-owed", "inv-issued"} {
			if !strings.Contains(err.Error(), id) {
				t.Errorf("error %q does not name %s", err, id)
			}
		}
		if got := strings.Join(f.invoicesDeleted, ","); got != "inv-draft-0" {
			t.Errorf("deleted = %q, want only the zero-total draft", got)
		}
	})

	t.Run("pending lines are invoiced first", func(t *testing.T) {
		c, f := newTestClient(t)
		f.invoices["cust"] = []om.Invoice{invoiceFor("cust", "inv-gathering", om.InvoiceStatusGathering, "0")}
		if err := c.PrepareInvoicesForCustomerDeletion(context.Background(), "cust"); err != nil {
			t.Fatalf("PrepareInvoicesForCustomerDeletion: %v", err)
		}
		if f.pendingLinesInvoiced != 1 {
			t.Errorf("pending lines invoiced %d times, want 1", f.pendingLinesInvoiced)
		}
		if got := strings.Join(f.invoicesDeleted, ","); got != "inv-gathering" {
			t.Errorf("deleted = %q, want the resulting zero-total draft", got)
		}
	})

	t.Run("no invoices", func(t *testing.T) {
		c, f := newTestClient(t)
		if err := c.PrepareInvoicesForCustomerDeletion(context.Background(), "cust"); err != nil {
			t.Fatalf("PrepareInvoicesForCustomerDeletion: %v", err)
		}
		if f.pendingLinesInvoiced != 0 || len(f.invoicesDeleted) != 0 {
			t.Error("issued writes with nothing to do")
		}
	})
}

func TestIsZeroAmount(t *testing.T) {
	for amount, want := range map[string]bool{"0": true, "0.00": true, " 0 ": true, "0.01": false, "-1": false, "": false, "abc": false} {
		if got := isZeroAmount(amount); got != want {
			t.Errorf("isZeroAmount(%q) = %v, want %v", amount, got, want)
		}
	}
}
