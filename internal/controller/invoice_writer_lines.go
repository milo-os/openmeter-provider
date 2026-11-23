// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
	"github.com/shopspring/decimal"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
)

// maxStripeInvoiceLines is the most lines a StripeInvoice takes.
const maxStripeInvoiceLines = 250

// invoiceWriterLines builds the StripeInvoice lines for inv from its own:
//
//   - A line from the account's contract or usage becomes one Stripe line
//     per detailed line OpenMeter worked out for it: one for a fee, one per
//     tier of usage. Its pricing is the ServicePricing its rate card's
//     description names (see offer_mapping.go), so the Stripe line is that
//     pricing's Product; its quantity, unit price and amount are
//     OpenMeter's, and its discount is the rate card's percentage.
//   - A charge staff added (its line carries datum_charge) becomes one
//     Stripe line from the Charge: its description, pricing (or the custom
//     charge pricing), quantity, unit price, tax, discount and note.
//
// A detailed line with nothing to bill is left off. When a line can't be
// built, held says why: the invoice waits until someone looks.
func (r *BillingAccountReconciler) invoiceWriterLines(ctx context.Context, inv om.Invoice) ([]stripev1alpha1.StripeInvoiceLine, string, error) {
	if inv.Lines == nil {
		return nil, "OpenMeter returned it without its lines", nil
	}
	var out []stripev1alpha1.StripeInvoiceLine
	for _, line := range *inv.Lines {
		if line.DeletedAt != nil || line.Status != om.InvoiceLineStatusValid {
			continue
		}
		if ref := lineMetadata(line.Metadata)[openmeter.ChargeMetadataKey]; ref != "" {
			l, held, err := r.chargeWriterLine(ctx, line, ref)
			if err != nil || held != "" {
				return nil, held, err
			}
			out = append(out, l)
			continue
		}
		lines, held := usageWriterLines(line)
		if held != "" {
			return nil, held, nil
		}
		out = append(out, lines...)
	}
	if len(out) > maxStripeInvoiceLines {
		return nil, fmt.Sprintf("it has %d lines to bill, more than a StripeInvoice takes (%d)", len(out), maxStripeInvoiceLines), nil
	}
	return out, "", nil
}

// usageWriterLines builds the Stripe lines for one of the account's
// contract or usage lines: one per detailed line with something to bill.
func usageWriterLines(line om.InvoiceLine) ([]stripev1alpha1.StripeInvoiceLine, string) {
	var details []om.InvoiceDetailedLine
	if line.Children != nil {
		for _, d := range *line.Children {
			if d.DeletedAt == nil && !isZero(d.Totals.Amount) {
				details = append(details, d)
			}
		}
	}
	if len(details) == 0 {
		if !isZero(line.Totals.Amount) {
			return nil, fmt.Sprintf("line %s (%s) has an amount but no detailed lines", line.Id, line.Name)
		}
		return nil, ""
	}
	pricing := pricingFromDescription(line.Description)
	if pricing == "" {
		return nil, fmt.Sprintf("line %s (%s) doesn't name the ServicePricing it's for", line.Id, line.Name)
	}
	out := make([]stripev1alpha1.StripeInvoiceLine, 0, len(details))
	for _, d := range details {
		amount, err := decimal.NewFromString(d.Totals.Amount)
		if err != nil || amount.IsNegative() {
			return nil, fmt.Sprintf("detailed line %s has amount %q, which can't be billed", d.Id, d.Totals.Amount)
		}
		quantity, unit := quantityAndUnit(d.Quantity, d.PerUnitAmount, amount)
		l := stripev1alpha1.StripeInvoiceLine{
			Key:               d.Id,
			Description:       truncate(d.Name, 500),
			ServicePricingRef: stripev1alpha1.ServicePricingRef{Namespace: billingv1alpha1.DefaultServicePricingNamespace, Name: pricing},
			Quantity:          quantity,
			UnitAmount:        unit,
			Amount:            amount.String(),
			Period:            inclusivePeriod(d.Period),
			Tax:               lineTax(d.TaxConfig, line.TaxConfig),
			Metadata: map[string]string{
				lineMetadataServicePricing: pricing,
				lineMetadataOpenMeterLine:  line.Id,
			},
		}
		percent, ok := discountPercent(d, line)
		if !ok {
			return nil, fmt.Sprintf("detailed line %s's discount isn't a single percentage Stripe can take", d.Id)
		}
		l.DiscountPercent = percent
		out = append(out, l)
	}
	return out, ""
}

