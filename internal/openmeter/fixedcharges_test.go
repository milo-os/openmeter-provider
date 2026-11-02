// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
)

const fixedChargesCustomer = CustomerID("01JCUSTOMER0000000000000000")

// gatheringInvoiceJSON is a gathering invoice as OpenMeter returns it with
// expand=lines: a setup fee and a monthly fee billable on 18 March, next
// month's fee, a usage line, and a deleted flat line. Gathering lines carry
// no discounts and no totals.
const gatheringInvoiceJSON = `{
  "items": [{
    "id": "01JGATHERING",
    "status": "gathering",
    "currency": "USD",
    "customer": {"key": "acme"},
    "lines": [
      {"id": "line-setup", "name": "Server installation", "currency": "USD", "type": "usage_based", "status": "valid", "managedBy": "subscription",
       "invoiceAt": "2026-03-18T00:00:00Z", "period": {"from": "2026-03-18T00:00:00Z", "to": "2026-03-18T00:00:00Z"},
       "rateCard": {"price": {"type": "flat", "amount": "1500", "paymentTerm": "in_advance"}},
       "subscription": {"subscription": {"id": "sub-1"}, "phase": {"id": "phase-1"}, "item": {"id": "item-setup"}, "billingPeriod": {"from": "2026-03-18T00:00:00Z", "to": "2026-04-18T00:00:00Z"}}},
      {"id": "line-monthly", "name": "Dedicated server", "currency": "USD", "type": "usage_based", "status": "valid", "managedBy": "subscription",
       "invoiceAt": "2026-03-18T00:00:00Z", "period": {"from": "2026-03-18T00:00:00Z", "to": "2026-04-18T00:00:00Z"},
       "rateCard": {"price": {"type": "flat", "amount": "1000", "paymentTerm": "in_advance"}},
       "subscription": {"subscription": {"id": "sub-1"}, "phase": {"id": "phase-1"}, "item": {"id": "item-monthly"}, "billingPeriod": {"from": "2026-03-18T00:00:00Z", "to": "2026-04-18T00:00:00Z"}}},
      {"id": "line-next-month", "name": "Dedicated server", "currency": "USD", "type": "usage_based", "status": "valid", "managedBy": "subscription",
       "invoiceAt": "2026-04-18T00:00:00Z", "period": {"from": "2026-04-18T00:00:00Z", "to": "2026-05-18T00:00:00Z"},
       "rateCard": {"price": {"type": "flat", "amount": "1000", "paymentTerm": "in_advance"}}},
      {"id": "line-usage", "name": "CPU", "currency": "USD", "type": "usage_based", "status": "valid", "managedBy": "subscription",
       "invoiceAt": "2026-03-18T00:00:00Z", "period": {"from": "2026-03-18T00:00:00Z", "to": "2026-04-18T00:00:00Z"},
       "rateCard": {"price": {"type": "unit", "amount": "0.01"}, "featureKey": "cpu"}},
      {"id": "line-deleted", "name": "Old fee", "currency": "USD", "type": "usage_based", "status": "valid", "managedBy": "subscription",
       "deletedAt": "2026-03-18T01:00:00Z",
       "invoiceAt": "2026-03-18T00:00:00Z", "period": {"from": "2026-03-18T00:00:00Z", "to": "2026-04-18T00:00:00Z"},
       "rateCard": {"price": {"type": "flat", "amount": "50"}}}
    ]
  }],
  "page": 1, "pageSize": 100, "totalCount": 1
}`

// subscriptionJSON gives the monthly fee a 10% discount.
const subscriptionJSON = `{
  "id": "sub-1", "status": "active",
  "phases": [{"id": "phase-1", "key": "default", "items": [
    {"id": "item-setup", "key": "setup", "name": "Server installation"},
    {"id": "item-monthly", "key": "monthly", "name": "Dedicated server", "discounts": {"percentage": {"percentage": 10}}}
  ]}]
}`

type fixedChargesServer struct {
	listQuery     map[string][]string
	subscriptions []string
	simulated     om.InvoiceSimulationInput
	invoiced      om.InvoicePendingLinesActionInput
	invoiceStatus int
	invoiceBody   string
}

func newFixedChargesClient(t *testing.T, f *fixedChargesServer) Client {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
	mux.HandleFunc("GET /api/v1/billing/invoices", func(w http.ResponseWriter, r *http.Request) {
		f.listQuery = r.URL.Query()
		writeJSON(w, http.StatusOK, gatheringInvoiceJSON)
	})
	mux.HandleFunc("GET /api/v1/subscriptions/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.subscriptions = append(f.subscriptions, r.PathValue("id"))
		writeJSON(w, http.StatusOK, subscriptionJSON)
	})
	mux.HandleFunc("POST /api/v1/billing/customers/{id}/invoices/simulate", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&f.simulated); err != nil {
			t.Errorf("decoding simulation: %v", err)
		}
		writeJSON(w, http.StatusOK, `{"id": "", "currency": "USD", "totals": {"total": "2400.00", "taxesExclusiveTotal": "0"}}`)
	})
	mux.HandleFunc("POST /api/v1/billing/invoices/invoice", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&f.invoiced); err != nil {
			t.Errorf("decoding invoice action: %v", err)
		}
		writeJSON(w, f.invoiceStatus, f.invoiceBody)
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

