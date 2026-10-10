// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"

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
func (c *client) ListInvoices(ctx context.Context, customerID CustomerID) ([]om.Invoice, error) {
	if customerID == "" {
		return nil, &PermanentError{Err: errors.New("customerID is required")}
	}

	pageSize := om.PaginationPageSize(invoiceListPageSize)
	var out []om.Invoice
	for page := 1; ; page++ {
		pageNum := om.PaginationPage(page)
		params := &om.ListInvoicesParams{
			Customers: &om.InvoiceListParamsCustomers{string(customerID)},
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
			if inv.Customer.Id == nil || *inv.Customer.Id != string(customerID) {
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

// ErrInvoicesOutstanding is returned by PrepareInvoicesForCustomerDeletion
// when the customer still has invoices that carry a balance and are not in a
// final state. OpenMeter refuses to delete such a customer; deleting or
// voiding them would forgo revenue, so the caller has to wait for them to be
// paid (or for someone to void them / mark them uncollectible).
var ErrInvoicesOutstanding = errors.New("openmeter: customer has outstanding invoices")

// finalInvoiceStatuses are the statuses OpenMeter accepts when deleting a
// customer (billing.StandardInvoiceStatus.IsFinal on the server; deleted
// invoices are not listed).
var finalInvoiceStatuses = []om.InvoiceStatus{
	om.InvoiceStatusPaid,
	om.InvoiceStatusVoided,
	om.InvoiceStatusUncollectible,
}

// PrepareInvoicesForCustomerDeletion clears what blocks deleting a customer
// without losing revenue:
//
//   - pending lines (a "gathering" invoice) are invoiced now, because
//     OpenMeter refuses to delete a customer with a gathering invoice;
//   - draft invoices with a zero total are deleted — nothing is owed, and
//     ending a subscription early leaves one behind for every closed period;
//   - every other non-final invoice is left alone and returned wrapped in
//     ErrInvoicesOutstanding.
//
// A nil error means the customer can be deleted as far as invoices go.
func (c *client) PrepareInvoicesForCustomerDeletion(ctx context.Context, customerID CustomerID) error {
	invoices, err := c.ListInvoices(ctx, customerID)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(invoices, func(inv om.Invoice) bool { return inv.Status == om.InvoiceStatusGathering }) {
		if err := c.invoicePendingLines(ctx, customerID); err != nil {
			return fmt.Errorf("invoice pending lines: %w", err)
		}
		if invoices, err = c.ListInvoices(ctx, customerID); err != nil {
			return err
		}
	}

	var outstanding []string
	for _, inv := range invoices {
		switch {
		case slices.Contains(finalInvoiceStatuses, inv.Status):
			continue
		case inv.Status == om.InvoiceStatusDraft && isZeroAmount(inv.Totals.Total):
			if err := c.deleteInvoice(ctx, inv.Id); err != nil {
				return fmt.Errorf("delete zero-total draft invoice %s: %w", inv.Id, err)
			}
		default:
			outstanding = append(outstanding, fmt.Sprintf("%s (%s, total %s %s)", inv.Id, inv.Status, inv.Totals.Total, inv.Currency))
		}
	}
	if len(outstanding) > 0 {
		return fmt.Errorf("%w: %s", ErrInvoicesOutstanding, strings.Join(outstanding, ", "))
	}
	return nil
}

func (c *client) invoicePendingLines(ctx context.Context, customerID CustomerID) error {
	resp, err := c.api.InvoicePendingLinesActionWithResponse(ctx, om.InvoicePendingLinesActionJSONRequestBody{
		CustomerId: string(customerID),
	})
	if err != nil {
		return classify(nil, nil, err)
	}
	return classify(resp.HTTPResponse, resp.Body, nil)
}

func (c *client) deleteInvoice(ctx context.Context, id string) error {
	resp, err := c.api.DeleteInvoiceWithResponse(ctx, id)
	if err != nil {
		return classify(nil, nil, err)
	}
	if resp.StatusCode() == http.StatusNotFound {
		return nil
	}
	return classify(resp.HTTPResponse, resp.Body, nil)
}

// isZeroAmount reports whether a decimal amount string is zero. An
// unparseable amount is treated as non-zero so it is never deleted.
func isZeroAmount(amount string) bool {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(amount))
	return ok && r.Sign() == 0
}
