// SPDX-License-Identifier: AGPL-3.0-only

package stripe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeKey(t *testing.T, key string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "apiKey")
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestLinks(t *testing.T, key, baseURL string) *InvoiceLinks {
	t.Helper()
	links, err := NewInvoiceLinks(writeKey(t, key))
	if err != nil {
		t.Fatalf("NewInvoiceLinks: %v", err)
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
	links := newTestLinks(t, "sk_test_123\n", srv.URL)
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

	wrongKey := newTestLinks(t, "sk_test_wrong", srv.URL)
	if _, err := wrongKey.HostedInvoiceURL(ctx, "in_open"); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("wrong key: err %v, want HTTP 401", err)
	}
}

func TestNewInvoiceLinks_RequiresAKey(t *testing.T) {
	for name, file := range map[string]string{
		"no file set":  "",
		"file missing": filepath.Join(t.TempDir(), "absent"),
		"empty file":   writeKey(t, " \n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewInvoiceLinks(file); err == nil {
				t.Error("NewInvoiceLinks succeeded, want an error")
			}
		})
	}
}