// chargeWriterLine builds the Stripe line for a charge staff added, from
// the Charge its line names (namespace/name).
func (r *BillingAccountReconciler) chargeWriterLine(ctx context.Context, line om.InvoiceLine, ref string) (stripev1alpha1.StripeInvoiceLine, string, error) {
	ns, name, ok := strings.Cut(ref, "/")
	if !ok {
		return stripev1alpha1.StripeInvoiceLine{}, fmt.Sprintf("line %s names charge %q, which isn't namespace/name", line.Id, ref), nil
	}
	var charge billingv1alpha1.Charge
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &charge); err != nil {
		if apierrors.IsNotFound(err) {
			return stripev1alpha1.StripeInvoiceLine{}, fmt.Sprintf("line %s is for charge %s, which no longer exists", line.Id, ref), nil
		}
		return stripev1alpha1.StripeInvoiceLine{}, "", fmt.Errorf("get charge %s: %w", ref, err)
	}
	if uid := lineMetadata(line.Metadata)[openmeter.ChargeUIDMetadataKey]; uid != "" && uid != string(charge.UID) {
		return stripev1alpha1.StripeInvoiceLine{}, fmt.Sprintf("line %s is for an earlier charge called %s", line.Id, ref), nil
	}

	pricing := stripev1alpha1.ServicePricingRef{Namespace: billingv1alpha1.DefaultServicePricingNamespace}
	switch {
	case charge.Spec.ServicePricingRef != nil:
		pricing.Name = charge.Spec.ServicePricingRef.Name
		if charge.Spec.ServicePricingRef.Namespace != "" {
			pricing.Namespace = charge.Spec.ServicePricingRef.Namespace
		}
	case r.InvoiceWriter != nil && r.InvoiceWriter.CustomChargePricing != "":
		pricing.Name = r.InvoiceWriter.CustomChargePricing
	default:
		return stripev1alpha1.StripeInvoiceLine{}, fmt.Sprintf(
			"charge %s has no catalog item, and no custom charge pricing is configured (invoiceWriter.customChargePricing)", ref), nil
	}

	quantity, err1 := decimal.NewFromString(charge.Spec.Quantity)
	unit, err2 := decimal.NewFromString(charge.Spec.UnitAmount)
	if err1 != nil || err2 != nil {
		return stripev1alpha1.StripeInvoiceLine{}, fmt.Sprintf("charge %s's quantity or unit amount isn't a number", ref), nil
	}
	l := stripev1alpha1.StripeInvoiceLine{
		Key:               line.Id,
		Description:       truncate(charge.Spec.Description, 500),
		ServicePricingRef: pricing,
		Quantity:          quantity.String(),
		UnitAmount:        unit.String(),
		// Before the discount: OpenMeter's line holds the amount after it,
		// and the totals are compared after discounts.
		Amount:          quantity.Mul(unit).StringFixed(2),
		Period:          inclusivePeriod(line.Period),
		DiscountPercent: charge.Spec.DiscountPercent,
		Note:            charge.Spec.Note,
		Metadata: map[string]string{
			lineMetadataServicePricing: pricing.Name,
			lineMetadataOpenMeterLine:  line.Id,
			"charge":                   ref,
			"charge_uid":               string(charge.UID),
		},
	}
	if by := charge.Annotations[billingv1alpha1.ChargeAddedByAnnotation]; by != "" {
		l.Metadata["added_by"] = truncate(by, 500)
	}
	if tax := charge.Spec.Tax; tax != nil {
		l.Tax = &stripev1alpha1.StripeInvoiceLineTax{Code: tax.Code, Taxable: tax.Taxable}
		if tax.Taxable != nil && !*tax.Taxable {
			l.Tax.Code = ""
		}
	}
	return l, "", nil
}

// pricingFromDescription is the ServicePricing a rate card's description
// names: offer_mapping.go writes "ServicePricing <name>", then optionally
// its service and unit.
func pricingFromDescription(description *string) string {
	if description == nil {
		return ""
	}
	rest, ok := strings.CutPrefix(*description, "ServicePricing ")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, " ")
	return name
}