func TestPendingFixedCharges(t *testing.T) {
	f := &fixedChargesServer{}
	c := newFixedChargesClient(t, f)
	asOf := time.Date(2026, time.March, 18, 10, 0, 0, 0, time.UTC)

	charges, err := c.PendingFixedCharges(context.Background(), fixedChargesCustomer, asOf)
	if err != nil {
		t.Fatalf("PendingFixedCharges: %v", err)
	}

	if got := f.listQuery["statuses"]; !slices.Equal(got, []string{"gathering"}) {
		t.Errorf("statuses = %v, want only gathering invoices", got)
	}
	if got := f.listQuery["expand"]; !slices.Equal(got, []string{"lines"}) {
		t.Errorf("expand = %v, want lines", got)
	}

	var ids []string
	for _, ch := range charges {
		ids = append(ids, ch.LineID)
	}
	// Not next month's fee (not billable yet), the usage line, or the
	// deleted line.
	if want := []string{"line-setup", "line-monthly"}; !slices.Equal(ids, want) {
		t.Fatalf("charges = %v, want %v", ids, want)
	}
	if charges[0].Discounts != nil {
		t.Errorf("setup fee discounts = %+v, want none", charges[0].Discounts)
	}
	if d := charges[1].Discounts; d == nil || d.Percentage == nil || percentage(t, d.Percentage.Percentage) != "10" {
		t.Errorf("monthly fee discounts = %+v, want the subscription item's 10%%", d)
	}
	if len(f.subscriptions) != 1 {
		t.Errorf("subscription reads = %v, want one per subscription", f.subscriptions)
	}
}

func TestPreviewInvoice_SimulatesWithDiscounts(t *testing.T) {
	f := &fixedChargesServer{}
	c := newFixedChargesClient(t, f)
	charges, err := c.PendingFixedCharges(context.Background(), fixedChargesCustomer, time.Date(2026, time.March, 18, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	inv, err := c.PreviewInvoice(context.Background(), fixedChargesCustomer, charges)
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
	monthly := sim.Lines[1]
	if monthly.Id == nil || *monthly.Id != "line-monthly" || monthly.Quantity != "1" {
		t.Errorf("monthly line = %+v, want its id and quantity 1", monthly)
	}
	if monthly.RateCard == nil || monthly.RateCard.Discounts == nil || monthly.RateCard.Discounts.Percentage == nil ||
		percentage(t, monthly.RateCard.Discounts.Percentage.Percentage) != "10" {
		t.Errorf("monthly rate card = %+v, want the 10%% discount", monthly.RateCard)
	}
	if sim.Lines[0].RateCard.Discounts != nil {
		t.Errorf("setup rate card discounts = %+v, want none", sim.Lines[0].RateCard.Discounts)
	}
}

func TestInvoiceFixedCharges(t *testing.T) {
	asOf := time.Date(2026, time.March, 18, 10, 0, 0, 0, time.UTC)

	t.Run("invoices only the given lines", func(t *testing.T) {
		f := &fixedChargesServer{invoiceStatus: http.StatusCreated, invoiceBody: `[{"id": "01JINVOICE", "status": "draft", "currency": "USD"}]`}
		c := newFixedChargesClient(t, f)

		invoices, err := c.InvoiceFixedCharges(context.Background(), fixedChargesCustomer, []string{"line-setup", "line-monthly"}, asOf)
		if err != nil {
			t.Fatalf("InvoiceFixedCharges: %v", err)
		}
		if len(invoices) != 1 || invoices[0].Id != "01JINVOICE" {
			t.Errorf("invoices = %+v", invoices)
		}
		got := f.invoiced
		if got.CustomerId != string(fixedChargesCustomer) || got.AsOf == nil || !got.AsOf.Equal(asOf) {
			t.Errorf("action = %+v, want the customer as of %s", got, asOf)
		}
		if got.Filters == nil || got.Filters.LineIds == nil || !slices.Equal(*got.Filters.LineIds, []string{"line-setup", "line-monthly"}) {
			t.Errorf("filters = %+v, want exactly the fixed-charge lines", got.Filters)
		}
	})

	t.Run("nothing billable", func(t *testing.T) {
		f := &fixedChargesServer{
			invoiceStatus: http.StatusBadRequest,
			invoiceBody:   `{"type": "about:blank", "title": "Bad Request", "status": 400, "detail": "the new invoice would have no lines", "extensions": {"code": "invoice_create_no_lines"}}`,
		}
		c := newFixedChargesClient(t, f)

		_, err := c.InvoiceFixedCharges(context.Background(), fixedChargesCustomer, []string{"line-setup"}, asOf)
		if !errors.Is(err, ErrNothingToInvoice) {
			t.Errorf("err = %v, want ErrNothingToInvoice", err)
		}
	})

	t.Run("no lines", func(t *testing.T) {
		c := newFixedChargesClient(t, &fixedChargesServer{})
		if _, err := c.InvoiceFixedCharges(context.Background(), fixedChargesCustomer, nil, asOf); !errors.Is(err, ErrNothingToInvoice) {
			t.Errorf("err = %v, want ErrNothingToInvoice", err)
		}
	})
}

func TestAmountBeforeTax(t *testing.T) {
	for _, tc := range []struct {
		total, exclusiveTax, want string
	}{
		{"2400.00", "0", "2400.00"},
		{"2640.00", "240.00", "2400.00"},
		{"10.5", "", "10.5"},
		{"0", "0", "0"},
	} {
		got, err := AmountBeforeTax(om.InvoiceTotals{Total: tc.total, TaxesExclusiveTotal: tc.exclusiveTax})
		if err != nil || got != tc.want {
			t.Errorf("AmountBeforeTax(%s, %s) = %q, %v; want %q", tc.total, tc.exclusiveTax, got, err, tc.want)
		}
	}
	if _, err := AmountBeforeTax(om.InvoiceTotals{Total: "lots"}); err == nil {
		t.Error("an invalid total: want an error")
	}
}
