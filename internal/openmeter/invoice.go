// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// invoiceListPageSize caps each ListInvoices page. A BillingAccount
// accumulates one or more invoices per billing period, so most accounts fit
// in a single page — but a long-lived account can outgrow it, so
// ListInvoices walks every page (see the TotalCount check below) rather
// than assuming the first one is enough.
const invoiceListPageSize = 100

// Invoice short statuses the generated SDK has no constants for. The API
// reports an invoice's status as the prefix of its extended status (see
// statusDetails.extendedStatus), and a standard invoice that is being
// deleted, or was deleted, reports these.
const (
	// InvoiceStatusDeleting covers delete.in_progress, delete.syncing and
	// delete.failed.
	InvoiceStatusDeleting om.InvoiceStatus = "delete"
	// InvoiceStatusDeleted is a deleted invoice. Only listed when deleted
	// invoices are requested (see ListInvoicesIncludingDeleted).
	InvoiceStatusDeleted om.InvoiceStatus = "deleted"
)

// invoiceExtendedStatusDeleteFailed is the extended status of an invoice
// whose deletion failed. OpenMeter only lets it be deleted again.
const invoiceExtendedStatusDeleteFailed = "delete.failed"

// IsInvoiceDeleted reports whether inv has been deleted in OpenMeter.
func IsInvoiceDeleted(inv om.Invoice) bool {
	return inv.Status == InvoiceStatusDeleted || inv.DeletedAt != nil
}

// ListInvoices returns every non-deleted invoice OpenMeter has for the
// customer identified by customerID, most-recent first. An empty (nil)
// slice with a nil error means the customer has no invoices yet — that is a
// healthy state, not an error (mirrors amberflo-provider's "NoInvoicesYet"
// treatment).
//
// customerID must be OpenMeter's internal customer id (a ULID), not the
// external key — confirmed by the server: passing an external key here
// 400s with "parameter \"customers\" in query has an error: ... string
// doesn't match the regular expression \"^[0-7[][0-9A-HJKMNP-TV-Za-hjkmnp-
// tv-z]{25}$\"" (the same ULID pattern
// UpsertBillingProfileCustomerOverride's doc comment documents).
//
// Results are filtered client-side as well as by the server-side Customers
// query param — see invoiceBelongsToCustomer.
func (c *client) ListInvoices(ctx context.Context, customerID CustomerID) ([]om.Invoice, error) {
	return c.listInvoices(ctx, customerID, false)
}

// ListInvoicesIncludingDeleted is ListInvoices with deleted invoices
// included (OpenMeter's includeDeleted). The invoice projection needs it to
// tell an invoice that was deleted — whose Milo Invoice must be closed out
// — from one that is merely missing from the response.
func (c *client) ListInvoicesIncludingDeleted(ctx context.Context, customerID CustomerID) ([]om.Invoice, error) {
	return c.listInvoices(ctx, customerID, true)
}