// quantityAndUnit is the Stripe line's quantity and unit price. OpenMeter's
// own are used when they multiply out to its amount, to the cent; a fee
// prorated or folded into one price (a contract's quantity is part of its
// flat price) becomes one of its amount.
func quantityAndUnit(quantity, perUnit *string, amount decimal.Decimal) (string, string) {
	if quantity != nil && perUnit != nil {
		q, err1 := decimal.NewFromString(*quantity)
		u, err2 := decimal.NewFromString(*perUnit)
		if err1 == nil && err2 == nil && q.IsPositive() && !u.IsNegative() && q.Mul(u).Round(2).Equal(amount) {
			return q.String(), u.String()
		}
	}
	return "1", amount.String()
}

// discountPercent is the percentage off a detailed line, as a
// StripeInvoiceLine takes it: at most two decimal places, up to 100. ok is
// false for a discount that can't be written that way.
//
// OpenMeter records a rate card's percentage discount on each detailed
// line it applies to, as an amount discount whose reason is
// ratecard_percentage; the rate card snapshot on the line may not carry
// it. A detailed line with no amount discounts falls back to the rate
// cards'. A maximum-spend discount, or two different percentages, can't be
// one Stripe coupon.
func discountPercent(d om.InvoiceDetailedLine, line om.InvoiceLine) (string, bool) {
	var found []decimal.Decimal
	if d.Discounts != nil && d.Discounts.Amount != nil {
		for _, a := range *d.Discounts.Amount {
			if a.DeletedAt != nil {
				continue
			}
			kind, err := a.Reason.Discriminator()
			if err != nil {
				return "", false
			}
			if kind != string(om.DiscountReasonRatecardPercentageTypeRatecardPercentage) {
				if isZero(a.Amount) {
					continue
				}
				return "", false
			}
			reason, err := a.Reason.AsDiscountReasonRatecardPercentage()
			if err != nil {
				return "", false
			}
			p, err := percentValue(reason.Percentage)
			if err != nil {
				return "", false
			}
			found = append(found, p)
		}
	}
	if len(found) == 0 {
		var discounts *om.BillingDiscounts
		switch {
		case d.RateCard != nil && d.RateCard.Discounts != nil:
			discounts = d.RateCard.Discounts
		case line.RateCard != nil && line.RateCard.Discounts != nil:
			discounts = line.RateCard.Discounts
		}
		if discounts != nil && discounts.Percentage != nil {
			p, err := percentValue(discounts.Percentage.Percentage)
			if err != nil {
				return "", false
			}
			found = append(found, p)
		}
	}
	if len(found) == 0 {
		return "", true
	}
	p := found[0]
	for _, other := range found[1:] {
		if !other.Equal(p) {
			return "", false
		}
	}
	if !p.IsPositive() {
		return "", true
	}
	if p.GreaterThan(decimal.NewFromInt(100)) || !p.Round(2).Equal(p) {
		return "", false
	}
	return p.String(), true
}

// percentValue is an OpenMeter percentage as a number: its String() adds
// a "%".
func percentValue(p om.Percentage) (decimal.Decimal, error) {
	return decimal.NewFromString(p.Decimal.String())
}

// lineTax is a Stripe line's tax code: the detailed line's, else its line's.
// Without one, the line is taxed as its Product is.
func lineTax(configs ...*om.TaxConfig) *stripev1alpha1.StripeInvoiceLineTax {
	for _, c := range configs {
		if c != nil && c.Stripe != nil && c.Stripe.Code != "" {
			return &stripev1alpha1.StripeInvoiceLineTax{Code: c.Stripe.Code}
		}
	}
	return nil
}

// inclusivePeriod turns OpenMeter's half-open period into a Stripe line's,
// which includes both ends: the end is the last whole second before
// OpenMeter's, as invoicePeriod does for an Invoice.
func inclusivePeriod(p om.Period) stripev1alpha1.StripeInvoiceLinePeriod {
	start := p.From.Truncate(time.Second)
	end := start
	if last := p.To.Add(-time.Nanosecond).Truncate(time.Second); last.After(start) {
		end = last
	}
	return stripev1alpha1.StripeInvoiceLinePeriod{Start: metav1.NewTime(start.UTC()), End: metav1.NewTime(end.UTC())}
}

func lineMetadata(m *om.Metadata) map[string]string {
	if m == nil {
		return nil
	}
	return *m
}

func isZero(amount string) bool {
	d, err := decimal.NewFromString(amount)
	return err == nil && d.IsZero()
}

// truncate keeps the first n characters of s.
func truncate(s string, n int) string {
	if runes := []rune(s); len(runes) > n {
		return string(runes[:n])
	}
	return s
}
