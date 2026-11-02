// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// ErrNothingToInvoice is returned by InvoiceFixedCharges when OpenMeter
// finds none of the lines billable.
var ErrNothingToInvoice = errors.New("openmeter: nothing to invoice")

// FixedCharge is a flat-fee line waiting on a customer's gathering invoice:
// a OneTime or Recurring ServicePricing of the customer's plan, for one
// service period.
type FixedCharge struct {
	// LineID is the gathering line's id.
	LineID string
	// Name is the line's name, as it appears on the invoice.
	Name string
	// Description is the line's description, if any.
	Description *string
	// Currency is the line's ISO 4217 currency.
	Currency string
	// Period is the service period the line covers.
	Period om.Period
	// InvoiceAt is when OpenMeter would invoice the line on its own.
	InvoiceAt time.Time
	// Price is the line's flat price. OpenMeter has already prorated it
	// for a partial period.
	Price om.RateCardUsageBasedPrice
	// Discounts are the discounts of the subscription item the line comes
	// from. Gathering lines don't carry them, so they're read from the
	// subscription.
	Discounts *om.Discounts
}

// PendingFixedCharges returns the customer's flat-fee lines that are
// billable as of asOf: on a gathering invoice, not deleted, and due to be
// invoiced at or before asOf. Usage lines are left out, so invoicing these
// never cuts an early usage invoice. customerID is OpenMeter's internal
// customer id, as for ListInvoices.
func (c *client) PendingFixedCharges(ctx context.Context, customerID CustomerID, asOf time.Time) ([]FixedCharge, error) {
	if customerID == "" {
		return nil, &PermanentError{Err: errors.New("customerID is required")}
	}
	pageSize := om.PaginationPageSize(invoiceListPageSize)
	resp, err := c.api.ListInvoicesWithResponse(ctx, &om.ListInvoicesParams{
		Customers: &om.InvoiceListParamsCustomers{string(customerID)},
		Statuses:  &om.InvoiceListParamsStatuses{om.InvoiceStatusGathering},
		Expand:    &om.InvoiceListParamsExpand{om.InvoiceExpandLines},
		PageSize:  &pageSize,
	})
	if err != nil {
		return nil, classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, &PermanentError{Err: fmt.Errorf("list gathering invoices for %q: empty response body", customerID)}
	}

	var charges []FixedCharge
	subscriptions := map[string]*om.SubscriptionExpanded{}
	for _, inv := range resp.JSON200.Items {
		if inv.Status != om.InvoiceStatusGathering || inv.Lines == nil || !invoiceBelongsToCustomer(inv, customerID) {
			continue
		}
		for _, line := range *inv.Lines {
			price := line.Price
			if line.RateCard != nil && line.RateCard.Price != nil {
				price = line.RateCard.Price
			}
			if line.DeletedAt != nil || line.InvoiceAt.After(asOf) || price == nil {
				continue
			}
			if kind, err := price.Discriminator(); err != nil || kind != string(om.FlatPriceWithPaymentTermTypeFlat) {
				continue
			}
			charge := FixedCharge{
				LineID:      line.Id,
				Name:        line.Name,
				Description: line.Description,
				Currency:    line.Currency,
				Period:      line.Period,
				InvoiceAt:   line.InvoiceAt,
				Price:       *price,
			}
			if ref := line.Subscription; ref != nil && ref.Subscription.Id != "" {
				sub, ok := subscriptions[ref.Subscription.Id]
				if !ok {
					if sub, err = c.getSubscription(ctx, ref.Subscription.Id, line.Period.From); err != nil {
						return nil, err
					}
					subscriptions[ref.Subscription.Id] = sub
				}
				charge.Discounts = subscriptionItemDiscounts(sub, ref.Item.Id)
			}
			charges = append(charges, charge)
		}
	}
	return charges, nil
}

// getSubscription reads a subscription with its phases and items as of at.
func (c *client) getSubscription(ctx context.Context, id string, at time.Time) (*om.SubscriptionExpanded, error) {
	resp, err := c.api.GetSubscriptionWithResponse(ctx, id, &om.GetSubscriptionParams{At: &at})
	if err != nil {
		return nil, classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, &PermanentError{Err: fmt.Errorf("get subscription %q: empty response body", id)}
	}
	return resp.JSON200, nil
}

// subscriptionItemDiscounts returns the discounts of the item itemID in
// sub, or nil when it has none or isn't found.
func subscriptionItemDiscounts(sub *om.SubscriptionExpanded, itemID string) *om.Discounts {
	if sub == nil || itemID == "" {
		return nil
	}
	for _, phase := range sub.Phases {
		for _, item := range phase.Items {
			if item.Id == itemID {
				return item.Discounts
			}
		}
		for _, timeline := range phase.ItemTimelines {
			for _, item := range timeline {
				if item.Id == itemID {
					return item.Discounts
				}
			}
		}
	}
	return nil
}

