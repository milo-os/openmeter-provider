// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// Metadata keys on the pending line that stands for a billing Charge. The
// invoice writer finds the charge through them, and the provider adopts a
// line it created before a lost response through ChargeUIDMetadataKey.
const (
	ChargeMetadataKey         = "datum_charge"
	ChargeUIDMetadataKey      = "datum_charge_uid"
	ChargeQuantityMetadataKey = "datum_quantity"
	ChargeUnitMetadataKey     = "datum_unit_amount"
	ChargeDiscountMetadataKey = "datum_discount_percent"
)

// NontaxableTaxCode is Stripe's tax code for a charge that's never taxed.
const NontaxableTaxCode = "txcd_00000000"

// ChargeLine is the pending line a billing Charge becomes on the customer's
// gathering invoice.
type ChargeLine struct {
	// Charge is the charge's namespace/name and UID, kept on the line's
	// metadata.
	Charge, ChargeUID string
	// Currency is the ISO 4217 currency, e.g. USD.
	Currency string
	// Name is what the invoice calls the line: the charge's description.
	Name string
	// Amount is the amount after any discount. OpenMeter ignores a
	// percentage discount on a manual line's rate card, so the discount is
	// taken off here, and the invoice writer puts it back as a coupon.
	Amount string
	// Quantity, UnitAmount and DiscountPercent are the charge's, kept on
	// the line's metadata for the invoice writer.
	Quantity, UnitAmount, DiscountPercent string
	// From and To are the service period, half-open as OpenMeter's
	// periods are: To is the day after the charge's last day.
	From, To time.Time
	// InvoiceAt is when OpenMeter invoices the line.
	InvoiceAt time.Time
	// TaxCode is the line's Stripe tax code, or "" for the account's
	// default.
	TaxCode string
}

// ChargeLineRef is where a charge's line is.
type ChargeLineRef struct {
	LineID        string
	InvoiceID     string
	InvoiceStatus om.InvoiceStatus
	InvoiceAt     time.Time
}

// Pending reports whether the line can still be removed: it's on the
// gathering invoice, or on a draft OpenMeter is holding.
func (r ChargeLineRef) Pending() bool {
	return r.InvoiceStatus == om.InvoiceStatusGathering || r.InvoiceStatus == om.InvoiceStatusDraft
}

// ErrLineInvoiced is RemoveChargeLine's answer for a line already on an
// issued invoice, which can't be changed.
var ErrLineInvoiced = errors.New("openmeter: the line is on an issued invoice")

// chargeLineSearchPageSize is how many of the customer's newest invoices
// FindChargeLine looks through. A charge's line is on the gathering invoice
// or a recent one, never an old one.
const chargeLineSearchPageSize = 50

// CreateChargeLine adds line to the customer's gathering invoice, and
// returns where it went.
func (c *client) CreateChargeLine(ctx context.Context, customerID CustomerID, line ChargeLine) (ChargeLineRef, error) {
	if customerID == "" {
		return ChargeLineRef{}, &PermanentError{Err: errors.New("customerID is required")}
	}
	var price om.RateCardUsageBasedPrice
	if err := price.FromFlatPriceWithPaymentTerm(om.FlatPriceWithPaymentTerm{
		Type:        om.FlatPriceWithPaymentTermTypeFlat,
		Amount:      line.Amount,
		PaymentTerm: ptr(om.PricePaymentTermInAdvance),
	}); err != nil {
		return ChargeLineRef{}, &PermanentError{Err: fmt.Errorf("build flat price: %w", err)}
	}
	rateCard := &om.InvoiceUsageBasedRateCard{Price: &price}
	if line.TaxCode != "" {
		rateCard.TaxConfig = &om.TaxConfig{Stripe: &om.StripeTaxConfig{Code: line.TaxCode}}
	}
	metadata := om.Metadata{
		ChargeMetadataKey:         line.Charge,
		ChargeUIDMetadataKey:      line.ChargeUID,
		ChargeQuantityMetadataKey: line.Quantity,
		ChargeUnitMetadataKey:     line.UnitAmount,
	}
	if line.DiscountPercent != "" {
		metadata[ChargeDiscountMetadataKey] = line.DiscountPercent
	}

	resp, err := c.api.CreatePendingInvoiceLineWithResponse(ctx, string(customerID), om.InvoicePendingLineCreateInput{
		Currency: line.Currency,
		Lines: []om.InvoicePendingLineCreate{{
			Name:      line.Name,
			InvoiceAt: line.InvoiceAt,
			Period:    om.Period{From: line.From, To: line.To},
			RateCard:  rateCard,
			Metadata:  &metadata,
		}},
	})
	if err != nil {
		return ChargeLineRef{}, classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return ChargeLineRef{}, err
	}
	created := resp.JSON201
	if created == nil || len(created.Lines) != 1 {
		return ChargeLineRef{}, &PermanentError{Err: errors.New("create pending line: unexpected response")}
	}
	return ChargeLineRef{
		LineID:        created.Lines[0].Id,
		InvoiceID:     created.Invoice.Id,
		InvoiceStatus: created.Invoice.Status,
		InvoiceAt:     created.Lines[0].InvoiceAt,
	}, nil
}

