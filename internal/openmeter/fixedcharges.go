// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// Fixed charges are a customer's OneTime and Recurring ServicePricings:
// flat fees. These helpers back InvoiceRequest previews. OpenMeter invoices
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

// DiscountedAmount is a flat fee after a percentage discount, rounded to
// the cent as OpenMeter rounds USD amounts (half away from zero). Flat fees
// need no rating beyond that, so a contract's first invoice can be worked
// out without OpenMeter: its invoice simulation, the natural tool, rejects
// every line in this version ("invalid line engine type").
func DiscountedAmount(amount string, percentage int32) (string, error) {
	r, _, err := parseDecimal(amount)
	if err != nil {
		return "", err
	}
	if percentage < 0 || percentage > 100 {
		return "", fmt.Errorf("discount percentage %d must be between 0 and 100", percentage)
	}
	r.Mul(r, big.NewRat(int64(100-percentage), 100))
	// Round to cents, half away from zero.
	cents := new(big.Rat).Mul(r, big.NewRat(100, 1))
	n, d := cents.Num(), cents.Denom()
	q, m := new(big.Int).QuoRem(n, d, new(big.Int))
	if new(big.Int).Mul(new(big.Int).Abs(m), big.NewInt(2)).Cmp(d) >= 0 {
		if n.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return new(big.Rat).SetFrac(q, big.NewInt(100)).FloatString(2), nil
}

// MultiplyAmount returns a fee's amount times its quantity, with as many
// decimal places as amount: the amount of a fee an Offer charges more than
// once, such as three servers at the same monthly price.
func MultiplyAmount(amount string, quantity int32) (string, error) {
	if quantity < 1 {
		return "", fmt.Errorf("quantity %d must be at least 1", quantity)
	}
	r, scale, err := parseDecimal(amount)
	if err != nil {
		return "", err
	}
	r.Mul(r, big.NewRat(int64(quantity), 1))
	return r.FloatString(scale), nil
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

// IsZeroDecimal reports whether a decimal string is zero. An empty string
// counts as zero.
func IsZeroDecimal(value string) (bool, error) {
	r, _, err := parseDecimal(value)
	if err != nil {
		return false, err
	}
	return r.Sign() == 0, nil
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