// PreviewInvoice works out the invoice OpenMeter would cut from charges,
// without creating anything: it runs them through OpenMeter's invoice
// simulation with their prices and discounts. charges must share one
// currency.
func (c *client) PreviewInvoice(ctx context.Context, customerID CustomerID, charges []FixedCharge) (om.Invoice, error) {
	if len(charges) == 0 {
		return om.Invoice{}, &PermanentError{Err: errors.New("no charges to preview")}
	}
	lines := make([]om.InvoiceSimulationLine, 0, len(charges))
	for _, ch := range charges {
		if ch.Currency != charges[0].Currency {
			return om.Invoice{}, &PermanentError{Err: fmt.Errorf("charges are in %s and %s; an invoice has one currency", charges[0].Currency, ch.Currency)}
		}
		id, price := ch.LineID, ch.Price
		lines = append(lines, om.InvoiceSimulationLine{
			Id:          &id,
			Name:        ch.Name,
			Description: ch.Description,
			InvoiceAt:   ch.InvoiceAt,
			Period:      ch.Period,
			Quantity:    "1",
			RateCard: &om.InvoiceUsageBasedRateCard{
				Price:     &price,
				Discounts: billingDiscounts(ch.Discounts),
			},
		})
	}
	resp, err := c.api.SimulateInvoiceWithResponse(ctx, string(customerID), om.SimulateInvoiceJSONRequestBody{
		Currency: charges[0].Currency,
		Lines:    lines,
	})
	if err != nil {
		return om.Invoice{}, classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return om.Invoice{}, err
	}
	if resp.JSON200 == nil {
		return om.Invoice{}, &PermanentError{Err: fmt.Errorf("simulate invoice for %q: empty response body", customerID)}
	}
	return *resp.JSON200, nil
}

// billingDiscounts converts a subscription item's discounts to the form an
// invoice line takes.
func billingDiscounts(d *om.Discounts) *om.BillingDiscounts {
	if d == nil || (d.Percentage == nil && d.Usage == nil) {
		return nil
	}
	out := &om.BillingDiscounts{}
	if d.Percentage != nil {
		out.Percentage = &om.BillingDiscountPercentage{Percentage: d.Percentage.Percentage}
	}
	if d.Usage != nil {
		out.Usage = &om.BillingDiscountUsage{Quantity: d.Usage.Quantity}
	}
	return out
}

// InvoiceFixedCharges invoices the lines lineIDs now, as of asOf, and
// returns the invoices OpenMeter created (usually one). Only those lines
// are invoiced: usage keeps accruing to the normal cycle. Returns
// ErrNothingToInvoice when none of them is billable, for example because
// they were invoiced already.
func (c *client) InvoiceFixedCharges(ctx context.Context, customerID CustomerID, lineIDs []string, asOf time.Time) ([]om.Invoice, error) {
	if len(lineIDs) == 0 {
		return nil, ErrNothingToInvoice
	}
	ids := append([]string(nil), lineIDs...)
	resp, err := c.api.InvoicePendingLinesActionWithResponse(ctx, om.InvoicePendingLinesActionJSONRequestBody{
		CustomerId: string(customerID),
		AsOf:       &asOf,
		Filters:    &om.InvoicePendingLinesActionFiltersInput{LineIds: &ids},
	})
	if err != nil {
		return nil, classify(nil, nil, err)
	}
	if resp.StatusCode() == http.StatusBadRequest && isNoLinesToInvoice(resp.Body) {
		return nil, ErrNothingToInvoice
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return nil, err
	}
	if resp.JSON201 == nil || len(*resp.JSON201) == 0 {
		return nil, ErrNothingToInvoice
	}
	return *resp.JSON201, nil
}

// GetInvoiceWithLines reads an invoice with its lines.
func (c *client) GetInvoiceWithLines(ctx context.Context, id string) (om.Invoice, error) {
	resp, err := c.api.GetInvoiceWithResponse(ctx, id, &om.GetInvoiceParams{
		Expand: &[]om.InvoiceExpand{om.InvoiceExpandLines},
	})
	if err != nil {
		return om.Invoice{}, classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return om.Invoice{}, err
	}
	if resp.JSON200 == nil {
		return om.Invoice{}, &PermanentError{Err: fmt.Errorf("get invoice %q: empty response body", id)}
	}
	return *resp.JSON200, nil
}

// AmountBeforeTax is what totals charge before any tax added on top: the
// total less exclusive taxes. OpenMeter adds no tax itself when Stripe
// calculates it, so this is usually the total.
func AmountBeforeTax(totals om.InvoiceTotals) (string, error) {
	return subtractDecimal(totals.Total, totals.TaxesExclusiveTotal)
}

// subtractDecimal returns a-b for decimal strings, with as many decimal
// places as the more precise of the two. An empty string counts as zero.
func subtractDecimal(a, b string) (string, error) {
	ra, scaleA, err := parseDecimal(a)
	if err != nil {
		return "", err
	}
	rb, scaleB, err := parseDecimal(b)
	if err != nil {
		return "", err
	}
	return new(big.Rat).Sub(ra, rb).FloatString(max(scaleA, scaleB)), nil
}

func parseDecimal(s string) (*big.Rat, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return new(big.Rat), 0, nil
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, 0, fmt.Errorf("invalid decimal %q", s)
	}
	scale := 0
	if i := strings.IndexByte(s, '.'); i >= 0 {
		scale = len(s) - i - 1
	}
	return r, scale, nil
}
