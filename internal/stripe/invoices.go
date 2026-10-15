// SPDX-License-Identifier: AGPL-3.0-only

// Package stripe reads from Stripe what OpenMeter does not keep: the link to
// each invoice OpenMeter issues through its Stripe app.
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

// InvoiceLinks reads invoice links from Stripe with the API key of the
// Stripe account OpenMeter's Stripe app issues invoices in.
type InvoiceLinks struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

// NewInvoiceLinks reads the Stripe API key from apiKeyFile and fails if the
// file is missing or empty.
func NewInvoiceLinks(apiKeyFile string) (*InvoiceLinks, error) {
	raw, err := os.ReadFile(apiKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read Stripe API key: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return nil, fmt.Errorf("stripe API key file %s is empty", apiKeyFile)
	}
	return &InvoiceLinks{
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
func (c *InvoiceLinks) HostedInvoiceURL(ctx context.Context, invoiceID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v1/invoices/"+url.PathEscape(invoiceID), nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.apiKey, "")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("get Stripe invoice %s: %w", invoiceID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read Stripe invoice %s: %w", invoiceID, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		return "", fmt.Errorf("get Stripe invoice %s: HTTP %d: %s", invoiceID, resp.StatusCode, e.Error.Message)
	}

	var invoice struct {
		HostedInvoiceURL *string `json:"hosted_invoice_url"`
	}
	if err := json.Unmarshal(body, &invoice); err != nil {
		return "", fmt.Errorf("decode Stripe invoice %s: %w", invoiceID, err)
	}
	if invoice.HostedInvoiceURL == nil {
		return "", nil
	}
	return *invoice.HostedInvoiceURL, nil
}
