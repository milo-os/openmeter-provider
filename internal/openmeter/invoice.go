// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// invoiceListPageSize caps each ListInvoices page. A BillingAccount
// accumulates one invoice per billing period (typically monthly), so most
// accounts fit in a single page — but a long-lived account can outgrow it,
// so ListInvoices walks every page (see the TotalCount check below) rather
// than assuming the first one is enough.
const invoiceListPageSize = 100

// ListInvoices returns every invoice OpenMeter has for the customer
// identified by customerID, most-recent first. An empty (nil) slice with a
// nil error means the customer has no invoices yet — that is a healthy
// state, not an error (mirrors amberflo-provider's "NoInvoicesYet"
// treatment).
//
// customerID must be OpenMeter's internal customer id (a ULID), not the
// external key — confirmed by the server: passing an external key here
// 400s with "parameter \"customers\" in query has an error: ... string
// doesn't match the regular expression \"^[0-7[][0-9A-HJKMNP-TV-Za-hjkmnp-
// tv-z]{25}$\"" (the same ULID pattern
// UpsertBillingProfileCustomerOverride's doc comment documents). This was
// previously passed the external key on the (explicitly flagged as
// unverified) assumption that the Customers param's matching semantics
// were undocumented but permissive; they are not.
//
// Results are filtered client-side against Invoice.Customer.Id (not .Key,
// since callers now supply the internal id) in addition to the server-side
// Customers query param, on the same "don't trust it blindly" principle.
func (c *client) ListInvoices(ctx context.Context, customerID string) ([]om.Invoice, error) {
	if customerID == "" {
		return nil, &PermanentError{Err: errors.New("customerID is required")}
	}

	pageSize := om.PaginationPageSize(invoiceListPageSize)
	var out []om.Invoice
	for page := 1; ; page++ {
		pageNum := om.PaginationPage(page)
		params := &om.ListInvoicesParams{
			Customers: &om.InvoiceListParamsCustomers{customerID},
			Page:      &pageNum,
			PageSize:  &pageSize,
			Order:     ptr(om.SortOrderDESC),
			OrderBy:   ptr(om.InvoiceOrderByCreatedAt),
		}

		resp, err := c.api.ListInvoicesWithResponse(ctx, params)
		if err != nil {
			return nil, classify(nil, nil, err)
		}
		if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
			return nil, err
		}
		if resp.JSON200 == nil {
			return nil, &PermanentError{Err: fmt.Errorf("list invoices for %q: empty response body", customerID)}
		}

		for _, inv := range resp.JSON200.Items {
			if inv.Customer.Id == nil || *inv.Customer.Id != customerID {
				continue
			}
			out = append(out, inv)
		}

		// Page off the size the server reports, not the size requested — a
		// server-side clamp below invoiceListPageSize would otherwise end
		// the walk early and silently drop older invoices.
		seen := resp.JSON200.PageSize
		if seen <= 0 {
			seen = len(resp.JSON200.Items)
		}
		if len(resp.JSON200.Items) == 0 || page*seen >= resp.JSON200.TotalCount {
			return out, nil
		}
	}
}

// ptr returns a pointer to v. Small helper for constructing SDK params that
// take pointers to enum-typed values.
func ptr[T any](v T) *T {
	return &v
}
