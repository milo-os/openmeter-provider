// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	om "github.com/openmeterio/openmeter/api/client/go"
	"github.com/openmeterio/openmeter/pkg/models"
)

// Fixed charges are a customer's OneTime and Recurring ServicePricings:
// flat fees. These calls back InvoiceRequest previews. OpenMeter invoices
// in-advance flat fees on its own as soon as they're due (its subscription
// sync invoices any pending line billable now), so a contract's setup fee
// and first month are invoiced the moment its subscription starts, and
// only later periods are ever left pending.

// UpcomingFixedCharges returns the flat-fee lines on the customer's
// gathering invoice, priced by OpenMeter with live data: their totals
// include discounts and proration. They're the charges the customer's next
// invoices will carry. customerID is OpenMeter's internal customer id, as
// for ListInvoices.
func (c *client) UpcomingFixedCharges(ctx context.Context, customerID CustomerID) ([]om.InvoiceLine, error) {
	invoices, err := c.listInvoicesWithLines(ctx, customerID, om.InvoiceStatusGathering)
	if err != nil {
		return nil, err
	}
	var lines []om.InvoiceLine
	for _, inv := range invoices {
		for _, line := range LineItems(inv) {
			if isFlatFee(line) {
				lines = append(lines, line)
			}
		}
	}
	return lines, nil
}

// DraftInvoice returns the customer's newest draft invoice with its lines,
// or nil when there is none. With the default billing profile a draft only
// lasts until OpenMeter's collection wait ends and it's issued.
func (c *client) DraftInvoice(ctx context.Context, customerID CustomerID) (*om.Invoice, error) {
	invoices, err := c.listInvoicesWithLines(ctx, customerID, om.InvoiceStatusDraft)
	if err != nil {
		return nil, err
	}
	var newest *om.Invoice
	for i := range invoices {
		if invoices[i].Status != om.InvoiceStatusDraft || IsInvoiceDeleted(invoices[i]) {
			continue
		}
		if newest == nil || invoices[i].CreatedAt.After(newest.CreatedAt) {
			newest = &invoices[i]
		}
	}
	return newest, nil
}

// listInvoicesWithLines lists the customer's invoices in status, with
// their lines. A customer has at most a handful in any one status, so one
// page is enough.
func (c *client) listInvoicesWithLines(ctx context.Context, customerID CustomerID, status om.InvoiceStatus) ([]om.Invoice, error) {
	if customerID == "" {
		return nil, &PermanentError{Err: errors.New("customerID is required")}
	}
	pageSize := om.PaginationPageSize(invoiceListPageSize)
	resp, err := c.api.ListInvoicesWithResponse(ctx, &om.ListInvoicesParams{
		Customers: &om.InvoiceListParamsCustomers{string(customerID)},
		Statuses:  &om.InvoiceListParamsStatuses{status},
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
		return nil, &PermanentError{Err: fmt.Errorf("list %s invoices for %q: empty response body", status, customerID)}
	}
	var out []om.Invoice
	for _, inv := range resp.JSON200.Items {
		if inv.Status == status && invoiceBelongsToCustomer(inv, customerID) {
			out = append(out, inv)
		}
	}
	return out, nil
}

// LineItems returns an invoice's lines that aren't deleted.
func LineItems(inv om.Invoice) []om.InvoiceLine {
	if inv.Lines == nil {
		return nil
	}
	var out []om.InvoiceLine
	for _, line := range *inv.Lines {
		if line.DeletedAt == nil {
			out = append(out, line)
		}
	}
	return out
}

// isFlatFee reports whether line is priced flat: a OneTime or Recurring
// ServicePricing rather than usage.
func isFlatFee(line om.InvoiceLine) bool {
	price := line.Price
	if line.RateCard != nil && line.RateCard.Price != nil {
		price = line.RateCard.Price
	}
	if price == nil {
		return false
	}
	kind, err := price.Discriminator()
	return err == nil && kind == string(om.FlatPriceWithPaymentTermTypeFlat)
}

// PlannedCharge is a flat fee a customer will be charged, priced from its
// ServicePricing rather than from a subscription.
type PlannedCharge struct {
	// Name is the charge's name on the invoice.
	Name string
	// Period is the service period it covers; From equals To for a
	// one-time charge.
	Period om.Period
	// Amount is the flat price before discounts, as a decimal string.
	Amount string
	// DiscountPercentage is the percentage discount, 0 for none.
	DiscountPercentage int32
}

// PreviewInvoice works out the invoice charges would make for the
// customer, creating nothing, with OpenMeter's invoice simulation: the
// same pricing and discount calculation as a real invoice.
func (c *client) PreviewInvoice(ctx context.Context, customerID CustomerID, currency string, charges []PlannedCharge) (om.Invoice, error) {
	if len(charges) == 0 {
		return om.Invoice{}, &PermanentError{Err: errors.New("no charges to preview")}
	}
	// OpenMeter rates a flat fee only once it's due: one invoiced later than
	// the simulation runs prices at zero. When a line is invoiced doesn't
	// change its amount, so every line is invoiced now (a minute back, at
	// OpenMeter's one-minute resolution, so clock skew can't push it into
	// the future), and the period still says what it covers.
	invoiceAt := time.Now().Add(-time.Minute).Truncate(time.Minute)
	lines := make([]om.InvoiceSimulationLine, 0, len(charges))
	for _, ch := range charges {
		var price om.RateCardUsageBasedPrice
		if err := price.FromFlatPriceWithPaymentTerm(om.FlatPriceWithPaymentTerm{
			Type:        om.FlatPriceWithPaymentTermTypeFlat,
			Amount:      ch.Amount,
			PaymentTerm: ptr(om.PricePaymentTermInAdvance),
		}); err != nil {
			return om.Invoice{}, &PermanentError{Err: fmt.Errorf("charge %q: encode price: %w", ch.Name, err)}
		}
		rateCard := &om.InvoiceUsageBasedRateCard{Price: &price}
		if ch.DiscountPercentage > 0 {
			rateCard.Discounts = &om.BillingDiscounts{
				Percentage: &om.BillingDiscountPercentage{Percentage: models.NewPercentage(int(ch.DiscountPercentage))},
			}
		}
		// A discount refers to its line by id, so every line has one.
		id := ulid.Make().String()
		lines = append(lines, om.InvoiceSimulationLine{
			Id:        &id,
			Name:      ch.Name,
			InvoiceAt: invoiceAt,
			Period:    ch.Period,
			Quantity:  "1",
			RateCard:  rateCard,
		})
	}
	resp, err := c.api.SimulateInvoiceWithResponse(ctx, string(customerID), om.SimulateInvoiceJSONRequestBody{
		Currency: currency,
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

// MonthlyPeriod is the first monthly service period starting at start.
func MonthlyPeriod(start time.Time) om.Period {
	return om.Period{From: start, To: start.AddDate(0, 1, 0)}
}

// AmountBeforeTax is what totals charge before any tax added on top: the
// total less exclusive taxes. OpenMeter adds no tax itself when Stripe
// calculates it, so this is usually the total.
func AmountBeforeTax(totals om.InvoiceTotals) (string, error) {
	return subtractDecimal(totals.Total, totals.TaxesExclusiveTotal)
}

// AddDecimals sums decimal strings, with as many decimal places as the most
// precise of them. An empty string counts as zero.
func AddDecimals(values ...string) (string, error) {
	sum, scale := new(big.Rat), 0
	for _, v := range values {
		r, s, err := parseDecimal(v)
		if err != nil {
			return "", err
		}
		sum.Add(sum, r)
		scale = max(scale, s)
	}
	return sum.FloatString(scale), nil
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
