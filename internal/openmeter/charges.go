// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"
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

// Pending reports whether the line's invoice is still to be issued: it's
// the gathering invoice, or a draft OpenMeter is holding.
func (r ChargeLineRef) Pending() bool {
	return r.InvoiceStatus == om.InvoiceStatusGathering || r.InvoiceStatus == om.InvoiceStatusDraft
}

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

// InvoiceChargeLinesNow invoices the lines now, together on an invoice of
// their own, leaving every other pending line where it is. With
// progressive billing on, the filter also keeps the customer's usage so
// far off it.
func (c *client) InvoiceChargeLinesNow(ctx context.Context, customerID CustomerID, lineIDs ...string) error {
	if len(lineIDs) == 0 {
		return &PermanentError{Err: errors.New("InvoiceChargeLinesNow needs at least one line")}
	}
	ids := append([]string(nil), lineIDs...)
	resp, err := c.api.InvoicePendingLinesActionWithResponse(ctx, om.InvoicePendingLinesActionJSONRequestBody{
		CustomerId: string(customerID),
		Filters:    &om.InvoicePendingLinesActionFiltersInput{LineIds: &ids},
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
