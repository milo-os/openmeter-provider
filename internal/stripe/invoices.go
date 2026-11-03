// SPDX-License-Identifier: AGPL-3.0-only

// Package stripe reads from Stripe what OpenMeter does not keep: the
// invoices in the Stripe account OpenMeter's Stripe app issues invoices in,
// both OpenMeter's and those staff create there by hand, with the links,
// numbers and amounts (credit notes, partial payments) Stripe has for them.
// It also writes the Milo invoice's name onto each one, so staff can find in
// Stripe an invoice they see in the portal.
package stripe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultBaseURL is Stripe's API.
const DefaultBaseURL = "https://api.stripe.com"

// Stripe invoice statuses.
const (
	InvoiceStatusDraft         = "draft"
	InvoiceStatusOpen          = "open"
	InvoiceStatusPaid          = "paid"
	InvoiceStatusUncollectible = "uncollectible"
	InvoiceStatusVoid          = "void"
)

// MetadataOpenMeterInvoiceID is the metadata key OpenMeter's Stripe app sets
// to its own invoice id on every Stripe invoice it creates.
const MetadataOpenMeterInvoiceID = "om_invoice_id"

// maxInvoicePages bounds ListCustomerInvoices: 20 pages of 100 is far more
// invoices than one customer has.
const maxInvoicePages = 20

// Invoice is a Stripe invoice, with amounts in the currency's minor unit
// (cents for USD).
type Invoice struct {
	ID string
	// Number is the invoice number printed on the invoice, set once it is
	// finalized.
	Number   string
	Status   string
	Currency string // ISO 4217, upper case
	Total    int64
	// AmountDue is what the customer owes after credit notes issued before
	// payment, and AmountRemaining what's still to pay of it.
	AmountDue       int64
	AmountPaid      int64
	AmountRemaining int64
	Created         time.Time
	PeriodStart     time.Time
	PeriodEnd       time.Time
	DueDate         *time.Time
	PaidAt          *time.Time
	// HostedInvoiceURL is "" while the invoice is a draft.
	HostedInvoiceURL string
	Metadata         map[string]string
}

// Invoices reads and annotates invoices in Stripe with the API key of the
// Stripe account OpenMeter's Stripe app issues invoices in.
type Invoices struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

