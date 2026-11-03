// SPDX-License-Identifier: AGPL-3.0-only

package stripe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeKey(t *testing.T, key string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "apiKey")
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestInvoices(t *testing.T, key, baseURL string) *Invoices {
	t.Helper()
	links, err := NewInvoices(writeKey(t, key))
	if err != nil {
		t.Fatalf("NewInvoices: %v", err)
	}
	links.baseURL = baseURL
	return links
}

func TestHostedInvoiceURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, _, _ := r.BasicAuth(); user != "sk_test_123" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid API Key provided"}}`))
			return
		}
		switch r.URL.Path {
		case "/v1/invoices/in_open":
			_, _ = w.Write([]byte(`{"id":"in_open","hosted_invoice_url":"https://invoice.stripe.com/i/acct_1/test_abc"}`))
		case "/v1/invoices/in_draft":
			_, _ = w.Write([]byte(`{"id":"in_draft","hosted_invoice_url":null}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"No such invoice"}}`))
		}
	}))
	defer srv.Close()

	// The key file's trailing newline is not part of the key.
	links := newTestInvoices(t, "sk_test_123\n", srv.URL)
	ctx := context.Background()

	got, err := links.HostedInvoiceURL(ctx, "in_open")
	if err != nil || got != "https://invoice.stripe.com/i/acct_1/test_abc" {
		t.Errorf("open invoice: got %q, %v", got, err)
	}
	if got, err := links.HostedInvoiceURL(ctx, "in_draft"); err != nil || got != "" {
		t.Errorf("draft invoice: got %q, %v, want no link", got, err)
	}
	if _, err := links.HostedInvoiceURL(ctx, "in_missing"); err == nil || !strings.Contains(err.Error(), "No such invoice") {
		t.Errorf("missing invoice: err %v, want Stripe's message", err)
	}

	wrongKey := newTestInvoices(t, "sk_test_wrong", srv.URL)
	if _, err := wrongKey.HostedInvoiceURL(ctx, "in_open"); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("wrong key: err %v, want HTTP 401", err)
	}
}

func TestNewInvoices_RequiresAKey(t *testing.T) {
	for name, file := range map[string]string{
		"no file set":  "",
		"file missing": filepath.Join(t.TempDir(), "absent"),
		"empty file":   writeKey(t, " \n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewInvoices(file); err == nil {
				t.Error("NewInvoices succeeded, want an error")
			}
		})
	}
}

func TestListCustomerInvoices(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/invoices" || r.URL.Query().Get("customer") != "cus_1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		queries = append(queries, r.URL.RawQuery)
		if r.URL.Query().Get("starting_after") == "" {
			_, _ = w.Write([]byte(`{"has_more":true,"data":[{
				"id":"in_manual","number":"ABC-0002","status":"open","currency":"usd",
				"total":180000,"amount_due":150000,"amount_paid":50000,"amount_remaining":100000,
				"created":1762171200,"period_start":1762171200,"period_end":1762171200,
				"due_date":1764763200,"hosted_invoice_url":"https://invoice.stripe.com/i/manual",
				"metadata":{"note":"replaces ABC-0001"},"status_transitions":{"paid_at":null}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"has_more":false,"data":[{
			"id":"in_om","number":null,"status":"draft","currency":"usd","total":5000,
			"created":1762084800,"due_date":null,"hosted_invoice_url":null,
			"metadata":{"om_invoice_id":"01OM"},"status_transitions":{"paid_at":null}}]}`))
	}))
	defer srv.Close()

	got, err := newTestInvoices(t, "sk_test_123", srv.URL).ListCustomerInvoices(context.Background(), "cus_1")
	if err != nil {
		t.Fatalf("ListCustomerInvoices: %v", err)
	}
	if len(got) != 2 || len(queries) != 2 || !strings.Contains(queries[1], "starting_after=in_manual") {
		t.Fatalf("got %d invoices from queries %v, want both pages", len(got), queries)
	}
	manual := got[0]
	due := time.Unix(1764763200, 0).UTC()
	if manual.Number != "ABC-0002" || manual.Currency != "USD" || manual.AmountRemaining != 100000 ||
		manual.DueDate == nil || !manual.DueDate.Equal(due) || manual.PaidAt != nil ||
		manual.HostedInvoiceURL != "https://invoice.stripe.com/i/manual" {
		t.Errorf("manual invoice = %+v", manual)
	}
	if draft := got[1]; draft.Status != InvoiceStatusDraft || draft.Number != "" || draft.DueDate != nil ||
		draft.Metadata[MetadataOpenMeterInvoiceID] != "01OM" {
		t.Errorf("draft invoice = %+v", draft)
	}
}

func TestSetInvoiceMetadata(t *testing.T) {
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/invoices/in_1" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"No such invoice"}}`))
			return
		}
		_ = r.ParseForm()
		form = r.PostForm
		_, _ = w.Write([]byte(`{"id":"in_1"}`))
	}))
	defer srv.Close()

	c := newTestInvoices(t, "sk_test_123", srv.URL)
	if err := c.SetInvoiceMetadata(context.Background(), "in_1", map[string]string{"milo_invoice": "acme-2026-11-abc"}); err != nil {
		t.Fatalf("SetInvoiceMetadata: %v", err)
	}
	if form.Get("metadata[milo_invoice]") != "acme-2026-11-abc" || len(form) != 1 {
		t.Errorf("form = %v, want only metadata[milo_invoice]", form)
	}
	if err := c.SetInvoiceMetadata(context.Background(), "in_missing", map[string]string{"k": "v"}); err == nil ||
		!strings.Contains(err.Error(), "No such invoice") {
		t.Errorf("missing invoice: err %v, want Stripe's message", err)
	}
}

func TestFormatAmount(t *testing.T) {
	for _, tc := range []struct {
		minor    int64
		currency string
		want     string
	}{
		{123456, "USD", "1234.56"},
		{5, "usd", "0.05"},
		{0, "USD", "0.00"},
		{-1050, "EUR", "-10.50"},
		{1500, "JPY", "1500"},
	} {
		if got := FormatAmount(tc.minor, tc.currency); got != tc.want {
			t.Errorf("FormatAmount(%d, %s) = %q, want %q", tc.minor, tc.currency, got, tc.want)
		}
	}
}
