// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// chargeServer is a fake OpenMeter holding one customer's invoices, for the
// charge calls: pending-line create, invoice list/get/replace and the
// invoice-now action.
type chargeServer struct {
	mu       sync.Mutex
	invoices map[string]*om.Invoice
	order    []string
	nextID   int

	created     []om.InvoicePendingLineCreateInput
	replaced    []om.InvoiceReplaceUpdate
	actions     []om.InvoicePendingLinesActionInput
	listQueries []string
	// touchOnGet bumps the invoice's updatedAt on the Nth GET of it, as
	// subscription sync would between a read and a write.
	touchOnGet map[string]int
	gets       map[string]int
}

func newChargeServer() *chargeServer {
	return &chargeServer{invoices: map[string]*om.Invoice{}, touchOnGet: map[string]int{}, gets: map[string]int{}}
}

func (s *chargeServer) id(prefix string) string {
	s.nextID++
	return prefix + strings.Repeat("0", 3) + string(rune('a'+s.nextID))
}

func (s *chargeServer) add(inv om.Invoice) {
	if inv.Customer.Id == nil {
		cust := "cust"
		inv.Customer.Id = &cust
	}
	s.invoices[inv.Id] = &inv
	s.order = append([]string{inv.Id}, s.order...)
}

func (s *chargeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/invoices/pending-lines"):
		var in om.InvoicePendingLineCreateInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.created = append(s.created, in)
		var gathering *om.Invoice
		for _, id := range s.order {
			if s.invoices[id].Status == om.InvoiceStatusGathering {
				gathering = s.invoices[id]
			}
		}
		if gathering == nil {
			s.add(om.Invoice{Id: s.id("gather"), Status: om.InvoiceStatusGathering, Lines: &[]om.InvoiceLine{}})
			gathering = s.invoices[s.order[0]]
		}
		var out []om.InvoiceLine
		for _, l := range in.Lines {
			line := om.InvoiceLine{Id: s.id("line"), Name: l.Name, InvoiceAt: l.InvoiceAt, Period: l.Period,
				Metadata: l.Metadata, RateCard: l.RateCard, ManagedBy: om.InvoiceLineManagedByManual}
			*gathering.Lines = append(*gathering.Lines, line)
			out = append(out, line)
		}
		gathering.UpdatedAt = gathering.UpdatedAt.Add(time.Second)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(om.InvoicePendingLineCreateResponse{Invoice: *gathering, Lines: out})

	case r.Method == http.MethodPost && path == "/api/v1/billing/invoices/invoice":
		var in om.InvoicePendingLinesActionInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.actions = append(s.actions, in)
		issued := om.Invoice{Id: s.id("inv"), Status: om.InvoiceStatusIssued, Lines: &[]om.InvoiceLine{}}
		for _, id := range s.order {
			inv := s.invoices[id]
			if inv.Status != om.InvoiceStatusGathering {
				continue
			}
			var keep []om.InvoiceLine
			for _, l := range *inv.Lines {
				if in.Filters != nil && in.Filters.LineIds != nil && contains(*in.Filters.LineIds, l.Id) {
					*issued.Lines = append(*issued.Lines, l)
				} else {
					keep = append(keep, l)
				}
			}
			inv.Lines = &keep
		}
		s.add(issued)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode([]om.Invoice{issued})

	case r.Method == http.MethodGet && path == "/api/v1/billing/invoices":
		s.listQueries = append(s.listQueries, r.URL.RawQuery)
		items := make([]om.Invoice, 0, len(s.order))
		for _, id := range s.order {
			items = append(items, *s.invoices[id])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(om.InvoicePaginatedResponse{Items: items, Page: 1, PageSize: len(items), TotalCount: len(items)})

	case strings.HasPrefix(path, "/api/v1/billing/invoices/"):
		id := strings.TrimPrefix(path, "/api/v1/billing/invoices/")
		inv, ok := s.invoices[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			s.gets[id]++
			if n := s.touchOnGet[id]; n > 0 && s.gets[id] == n {
				inv.UpdatedAt = inv.UpdatedAt.Add(time.Second)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(inv)
		case http.MethodPut:
			var in om.InvoiceReplaceUpdate
			_ = json.NewDecoder(r.Body).Decode(&in)
			s.replaced = append(s.replaced, in)
			var lines []om.InvoiceLine
			for _, l := range in.Lines {
				lines = append(lines, om.InvoiceLine{Id: *l.Id, Name: l.Name, Period: l.Period, Metadata: l.Metadata})
			}
			inv.Lines = &lines
			inv.UpdatedAt = inv.UpdatedAt.Add(time.Second)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(inv)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func newChargeTestClient(t *testing.T) (Client, *chargeServer) {
	t.Helper()
	s := newChargeServer()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	return c, s
}

func testChargeLine() ChargeLine {
	return ChargeLine{
		Charge: "org-acme/install", ChargeUID: "uid-1", Currency: "USD", Name: "Cross-connect install",
		Amount: "450.00", Quantity: "2", UnitAmount: "250.00", DiscountPercent: "10",
		From: time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 12, 16, 0, 0, 0, 0, time.UTC),
		InvoiceAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), TaxCode: "txcd_20030000",
	}
}

func TestCreateAndFindChargeLine(t *testing.T) {
	c, s := newChargeTestClient(t)
	ctx := context.Background()

	ref, err := c.CreateChargeLine(ctx, "cust", testChargeLine())
	if err != nil {
		t.Fatal(err)
	}
	if !ref.Pending() || ref.LineID == "" {
		t.Fatalf("ref = %+v", ref)
	}

	sent := s.created[0]
	line := sent.Lines[0]
	if sent.Currency != "USD" || line.Name != "Cross-connect install" {
		t.Errorf("sent %+v", sent)
	}
	if !line.Period.From.Equal(testChargeLine().From) || !line.Period.To.Equal(testChargeLine().To) {
		t.Errorf("period = %+v", line.Period)
	}
	flat, err := line.RateCard.Price.AsFlatPriceWithPaymentTerm()
	if err != nil || flat.Amount != "450.00" {
		t.Errorf("price = %+v, %v; want the amount after the discount", flat, err)
	}
	if line.RateCard.Discounts != nil {
		t.Error("a rate card discount was sent; OpenMeter drops it on manual lines")
	}
	if line.RateCard.TaxConfig == nil || line.RateCard.TaxConfig.Stripe.Code != "txcd_20030000" {
		t.Errorf("tax config = %+v", line.RateCard.TaxConfig)
	}
	md := *line.Metadata
	if md[ChargeUIDMetadataKey] != "uid-1" || md[ChargeQuantityMetadataKey] != "2" ||
		md[ChargeUnitMetadataKey] != "250.00" || md[ChargeDiscountMetadataKey] != "10" {
		t.Errorf("metadata = %v", md)
	}

	found, err := c.FindChargeLine(ctx, "cust", "uid-1")
	if err != nil || found == nil || found.LineID != ref.LineID {
		t.Fatalf("FindChargeLine = %+v, %v", found, err)
	}
	if !strings.Contains(s.listQueries[0], "expand=lines") {
		t.Errorf("list query %q doesn't expand lines", s.listQueries[0])
	}
	missing, err := c.FindChargeLine(ctx, "cust", "uid-other")
	if err != nil || missing != nil {
		t.Fatalf("FindChargeLine for another charge = %+v, %v", missing, err)
	}
}

func TestInvoiceChargeLineNow(t *testing.T) {
	c, s := newChargeTestClient(t)
	ctx := context.Background()
	other := testChargeLine()
	other.ChargeUID = "uid-other"
	if _, err := c.CreateChargeLine(ctx, "cust", other); err != nil {
		t.Fatal(err)
	}
	ref, err := c.CreateChargeLine(ctx, "cust", testChargeLine())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.InvoiceChargeLineNow(ctx, "cust", ref.LineID); err != nil {
		t.Fatal(err)
	}
	if got := s.actions[0].Filters; got == nil || got.LineIds == nil || len(*got.LineIds) != 1 || (*got.LineIds)[0] != ref.LineID {
		t.Fatalf("action filters = %+v; want only the one line", got)
	}
	found, err := c.FindChargeLine(ctx, "cust", "uid-1")
	if err != nil || found == nil || found.Pending() {
		t.Fatalf("after invoicing now: %+v, %v", found, err)
	}
	stillPending, _ := c.FindChargeLine(ctx, "cust", "uid-other")
	if stillPending == nil || !stillPending.Pending() {
		t.Fatalf("the other charge moved: %+v", stillPending)
	}
}

func TestRemoveChargeLine(t *testing.T) {
	ctx := context.Background()

	t.Run("keeps every other line", func(t *testing.T) {
		c, s := newChargeTestClient(t)
		s.add(om.Invoice{Id: "gather", Status: om.InvoiceStatusGathering, Lines: &[]om.InvoiceLine{
			{Id: "sub-line", Name: "Monthly fee", ManagedBy: om.InvoiceLineManagedBySubscription},
		}})
		ref, err := c.CreateChargeLine(ctx, "cust", testChargeLine())
		if err != nil {
			t.Fatal(err)
		}
		if err := c.RemoveChargeLine(ctx, ref.InvoiceID, ref.LineID); err != nil {
			t.Fatal(err)
		}
		lines := s.replaced[len(s.replaced)-1].Lines
		if len(lines) != 1 || *lines[0].Id != "sub-line" {
			t.Fatalf("replaced lines = %+v; want only the subscription's", lines)
		}
		if found, _ := c.FindChargeLine(ctx, "cust", "uid-1"); found != nil {
			t.Fatalf("line still there: %+v", found)
		}
		// Removing it again is a no-op.
		if err := c.RemoveChargeLine(ctx, ref.InvoiceID, ref.LineID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("starts over when the invoice changes under it", func(t *testing.T) {
		c, s := newChargeTestClient(t)
		ref, err := c.CreateChargeLine(ctx, "cust", testChargeLine())
		if err != nil {
			t.Fatal(err)
		}
		s.touchOnGet[ref.InvoiceID] = 2 // the re-read before writing sees a change
		if err := c.RemoveChargeLine(ctx, ref.InvoiceID, ref.LineID); err != nil {
			t.Fatal(err)
		}
		if len(s.replaced) != 1 {
			t.Fatalf("replaced %d times; want once, after starting over", len(s.replaced))
		}
		if s.gets[ref.InvoiceID] != 4 {
			t.Fatalf("read the invoice %d times; want 4 (two attempts)", s.gets[ref.InvoiceID])
		}
	})

	t.Run("refuses a line on an issued invoice", func(t *testing.T) {
		c, s := newChargeTestClient(t)
		ref, err := c.CreateChargeLine(ctx, "cust", testChargeLine())
		if err != nil {
			t.Fatal(err)
		}
		if err := c.InvoiceChargeLineNow(ctx, "cust", ref.LineID); err != nil {
			t.Fatal(err)
		}
		found, _ := c.FindChargeLine(ctx, "cust", "uid-1")
		if err := c.RemoveChargeLine(ctx, found.InvoiceID, found.LineID); !errors.Is(err, ErrLineInvoiced) {
			t.Fatalf("err = %v, want ErrLineInvoiced", err)
		}
		if len(s.replaced) != 0 {
			t.Fatal("an issued invoice was replaced")
		}
	})

	t.Run("a gone invoice is success", func(t *testing.T) {
		c, _ := newChargeTestClient(t)
		if err := c.RemoveChargeLine(ctx, "gone", "line"); err != nil {
			t.Fatal(err)
		}
	})
}
