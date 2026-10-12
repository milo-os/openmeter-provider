// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// fakePendingLines describes a customer's gathering invoice for
// InvoicePendingLinesAction: BillableTotal is what invoicing it now would
// bill ("" means nothing is billable yet), FutureLines whether lines with a
// future invoiceAt remain on the gathering invoice afterwards.
type fakePendingLines struct {
	BillableTotal string
	FutureLines   bool
}

// writeProblem answers like OpenMeter's billing HTTP driver does for a
// billing.ValidationError: 400, application/problem+json, the validation
// issue's message as detail and its code under extensions.
func writeProblem(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":       "about:blank",
		"title":      http.StatusText(status),
		"status":     status,
		"detail":     message,
		"extensions": map[string]any{"code": code, "message": message, "severity": "critical"},
	})
}

// serveInvoices mirrors the OpenMeter invoice routes the client uses:
//
//	GET    /api/v1/billing/invoices          list; honors customers (unless
//	                                         ignoreInvoiceCustomerFilter),
//	                                         includeDeleted, page/pageSize
//	POST   /api/v1/billing/invoices/invoice  invoice pending lines
//	DELETE /api/v1/billing/invoices/{id}     delete
//
// f.invoices is keyed by the owning customer's id — the server knows the
// owner of a gathering invoice even though its response carries no
// customer id (see gatheringInvoice).
//
// Pending lines behave like the real service: the billable lines move to a
// NEW standard (draft) invoice, the gathering invoice is deleted only when
// no lines remain on it, and nothing billable is a 400 with code
// invoice_create_no_lines. Gathering invoices cannot be deleted, and
// deleting a draft soft-deletes it (status deleted, deletedAt set), hidden
// from the default list. An earlier fake simply flipped the gathering
// invoice to draft, which hid all three behaviors.
func (f *fakeServer) serveInvoices(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/v1/billing/invoices") {
		return false
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/billing/invoices/invoice":
		var body om.InvoicePendingLinesActionInput
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.pendingLinesInvoiced++
		if f.pendingLinesStatus != 0 {
			writeProblem(w, f.pendingLinesStatus, "", "injected failure")
			return true
		}
		invs := f.invoices[body.CustomerId]
		gathering := -1
		for i, inv := range invs {
			if inv.Status == om.InvoiceStatusGathering && inv.DeletedAt == nil {
				gathering = i
			}
		}
		lines := f.pendingLines[body.CustomerId]
		if gathering < 0 || lines.BillableTotal == "" {
			writeProblem(w, http.StatusBadRequest, "invoice_create_no_lines", "the new invoice would have no lines")
			return true
		}
		f.invoiceSeq++
		created := invoiceFor(body.CustomerId, fmt.Sprintf("inv-std-%d", f.invoiceSeq), om.InvoiceStatusDraft, lines.BillableTotal)
		invs = append(invs, created)
		if lines.FutureLines {
			f.pendingLines[body.CustomerId] = fakePendingLines{FutureLines: true}
		} else {
			now := time.Now().UTC()
			invs[gathering].DeletedAt = &now
			delete(f.pendingLines, body.CustomerId)
		}
		f.invoices[body.CustomerId] = invs
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode([]om.Invoice{created})
		return true

	case r.Method == http.MethodDelete:
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/billing/invoices/")
		for key, invs := range f.invoices {
			for i, inv := range invs {
				if inv.Id != id || inv.DeletedAt != nil {
					continue
				}
				switch inv.Status {
				case om.InvoiceStatusGathering:
					writeProblem(w, http.StatusBadRequest, "invoice_cannot_delete_gathering",
						"gathering invoices cannot be deleted, please delete the upcoming lines instead")
					return true
				case om.InvoiceStatusDraft:
				default:
					writeProblem(w, http.StatusBadRequest, "invoice_delete_failed", "invoice delete failed")
					return true
				}
				f.invoicesDeleted = append(f.invoicesDeleted, id)
				if f.deleteInvoiceAsync {
					invs[i].Status = InvoiceStatusDeleting
					invs[i].StatusDetails.ExtendedStatus = "delete.in_progress"
				} else {
					now := time.Now().UTC()
					invs[i].Status = InvoiceStatusDeleted
					invs[i].StatusDetails.ExtendedStatus = "deleted"
					invs[i].DeletedAt = &now
				}
				f.invoices[key] = invs
				w.WriteHeader(http.StatusOK)
				return true
			}
		}
		w.WriteHeader(http.StatusNotFound)
		return true

	case r.Method != http.MethodGet:
		return false
	}

	q := r.URL.Query()
	includeDeleted := q.Get("includeDeleted") == "true"
	customers := q["customers"]
	var items []om.Invoice
	for owner, invs := range f.invoices {
		if !f.ignoreInvoiceCustomerFilter && len(customers) > 0 && !containsString(customers, owner) {
			continue
		}
		for _, inv := range invs {
			if inv.DeletedAt != nil && !includeDeleted {
				continue
			}
			items = append(items, inv)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Id < items[j].Id })

	page := 1
	if v := q.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	pageSize := invoiceListPageSize
	if v := q.Get("pageSize"); v != "" {
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

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// baseInvoice's customerID represents OpenMeter's internal customer id (a
// ULID in reality — an opaque test string here), matching what ListInvoices
// requires and filters on via Invoice.Customer.Id.
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

// gatheringInvoice is a gathering invoice as OpenMeter's gathering mapping
// renders it: the customer snapshot has a key and name but an EMPTY id,
// and the totals are empty.
func gatheringInvoice(customerKey, id string) om.Invoice {
	inv := baseInvoice("", om.InvoiceStatusGathering)
	inv.Id = id
	empty := ""
	inv.Customer = om.BillingInvoiceCustomerExtendedDetails{Id: &empty, Key: &customerKey}
	inv.Totals = om.InvoiceTotals{}
	inv.StatusDetails.ExtendedStatus = "gathering"
	return inv
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

// TestListInvoices_ClientSideFilterDropsForeignInvoices covers the
// client-side half of the customer filter, with a server that ignores the
// customers param: another customer's standard invoice is dropped, but a
// gathering invoice without a customer id is not (see
// invoiceBelongsToCustomer).
func TestListInvoices_ClientSideFilterDropsForeignInvoices(t *testing.T) {
	c, f := newTestClient(t)
	f.ignoreInvoiceCustomerFilter = true
	f.invoices["cust-1"] = []om.Invoice{baseInvoice("cust-1", om.InvoiceStatusIssued)}
	f.invoices["cust-2"] = []om.Invoice{baseInvoice("cust-2", om.InvoiceStatusPaid)}

	got, err := c.ListInvoices(context.Background(), "cust-1")
	if err != nil {
		t.Fatalf("ListInvoices: %v", err)
	}
	if len(got) != 1 || got[0].Id != "inv_cust-1" {
		t.Errorf("got %v, want only inv_cust-1", invoiceIDs(got))
	}
}

// TestListInvoices_KeepsGatheringInvoiceWithoutCustomerID is the H1
// regression: OpenMeter renders gathering invoices without a customer id,
// and dropping them meant the pending lines were never invoiced, so the
// customer could never be deleted ("invoice ... is still in gathering
// state").
func TestListInvoices_KeepsGatheringInvoiceWithoutCustomerID(t *testing.T) {
	c, f := newTestClient(t)
	f.invoices["cust-1"] = []om.Invoice{gatheringInvoice("key-1", "inv-gathering")}
	f.invoices["cust-2"] = []om.Invoice{gatheringInvoice("key-2", "inv-gathering-other")}

	got, err := c.ListInvoices(context.Background(), "cust-1")
	if err != nil {
		t.Fatalf("ListInvoices: %v", err)
	}
	if ids := invoiceIDs(got); len(ids) != 1 || ids[0] != "inv-gathering" {
		t.Errorf("got %v, want [inv-gathering] (kept, and not another customer's)", ids)
	}
}

func TestInvoiceBelongsToCustomer(t *testing.T) {
	empty := ""
	other := "cust-2"
	mine := "cust-1"
	for _, tt := range []struct {
		name string
		inv  om.Invoice
		want bool
	}{
		{"own id", om.Invoice{Status: om.InvoiceStatusIssued, Customer: om.BillingInvoiceCustomerExtendedDetails{Id: &mine}}, true},
		{"other id", om.Invoice{Status: om.InvoiceStatusIssued, Customer: om.BillingInvoiceCustomerExtendedDetails{Id: &other}}, false},
		{"other id gathering", om.Invoice{Status: om.InvoiceStatusGathering, Customer: om.BillingInvoiceCustomerExtendedDetails{Id: &other}}, false},
		{"empty id gathering", om.Invoice{Status: om.InvoiceStatusGathering, Customer: om.BillingInvoiceCustomerExtendedDetails{Id: &empty}}, true},
		{"nil id gathering", om.Invoice{Status: om.InvoiceStatusGathering}, true},
		{"empty id standard", om.Invoice{Status: om.InvoiceStatusIssued, Customer: om.BillingInvoiceCustomerExtendedDetails{Id: &empty}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := invoiceBelongsToCustomer(tt.inv, "cust-1"); got != tt.want {
				t.Errorf("invoiceBelongsToCustomer = %v, want %v", got, tt.want)
			}
		})
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
	if _, err := c.ListInvoicesIncludingDeleted(context.Background(), ""); !IsPermanent(err) {
		t.Errorf("expected a PermanentError for an empty key, got %T: %v", err, err)
	}
}

func TestListInvoicesIncludingDeleted(t *testing.T) {
	c, f := newTestClient(t)
	deleted := invoiceFor("cust", "inv-deleted", InvoiceStatusDeleted, "0")
	at := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	deleted.DeletedAt = &at
	f.invoices["cust"] = []om.Invoice{deleted, invoiceFor("cust", "inv-live", om.InvoiceStatusIssued, "5")}

	live, err := c.ListInvoices(context.Background(), "cust")
	if err != nil {
		t.Fatalf("ListInvoices: %v", err)
	}
	if ids := invoiceIDs(live); len(ids) != 1 || ids[0] != "inv-live" {
		t.Errorf("ListInvoices = %v, want only the live invoice", ids)
	}
	all, err := c.ListInvoicesIncludingDeleted(context.Background(), "cust")
	if err != nil {
		t.Fatalf("ListInvoicesIncludingDeleted: %v", err)
	}
	if ids := invoiceIDs(all); len(ids) != 2 {
		t.Errorf("ListInvoicesIncludingDeleted = %v, want both invoices", ids)
	}
	if !IsInvoiceDeleted(deleted) || IsInvoiceDeleted(live[0]) {
		t.Error("IsInvoiceDeleted misclassified an invoice")
	}
}

func invoiceIDs(invs []om.Invoice) []string {
	ids := make([]string, 0, len(invs))
	for _, inv := range invs {
		ids = append(ids, inv.Id)
	}
	sort.Strings(ids)
	return ids
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
			invoiceFor("cust", "inv-uncollectible", om.InvoiceStatusUncollectible, "7"),
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
		if strings.Contains(err.Error(), "inv-draft-0 ") {
			t.Errorf("error %q names the deleted zero-total draft", err)
		}
		if got := strings.Join(f.invoicesDeleted, ","); got != "inv-draft-0" {
			t.Errorf("deleted = %q, want only the zero-total draft", got)
		}
	})

	t.Run("pending lines are invoiced into a new invoice", func(t *testing.T) {
		// The gathering invoice is invoiced, OpenMeter creates a NEW
		// zero-total draft and removes the emptied gathering invoice; the
		// draft is then deleted. The gathering invoice itself is never
		// deleted (the real server refuses that).
		c, f := newTestClient(t)
		f.invoices["cust"] = []om.Invoice{gatheringInvoice("key", "inv-gathering")}
		f.pendingLines["cust"] = fakePendingLines{BillableTotal: "0"}
		if err := c.PrepareInvoicesForCustomerDeletion(context.Background(), "cust"); err != nil {
			t.Fatalf("PrepareInvoicesForCustomerDeletion: %v", err)
		}
		if f.pendingLinesInvoiced != 1 {
			t.Errorf("pending lines invoiced %d times, want 1", f.pendingLinesInvoiced)
		}
		if got := strings.Join(f.invoicesDeleted, ","); got != "inv-std-1" {
			t.Errorf("deleted = %q, want the newly created zero-total draft inv-std-1", got)
		}
	})

	t.Run("pending lines with a balance leave an outstanding draft", func(t *testing.T) {
		c, f := newTestClient(t)
		f.invoices["cust"] = []om.Invoice{gatheringInvoice("key", "inv-gathering")}
		f.pendingLines["cust"] = fakePendingLines{BillableTotal: "42.00"}
		err := c.PrepareInvoicesForCustomerDeletion(context.Background(), "cust")
		if !errors.Is(err, ErrInvoicesOutstanding) || !strings.Contains(err.Error(), "inv-std-1 (draft, total 42.00 USD)") {
			t.Fatalf("err = %v, want ErrInvoicesOutstanding naming the new draft", err)
		}
		if len(f.invoicesDeleted) != 0 {
			t.Errorf("deleted %v, want nothing", f.invoicesDeleted)
		}
	})

	t.Run("nothing billable is not an error, remaining gathering invoice is outstanding", func(t *testing.T) {
		// InvoicePendingLinesAction answers 400 invoice_create_no_lines
		// when no line is billable yet; the gathering invoice (lines with a
		// future invoiceAt) still blocks customer deletion, so it is
		// waited for rather than failing hard.
		c, f := newTestClient(t)
		f.invoices["cust"] = []om.Invoice{gatheringInvoice("key", "inv-gathering")}
		f.pendingLines["cust"] = fakePendingLines{FutureLines: true}
		err := c.PrepareInvoicesForCustomerDeletion(context.Background(), "cust")
		if !errors.Is(err, ErrInvoicesOutstanding) {
			t.Fatalf("err = %v, want ErrInvoicesOutstanding", err)
		}
		if IsPermanent(err) {
			t.Errorf("err = %v is permanent; nothing to invoice must not be a hard failure", err)
		}
		if !strings.Contains(err.Error(), "inv-gathering (gathering") {
			t.Errorf("error %q does not name the gathering invoice", err)
		}
		if len(f.invoicesDeleted) != 0 {
			t.Errorf("deleted %v, want nothing (gathering invoices cannot be deleted)", f.invoicesDeleted)
		}
	})

	t.Run("billable and future lines: new invoice plus remaining gathering invoice", func(t *testing.T) {
		c, f := newTestClient(t)
		f.invoices["cust"] = []om.Invoice{gatheringInvoice("key", "inv-gathering")}
		f.pendingLines["cust"] = fakePendingLines{BillableTotal: "0", FutureLines: true}
		err := c.PrepareInvoicesForCustomerDeletion(context.Background(), "cust")
		if !errors.Is(err, ErrInvoicesOutstanding) || !strings.Contains(err.Error(), "inv-gathering") {
			t.Fatalf("err = %v, want ErrInvoicesOutstanding naming the gathering invoice", err)
		}
		if got := strings.Join(f.invoicesDeleted, ","); got != "inv-std-1" {
			t.Errorf("deleted = %q, want the zero-total draft inv-std-1", got)
		}
	})

	t.Run("other pending-lines failures are errors", func(t *testing.T) {
		c, f := newTestClient(t)
		f.invoices["cust"] = []om.Invoice{gatheringInvoice("key", "inv-gathering")}
		f.pendingLinesStatus = http.StatusInternalServerError
		err := c.PrepareInvoicesForCustomerDeletion(context.Background(), "cust")
		if err == nil || errors.Is(err, ErrInvoicesOutstanding) || !IsTransient(err) {
			t.Fatalf("err = %v, want the transient action failure", err)
		}
	})

	t.Run("deletion in progress is waited for", func(t *testing.T) {
		c, f := newTestClient(t)
		f.deleteInvoiceAsync = true
		f.invoices["cust"] = []om.Invoice{invoiceFor("cust", "inv-draft-0", om.InvoiceStatusDraft, "0")}
		err := c.PrepareInvoicesForCustomerDeletion(context.Background(), "cust")
		if !errors.Is(err, ErrInvoicesOutstanding) || !strings.Contains(err.Error(), "inv-draft-0 (deletion in progress)") {
			t.Fatalf("err = %v, want ErrInvoicesOutstanding for the deletion in progress", err)
		}
	})

	t.Run("failed deletion is outstanding with a clear reason", func(t *testing.T) {
		c, f := newTestClient(t)
		inv := invoiceFor("cust", "inv-delete-failed", InvoiceStatusDeleting, "0")
		inv.StatusDetails = om.InvoiceStatusDetails{ExtendedStatus: "delete.failed", Failed: true}
		f.invoices["cust"] = []om.Invoice{inv}
		err := c.PrepareInvoicesForCustomerDeletion(context.Background(), "cust")
		if !errors.Is(err, ErrInvoicesOutstanding) || !strings.Contains(err.Error(), "deletion failed (delete.failed)") {
			t.Fatalf("err = %v, want ErrInvoicesOutstanding explaining the failed deletion", err)
		}
		if len(f.invoicesDeleted) != 0 {
			t.Errorf("deleted %v, want nothing", f.invoicesDeleted)
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

func TestIsNoLinesToInvoice(t *testing.T) {
	for name, tt := range map[string]struct {
		body string
		want bool
	}{
		"code":                {`{"status":400,"detail":"x","extensions":{"code":"invoice_create_no_lines"}}`, true},
		"other code":          {`{"status":400,"detail":"the new invoice would have no lines","extensions":{"code":"something_else"}}`, false},
		"detail without code": {`{"status":400,"detail":"preparing: the new invoice would have no lines"}`, true},
		"unrelated":           {`{"status":400,"detail":"bad"}`, false},
		"not json":            {`nope`, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := isNoLinesToInvoice([]byte(tt.body)); got != tt.want {
				t.Errorf("isNoLinesToInvoice = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsZeroAmount(t *testing.T) {
	for amount, want := range map[string]bool{"0": true, "0.00": true, " 0 ": true, "0.01": false, "-1": false, "": false, "abc": false} {
		if got := isZeroAmount(amount); got != want {
			t.Errorf("isZeroAmount(%q) = %v, want %v", amount, got, want)
		}
	}
}
