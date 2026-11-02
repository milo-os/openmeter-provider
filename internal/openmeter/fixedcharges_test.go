// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
)

const fixedChargesCustomer = CustomerID("01JCUSTOMER0000000000000000")

// gatheringInvoiceJSON is a gathering invoice as OpenMeter lists it with
// expand=lines, priced with live data: next month's contract fee, a usage
// line, and a deleted flat line. The setup fee and first month are already
// invoiced (OpenMeter invoices in-advance fees as soon as they're due).
const gatheringInvoiceJSON = `{
  "items": [{
    "id": "01JGATHERING", "status": "gathering", "currency": "USD", "customer": {"key": "acme"},
    "lines": [
      {"id": "line-next-month", "name": "Dedicated server", "currency": "USD", "type": "usage_based", "status": "valid", "managedBy": "subscription",
       "invoiceAt": "2026-12-02T00:00:00Z", "period": {"from": "2026-12-02T00:00:00Z", "to": "2027-01-02T00:00:00Z"},
       "rateCard": {"price": {"type": "flat", "amount": "1000", "paymentTerm": "in_advance"}},
       "totals": {"total": "900", "taxesExclusiveTotal": "0"}},
      {"id": "line-usage", "name": "CPU", "currency": "USD", "type": "usage_based", "status": "valid", "managedBy": "subscription",
       "invoiceAt": "2026-12-02T00:00:00Z", "period": {"from": "2026-11-02T00:00:00Z", "to": "2026-12-02T00:00:00Z"},
       "rateCard": {"price": {"type": "unit", "amount": "0.01"}, "featureKey": "cpu"},
       "totals": {"total": "12.34", "taxesExclusiveTotal": "0"}},
      {"id": "line-deleted", "name": "Old fee", "currency": "USD", "type": "usage_based", "status": "valid", "managedBy": "subscription",
       "deletedAt": "2026-11-02T01:00:00Z", "invoiceAt": "2026-12-02T00:00:00Z", "period": {"from": "2026-12-02T00:00:00Z", "to": "2027-01-02T00:00:00Z"},
       "rateCard": {"price": {"type": "flat", "amount": "50"}}, "totals": {"total": "50"}}
    ]
  }],
  "page": 1, "pageSize": 100, "totalCount": 1
}`

const draftInvoicesJSON = `{
  "items": [
    {"id": "01JOLDDRAFT", "status": "draft", "currency": "USD", "customer": {"id": "01JCUSTOMER0000000000000000"}, "createdAt": "2026-11-01T00:00:00Z"},
    {"id": "01JDRAFT", "status": "draft", "currency": "USD", "customer": {"id": "01JCUSTOMER0000000000000000"}, "createdAt": "2026-11-02T13:43:10Z",
     "lines": [{"id": "l1", "name": "Server installation", "currency": "USD", "type": "usage_based", "status": "valid", "managedBy": "subscription",
                "invoiceAt": "2026-11-02T13:43:10Z", "period": {"from": "2026-11-02T13:43:10Z", "to": "2026-11-02T13:43:10Z"},
                "totals": {"total": "1500", "taxesExclusiveTotal": "0"}}]},
    {"id": "01JOTHER", "status": "draft", "currency": "USD", "customer": {"id": "01JSOMEONEELSE000000000000"}, "createdAt": "2026-11-03T00:00:00Z"}
  ],
  "page": 1, "pageSize": 100, "totalCount": 3
}`

type fixedChargesServer struct {
	listQueries []map[string][]string
	simulated   om.InvoiceSimulationInput
}