// FindChargeLine finds the line OpenMeter holds for the charge with
// chargeUID, on any of the customer's invoices. It returns nil when there's
// none.
func (c *client) FindChargeLine(ctx context.Context, customerID CustomerID, chargeUID string) (*ChargeLineRef, error) {
	if customerID == "" {
		return nil, &PermanentError{Err: errors.New("customerID is required")}
	}
	pageSize := om.PaginationPageSize(chargeLineSearchPageSize)
	resp, err := c.api.ListInvoicesWithResponse(ctx, &om.ListInvoicesParams{
		Customers: &om.InvoiceListParamsCustomers{string(customerID)},
		Expand:    &om.InvoiceListParamsExpand{om.InvoiceExpandLines},
		PageSize:  &pageSize,
		Order:     ptr(om.SortOrderDESC),
		OrderBy:   ptr(om.InvoiceOrderByCreatedAt),
	})
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
		if !invoiceBelongsToCustomer(inv, customerID) {
			continue
		}
		if IsInvoiceDeleted(inv) {
			continue
		}
		for _, line := range LineItems(inv) {
			if line.Metadata != nil && (*line.Metadata)[ChargeUIDMetadataKey] == chargeUID {
				return &ChargeLineRef{LineID: line.Id, InvoiceID: inv.Id, InvoiceStatus: inv.Status, InvoiceAt: line.InvoiceAt}, nil
			}
		}
	}
	return nil, nil
}

// chargeLineRemoveAttempts bounds RemoveChargeLine's retries when the
// invoice changes under it.
const chargeLineRemoveAttempts = 3

// errInvoiceChanged is removeLineOnce's report that the invoice changed
// between reading it and writing it back.
var errInvoiceChanged = errors.New("openmeter: invoice changed while removing a line")

// RemoveChargeLine takes the line off its invoice: the gathering invoice,
// or a draft OpenMeter is holding. OpenMeter has no endpoint for one line,
// so it reads the invoice and replaces it without the line. Subscription
// sync can change the same invoice meanwhile, so it reads it again just
// before writing, and starts over if it moved. A line already gone is
// success; one on an issued invoice is ErrLineInvoiced.
func (c *client) RemoveChargeLine(ctx context.Context, invoiceID, lineID string) error {
	for attempt := 0; attempt < chargeLineRemoveAttempts; attempt++ {
		err := c.removeLineOnce(ctx, invoiceID, lineID)
		if !errors.Is(err, errInvoiceChanged) {
			return err
		}
	}
	return fmt.Errorf("remove line %s from invoice %s: %w", lineID, invoiceID, errInvoiceChanged)
}

func (c *client) removeLineOnce(ctx context.Context, invoiceID, lineID string) error {
	inv, found, err := c.getInvoiceWithLines(ctx, invoiceID)
	if err != nil || !found {
		return err
	}
	lines := LineItems(inv)
	keep := make([]om.InvoiceLine, 0, len(lines))
	onInvoice := false
	for _, line := range lines {
		if line.Id == lineID {
			onInvoice = true
			continue
		}
		keep = append(keep, line)
	}
	if !onInvoice {
		return nil
	}
	if inv.Status != om.InvoiceStatusGathering && inv.Status != om.InvoiceStatusDraft {
		return ErrLineInvoiced
	}

	body := invoiceReplaceWithout(inv, keep)
	again, found, err := c.getInvoiceWithLines(ctx, invoiceID)
	if err != nil {
		return err
	}
	if !found || !again.UpdatedAt.Equal(inv.UpdatedAt) {
		return errInvoiceChanged
	}
	resp, err := c.api.UpdateInvoiceWithResponse(ctx, invoiceID, body)
	if err != nil {
		return classify(nil, nil, err)
	}
	if resp.StatusCode() == http.StatusConflict {
		return errInvoiceChanged
	}
	return classify(resp.HTTPResponse, resp.Body, nil)
}