func (c *client) listInvoices(ctx context.Context, customerID CustomerID, includeDeleted bool) ([]om.Invoice, error) {
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
		if includeDeleted {
			params.IncludeDeleted = ptr(true)
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
			if invoiceBelongsToCustomer(inv, customerID) {
				out = append(out, inv)
			}
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

// invoiceBelongsToCustomer is the client-side half of ListInvoices'
// customer filter, on a "don't trust the server-side filter blindly"
// principle: an invoice whose Customer.Id names another customer is
// dropped.
//
// A gathering invoice can come back WITHOUT a customer id: OpenMeter's
// gathering-invoice mapping (httpdriver MapGatheringInvoiceToAPI) builds
// the customer snapshot from key and name only, so Customer.Id is ""
// whenever the gathering invoice is not recalculated into a standard
// one. Dropping those (as an earlier version did) hid the very invoice
// that blocks customer deletion ("invoice ... is still in gathering
// state"), so PrepareInvoicesForCustomerDeletion never invoiced the
// pending lines. Such invoices are kept on the strength of the server-side
// Customers filter (which the server applies to gathering invoices by
// their customer_id column regardless of how the response is mapped).
// Only gathering invoices get this allowance, and nothing projects them
// into Milo: they only trigger InvoicePendingLinesAction, which is itself
// scoped to customerID.
func invoiceBelongsToCustomer(inv om.Invoice, customerID CustomerID) bool {
	id := ""
	if inv.Customer.Id != nil {
		id = *inv.Customer.Id
	}
	if id == "" {
		return inv.Status == om.InvoiceStatusGathering
	}
	return id == string(customerID)
}

// ptr returns a pointer to v. Small helper for constructing SDK params that
// take pointers to enum-typed values.
func ptr[T any](v T) *T {
	return &v
}

// ErrInvoicesOutstanding is returned by PrepareInvoicesForCustomerDeletion
// when the customer still has invoices that block deleting it and that this
// client will not clear on its own: invoices with a balance that are not
// in a final state (deleting or voiding them would forgo revenue, so the
// caller has to wait for them to be paid, or for someone to void them /
// mark them uncollectible), a gathering invoice holding lines that are not
// billable yet, and invoices whose deletion is in progress or failed.
var ErrInvoicesOutstanding = errors.New("openmeter: customer has outstanding invoices")

// errNothingToInvoice is invoicePendingLines' report that OpenMeter found no
// billable pending lines.
var errNothingToInvoice = errors.New("openmeter: no pending lines to invoice")

// invoiceCreateNoLinesCode is the validation-issue code OpenMeter answers
// InvoicePendingLinesAction with (HTTP 400) when nothing is billable yet —
// billing.ErrInvoiceCreateNoLines — and invoiceCreateNoLinesMessage its
// message, which the problem's detail carries.
const (
	invoiceCreateNoLinesCode    = "invoice_create_no_lines"
	invoiceCreateNoLinesMessage = "the new invoice would have no lines"
)

// finalInvoiceStatuses are the statuses OpenMeter accepts when deleting a
// customer (billing.StandardInvoiceStatus.IsFinal on the server: deleted,
// paid, uncollectible, voided). Deleted invoices are not listed by
// ListInvoices, but listing them is harmless.
var finalInvoiceStatuses = []om.InvoiceStatus{
	om.InvoiceStatusPaid,
	om.InvoiceStatusVoided,
	om.InvoiceStatusUncollectible,
	InvoiceStatusDeleted,
}

// PrepareInvoicesForCustomerDeletion clears what blocks deleting a customer
// without losing revenue:
//
//   - pending lines (a "gathering" invoice) are invoiced now, because
//     OpenMeter refuses to delete a customer with a gathering invoice.
//     OpenMeter moves the billable lines onto a NEW standard invoice;
//     "nothing billable" (a 400 with code invoice_create_no_lines) is not
//     an error. A gathering invoice that survives — it still holds lines
//     whose invoiceAt is in the future — cannot be deleted through the API
//     and is reported as outstanding until subscription sync removes them;
//   - draft invoices with a zero total are deleted — nothing is owed, and
//     ending a subscription early leaves one behind for every closed period;
//   - invoices already being deleted are waited for, and ones whose
//     deletion failed are reported (OpenMeter only lets them be deleted
//     again, which needs an operator to look at why it failed);
//   - every other non-final invoice is left alone.
//
// Everything left over is returned wrapped in ErrInvoicesOutstanding. A nil
// error means the customer can be deleted as far as invoices go.
func (c *client) PrepareInvoicesForCustomerDeletion(ctx context.Context, customerID CustomerID) error {
	invoices, err := c.ListInvoices(ctx, customerID)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(invoices, func(inv om.Invoice) bool { return inv.Status == om.InvoiceStatusGathering }) {
		if err := c.invoicePendingLines(ctx, customerID); err != nil && !errors.Is(err, errNothingToInvoice) {
			return fmt.Errorf("invoice pending lines: %w", err)
		}
		if invoices, err = c.ListInvoices(ctx, customerID); err != nil {
			return err
		}
	}

	deleted := false
	for _, inv := range invoices {
		if inv.Status == om.InvoiceStatusDraft && isZeroAmount(inv.Totals.Total) {
			if err := c.deleteInvoice(ctx, inv.Id); err != nil {
				return fmt.Errorf("delete zero-total draft invoice %s: %w", inv.Id, err)
			}
			deleted = true
		}
	}
	if deleted {
		// Deletion runs through OpenMeter's invoice state machine
		// (delete.in_progress → delete.syncing → deleted) and may not have
		// finished by the time DELETE returns; look again rather than
		// assuming it did.
		if invoices, err = c.ListInvoices(ctx, customerID); err != nil {
			return err
		}
	}

	var outstanding []string
	for _, inv := range invoices {
		if reason := invoiceBlocksCustomerDeletion(inv); reason != "" {
			outstanding = append(outstanding, fmt.Sprintf("%s (%s)", inv.Id, reason))
		}
	}
	if len(outstanding) > 0 {
		return fmt.Errorf("%w: %s", ErrInvoicesOutstanding, strings.Join(outstanding, ", "))
	}
	return nil
}

// invoiceBlocksCustomerDeletion explains why inv still blocks deleting its
// customer, or returns "" when it does not.
func invoiceBlocksCustomerDeletion(inv om.Invoice) string {
	switch {
	case slices.Contains(finalInvoiceStatuses, inv.Status), inv.DeletedAt != nil:
		return ""
	case inv.Status == om.InvoiceStatusGathering:
		// Gathering totals are not meaningful (OpenMeter reports them
		// empty, or as a live preview), so none is quoted.
		return "gathering, holds pending lines that are not billable yet"
	case inv.Status == InvoiceStatusDeleting:
		if inv.StatusDetails.ExtendedStatus == invoiceExtendedStatusDeleteFailed || inv.StatusDetails.Failed {
			return fmt.Sprintf("deletion failed (%s); it must be deleted again in OpenMeter", inv.StatusDetails.ExtendedStatus)
		}
		return "deletion in progress"
	default:
		return fmt.Sprintf("%s, total %s %s", inv.Status, inv.Totals.Total, inv.Currency)
	}
}

// invoicePendingLines asks OpenMeter to invoice the customer's billable
// pending lines. Returns errNothingToInvoice when OpenMeter reports there
// is nothing billable (see isNoLinesToInvoice).
func (c *client) invoicePendingLines(ctx context.Context, customerID CustomerID) error {
	resp, err := c.api.InvoicePendingLinesActionWithResponse(ctx, om.InvoicePendingLinesActionJSONRequestBody{
		CustomerId: string(customerID),
	})
	if err != nil {
		return classify(nil, nil, err)
	}
	if resp.StatusCode() == http.StatusBadRequest && isNoLinesToInvoice(resp.Body) {
		return errNothingToInvoice
	}
	return classify(resp.HTTPResponse, resp.Body, nil)
}

// isNoLinesToInvoice reports whether body is OpenMeter's problem response
// for billing.ErrInvoiceCreateNoLines. The service answers the pending-lines
// action with it both when the customer has no gathering invoice and when
// none of its lines is billable yet (service/gatheringinvoicependinglines.go
// prepareBillableLines); the HTTP driver encodes the validation issue as
// extensions.code, and the issue's message as the problem detail.
func isNoLinesToInvoice(body []byte) bool {
	var problem struct {
		Detail     string         `json:"detail"`
		Extensions map[string]any `json:"extensions"`
	}
	if err := json.Unmarshal(body, &problem); err != nil {
		return false
	}
	if code, ok := problem.Extensions["code"].(string); ok {
		return code == invoiceCreateNoLinesCode
	}
	return strings.Contains(problem.Detail, invoiceCreateNoLinesMessage)
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