func newFixedChargesClient(t *testing.T, f *fixedChargesServer) Client {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
	mux.HandleFunc("GET /api/v1/billing/invoices", func(w http.ResponseWriter, r *http.Request) {
		f.listQueries = append(f.listQueries, r.URL.Query())
		switch r.URL.Query().Get("statuses") {
		case "gathering":
			writeJSON(w, gatheringInvoiceJSON)
		case "draft":
			writeJSON(w, draftInvoicesJSON)
		default:
			t.Errorf("unexpected statuses %q", r.URL.Query().Get("statuses"))
		}
	})
	mux.HandleFunc("POST /api/v1/billing/customers/{id}/invoices/simulate", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&f.simulated); err != nil {
			t.Errorf("decoding simulation: %v", err)
		}
		writeJSON(w, `{"id": "", "currency": "USD", "totals": {"total": "2400.00", "taxesExclusiveTotal": "0"}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// percentage renders an OpenMeter percentage as it's sent.
func percentage(t *testing.T, p om.Percentage) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("encoding percentage: %v", err)
	}
	return string(b)
}

func TestUpcomingFixedCharges(t *testing.T) {
	f := &fixedChargesServer{}
	c := newFixedChargesClient(t, f)

	lines, err := c.UpcomingFixedCharges(context.Background(), fixedChargesCustomer)
	if err != nil {
		t.Fatalf("UpcomingFixedCharges: %v", err)
	}
	q := f.listQueries[0]
	if !slices.Equal(q["statuses"], []string{"gathering"}) || !slices.Equal(q["expand"], []string{"lines"}) {
		t.Errorf("query = %v, want gathering invoices with lines", q)
	}
	// Not the usage line or the deleted line.
	if len(lines) != 1 || lines[0].Id != "line-next-month" || lines[0].Totals.Total != "900" {
		t.Errorf("lines = %+v, want next month's fee with its live total", lines)
	}
}

func TestDraftInvoice(t *testing.T) {
	c := newFixedChargesClient(t, &fixedChargesServer{})

	draft, err := c.DraftInvoice(context.Background(), fixedChargesCustomer)
	if err != nil {
		t.Fatalf("DraftInvoice: %v", err)
	}
	// The newest of the customer's drafts, not another customer's.
	if draft == nil || draft.Id != "01JDRAFT" || len(LineItems(*draft)) != 1 {
		t.Errorf("draft = %+v, want 01JDRAFT with its line", draft)
	}
}

func TestPreviewInvoice_SimulatesPlannedCharges(t *testing.T) {
	f := &fixedChargesServer{}
	c := newFixedChargesClient(t, f)
	start := time.Date(2026, time.November, 18, 0, 0, 0, 0, time.UTC)

	inv, err := c.PreviewInvoice(context.Background(), fixedChargesCustomer, "USD", []PlannedCharge{
		{Name: "Server installation", Period: om.Period{From: start, To: start}, Amount: "1500.00"},
		{Name: "Dedicated server", Period: MonthlyPeriod(start), Amount: "1000.00", DiscountPercentage: 10},
	})
	if err != nil {
		t.Fatalf("PreviewInvoice: %v", err)
	}
	if inv.Totals.Total != "2400.00" {
		t.Errorf("total = %q, want the simulation's", inv.Totals.Total)
	}

	sim := f.simulated
	if sim.Currency != "USD" || len(sim.Lines) != 2 {
		t.Fatalf("simulation = %+v, want two USD lines", sim)
	}
	setup, monthly := sim.Lines[0], sim.Lines[1]
	if setup.Id == nil || monthly.Id == nil || *setup.Id == *monthly.Id {
		t.Errorf("line ids = %v, %v; want distinct ids for discounts to refer to", setup.Id, monthly.Id)
	}
	if !monthly.Period.To.Equal(time.Date(2026, time.December, 18, 0, 0, 0, 0, time.UTC)) || !monthly.InvoiceAt.Equal(start) {
		t.Errorf("monthly line = %+v, want the month from the start, invoiced at the start", monthly)
	}
	price, err := monthly.RateCard.Price.AsFlatPriceWithPaymentTerm()
	if err != nil || price.Amount != "1000.00" || price.PaymentTerm == nil || *price.PaymentTerm != om.PricePaymentTermInAdvance {
		t.Errorf("monthly price = %+v (%v), want 1000.00 in advance", price, err)
	}
	if d := monthly.RateCard.Discounts; d == nil || d.Percentage == nil || percentage(t, d.Percentage.Percentage) != "10" {
		t.Errorf("monthly discounts = %+v, want 10%%", d)
	}
	if setup.RateCard.Discounts != nil {
		t.Errorf("setup discounts = %+v, want none", setup.RateCard.Discounts)
	}
}

func TestDecimals(t *testing.T) {
	for _, tc := range []struct {
		total, exclusiveTax, want string
	}{
		{"2400.00", "0", "2400.00"},
		{"2640.00", "240.00", "2400.00"},
		{"10.5", "", "10.5"},
	} {
		got, err := AmountBeforeTax(om.InvoiceTotals{Total: tc.total, TaxesExclusiveTotal: tc.exclusiveTax})
		if err != nil || got != tc.want {
			t.Errorf("AmountBeforeTax(%s, %s) = %q, %v; want %q", tc.total, tc.exclusiveTax, got, err, tc.want)
		}
	}
	if got, err := AddDecimals("1500", "900.00", "0.5"); err != nil || got != "2400.50" {
		t.Errorf("AddDecimals = %q, %v; want 2400.50", got, err)
	}
	if got, err := AddDecimals(); err != nil || got != "0" {
		t.Errorf("AddDecimals() = %q, %v; want 0", got, err)
	}
	if _, err := AmountBeforeTax(om.InvoiceTotals{Total: "lots"}); err == nil {
		t.Error("an invalid total: want an error")
	}
}