// invoiceReplaceWithout is the UpdateInvoice body that keeps inv as it is,
// with only lines on it.
func invoiceReplaceWithout(inv om.Invoice, lines []om.InvoiceLine) om.InvoiceReplaceUpdate {
	body := om.InvoiceReplaceUpdate{
		Description: inv.Description,
		Metadata:    inv.Metadata,
		Customer: om.BillingPartyReplaceUpdate{
			Addresses: inv.Customer.Addresses,
			Key:       inv.Customer.Key,
			Name:      inv.Customer.Name,
			TaxId:     inv.Customer.TaxId,
		},
		Supplier: om.BillingPartyReplaceUpdate{
			Addresses: inv.Supplier.Addresses,
			Key:       inv.Supplier.Key,
			Name:      inv.Supplier.Name,
			TaxId:     inv.Supplier.TaxId,
		},
		Lines: make([]om.InvoiceLineReplaceUpdate, 0, len(lines)),
	}
	if wf := inv.Workflow.Workflow; wf.Invoicing != nil {
		body.Workflow.Workflow.Invoicing = om.InvoiceWorkflowInvoicingSettingsReplaceUpdate{
			AutoAdvance:                  wf.Invoicing.AutoAdvance,
			DefaultTaxConfig:             wf.Invoicing.DefaultTaxConfig,
			DraftPeriod:                  wf.Invoicing.DraftPeriod,
			DueAfter:                     wf.Invoicing.DueAfter,
			SubscriptionEndProrationMode: wf.Invoicing.SubscriptionEndProrationMode,
		}
	}
	if wf := inv.Workflow.Workflow; wf.Payment != nil {
		body.Workflow.Workflow.Payment = *wf.Payment
	}
	for _, line := range lines {
		id := line.Id
		body.Lines = append(body.Lines, om.InvoiceLineReplaceUpdate{
			Id:          &id,
			Name:        line.Name,
			Description: line.Description,
			InvoiceAt:   line.InvoiceAt,
			Period:      line.Period,
			Metadata:    line.Metadata,
			RateCard:    line.RateCard,
			FeatureKey:  line.FeatureKey,
		})
	}
	return body
}

// getInvoiceWithLines reads one invoice with its lines. found is false
// when OpenMeter has no such invoice.
func (c *client) getInvoiceWithLines(ctx context.Context, invoiceID string) (inv om.Invoice, found bool, err error) {
	resp, err := c.api.GetInvoiceWithResponse(ctx, invoiceID, &om.GetInvoiceParams{
		Expand: &[]om.InvoiceExpand{om.InvoiceExpandLines},
	})
	if err != nil {
		return om.Invoice{}, false, classify(nil, nil, err)
	}
	if resp.StatusCode() == http.StatusNotFound {
		return om.Invoice{}, false, nil
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return om.Invoice{}, false, err
	}
	if resp.JSON200 == nil {
		return om.Invoice{}, false, &PermanentError{Err: fmt.Errorf("get invoice %s: empty response body", invoiceID)}
	}
	return *resp.JSON200, true, nil
}

// InvoiceChargeLineNow invoices the one line now, on an invoice of its own,
// leaving every other pending line where it is. With progressive billing
// on, the filter also keeps the customer's usage so far off it.
func (c *client) InvoiceChargeLineNow(ctx context.Context, customerID CustomerID, lineID string) error {
	resp, err := c.api.InvoicePendingLinesActionWithResponse(ctx, om.InvoicePendingLinesActionJSONRequestBody{
		CustomerId: string(customerID),
		Filters:    &om.InvoicePendingLinesActionFiltersInput{LineIds: &[]string{lineID}},
	})
	if err != nil {
		return classify(nil, nil, err)
	}
	return classify(resp.HTTPResponse, resp.Body, nil)
}

// CurrentBillingPeriodEnd is when the customer's live subscription's
// current billing period ends, or ok false when it has none.
func (c *client) CurrentBillingPeriodEnd(ctx context.Context, customerKey CustomerKey) (time.Time, bool, error) {
	subs, err := c.ListSubscriptions(ctx, customerKey)
	if err != nil {
		return time.Time{}, false, err
	}
	for _, sub := range subs {
		if sub.Status != om.SubscriptionStatusActive {
			continue
		}
		period, err := c.currentBillingPeriod(ctx, sub.Id)
		if err != nil {
			return time.Time{}, false, err
		}
		if period != nil {
			return period.To, true, nil
		}
	}
	return time.Time{}, false, nil
}