// NewInvoices reads the Stripe API key from apiKeyFile and fails if the
// file is missing or empty.
func NewInvoices(apiKeyFile string) (*Invoices, error) {
	raw, err := os.ReadFile(apiKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read Stripe API key: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return nil, fmt.Errorf("stripe API key file %s is empty", apiKeyFile)
	}
	return &Invoices{
		apiKey:     key,
		baseURL:    DefaultBaseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// HostedInvoiceURL returns the URL of the Stripe-hosted page of the invoice
// with id invoiceID, where the customer can view it, download its PDF and
// pay it. It is "" while the invoice is a draft.
//
// Stripe expires the link 30 days after the invoice's due date, but a URL
// read through the API is guaranteed to work for at least 10 days, so
// callers must read it again before then.
func (c *Invoices) HostedInvoiceURL(ctx context.Context, invoiceID string) (string, error) {
	var invoice apiInvoice
	if err := c.do(ctx, http.MethodGet, "/v1/invoices/"+url.PathEscape(invoiceID), nil, &invoice); err != nil {
		return "", fmt.Errorf("get Stripe invoice %s: %w", invoiceID, err)
	}
	if invoice.HostedInvoiceURL == nil {
		return "", nil
	}
	return *invoice.HostedInvoiceURL, nil
}

// ListCustomerInvoices returns every invoice of the Stripe customer, drafts
// included, newest first.
func (c *Invoices) ListCustomerInvoices(ctx context.Context, customerID string) ([]Invoice, error) {
	var out []Invoice
	query := url.Values{"customer": {customerID}, "limit": {"100"}}
	for range maxInvoicePages {
		var page struct {
			Data    []apiInvoice `json:"data"`
			HasMore bool         `json:"has_more"`
		}
		if err := c.do(ctx, http.MethodGet, "/v1/invoices?"+query.Encode(), nil, &page); err != nil {
			return nil, fmt.Errorf("list Stripe invoices of customer %s: %w", customerID, err)
		}
		for _, inv := range page.Data {
			out = append(out, inv.invoice())
		}
		if !page.HasMore || len(page.Data) == 0 {
			return out, nil
		}
		query.Set("starting_after", page.Data[len(page.Data)-1].ID)
	}
	return nil, fmt.Errorf("list Stripe invoices of customer %s: more than %d pages", customerID, maxInvoicePages)
}

// SetInvoiceMetadata sets the given metadata keys on the invoice, leaving
// its other keys as they are. Stripe allows this on an invoice in any
// status.
func (c *Invoices) SetInvoiceMetadata(ctx context.Context, invoiceID string, metadata map[string]string) error {
	form := url.Values{}
	for k, v := range metadata {
		form.Set("metadata["+k+"]", v)
	}
	if err := c.do(ctx, http.MethodPost, "/v1/invoices/"+url.PathEscape(invoiceID), form, nil); err != nil {
		return fmt.Errorf("set metadata on Stripe invoice %s: %w", invoiceID, err)
	}
	return nil
}

// do sends one request to Stripe and decodes the response into out, when
// out isn't nil. A POST sends form as its body.
func (c *Invoices) do(ctx context.Context, method, path string, form url.Values, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.apiKey, "")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Error.Message)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// apiInvoice is the part of Stripe's invoice object this package reads.
type apiInvoice struct {
	ID                string            `json:"id"`
	Number            *string           `json:"number"`
	Status            *string           `json:"status"`
	Currency          string            `json:"currency"`
	Total             int64             `json:"total"`
	AmountDue         int64             `json:"amount_due"`
	AmountPaid        int64             `json:"amount_paid"`
	AmountRemaining   int64             `json:"amount_remaining"`
	Created           int64             `json:"created"`
	PeriodStart       int64             `json:"period_start"`
	PeriodEnd         int64             `json:"period_end"`
	DueDate           *int64            `json:"due_date"`
	HostedInvoiceURL  *string           `json:"hosted_invoice_url"`
	Metadata          map[string]string `json:"metadata"`
	StatusTransitions struct {
		PaidAt *int64 `json:"paid_at"`
	} `json:"status_transitions"`
}

func (a apiInvoice) invoice() Invoice {
	return Invoice{
		ID:               a.ID,
		Number:           deref(a.Number),
		Status:           deref(a.Status),
		Currency:         strings.ToUpper(a.Currency),
		Total:            a.Total,
		AmountDue:        a.AmountDue,
		AmountPaid:       a.AmountPaid,
		AmountRemaining:  a.AmountRemaining,
		Created:          unix(a.Created),
		PeriodStart:      unix(a.PeriodStart),
		PeriodEnd:        unix(a.PeriodEnd),
		DueDate:          unixPtr(a.DueDate),
		PaidAt:           unixPtr(a.StatusTransitions.PaidAt),
		HostedInvoiceURL: deref(a.HostedInvoiceURL),
		Metadata:         a.Metadata,
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func unix(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

func unixPtr(sec *int64) *time.Time {
	if sec == nil || *sec == 0 {
		return nil
	}
	t := unix(*sec)
	return &t
}

// zeroDecimalCurrencies are the currencies Stripe counts in whole units
// rather than hundredths (https://docs.stripe.com/currencies#zero-decimal).
var zeroDecimalCurrencies = map[string]bool{
	"BIF": true, "CLP": true, "DJF": true, "GNF": true, "JPY": true, "KMF": true,
	"KRW": true, "MGA": true, "PYG": true, "RWF": true, "UGX": true, "VND": true,
	"VUV": true, "XAF": true, "XOF": true, "XPF": true,
}

// FormatAmount writes an amount in currency's minor unit as a decimal
// string, the way Milo Invoices carry amounts: 123456 USD is "1234.56".
func FormatAmount(minor int64, currency string) string {
	if zeroDecimalCurrencies[strings.ToUpper(currency)] {
		return fmt.Sprintf("%d", minor)
	}
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	return fmt.Sprintf("%s%d.%02d", sign, minor/100, minor%100)
}
