// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
)

// previewChargeDraft works out the invoice a ChargeDraft's lines would
// make on their own, through Stripe as a contract's first invoice is, with
// a draft in the Stripe dashboard when the request asks for one.
func (r *InvoiceRequestReconciler) previewChargeDraft(
	ctx context.Context,
	ir *billingv1alpha1.InvoiceRequest,
	account *billingv1alpha1.BillingAccount,
) (ctrl.Result, error) {
	name := ir.Spec.ChargeDraftRef.Name
	var draft billingv1alpha1.ChargeDraft
	if err := r.Get(ctx, types.NamespacedName{Namespace: ir.Namespace, Name: name}, &draft); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get charge draft: %w", err)
		}
		return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed,
			fmt.Sprintf("ChargeDraft %q not found", name))
	}
	if draft.Spec.BillingAccountRef.Name != account.Name {
		return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed,
			fmt.Sprintf("ChargeDraft %q is for billing account %q", name, draft.Spec.BillingAccountRef.Name))
	}
	charges, total, err := r.chargeDraftCharges(ctx, &draft, account, r.now())
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed, err.Error())
	}
	lines := "line"
	if len(charges) != 1 {
		lines = "lines"
	}
	return r.previewFirstInvoiceResult(ctx, ir, account, charges, total,
		fmt.Sprintf("The invoice the draft's %d %s would make on their own", len(charges), lines))
}

// chargeDraftCharges prices a draft's lines as billing would fill them in
// if the draft were sent now: each catalog item's description, price and
// tax, and the account's current billing period, where the line leaves
// them out. A line without a catalog item is priced under the custom
// charge pricing, as the invoice writer bills it.
func (r *InvoiceRequestReconciler) chargeDraftCharges(
	ctx context.Context,
	draft *billingv1alpha1.ChargeDraft,
	account *billingv1alpha1.BillingAccount,
	now time.Time,
) ([]firstCharge, string, error) {
	var charges []firstCharge
	total := decimal.Zero
	for i, line := range draft.Spec.Lines {
		c, amount, err := r.chargeDraftCharge(ctx, line, account, now)
		if err != nil {
			return nil, "", fmt.Errorf("line %d: %w", i+1, err)
		}
		charges = append(charges, c)
		total = total.Add(amount)
	}
	return charges, total.StringFixed(2), nil
}

func (r *InvoiceRequestReconciler) chargeDraftCharge(
	ctx context.Context,
	line billingv1alpha1.ChargeDraftLine,
	account *billingv1alpha1.BillingAccount,
	now time.Time,
) (firstCharge, decimal.Decimal, error) {
	c := firstCharge{
		name: line.Description, quantity: line.Quantity, unitAmount: line.UnitAmount,
		discount: line.DiscountPercent, note: line.Note,
	}
	tax := line.Tax
	if ref := line.ServicePricingRef; ref != nil {
		ns := ref.Namespace
		if ns == "" {
			ns = billingv1alpha1.DefaultServicePricingNamespace
		}
		var pricing billingv1alpha1.ServicePricing
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &pricing); err != nil {
			if apierrors.IsNotFound(err) {
				return c, decimal.Zero, fmt.Errorf("ServicePricing %q not found", ref.Name)
			}
			return c, decimal.Zero, fmt.Errorf("get ServicePricing %q: %w", ref.Name, err)
		}
		c.pricing = pricing.Name
		if c.name == "" {
			c.name = pricing.Spec.DisplayName
		}
		if c.name == "" {
			c.name = pricing.Name
		}
		if c.unitAmount == "" {
			c.unitAmount = pricing.Spec.Amount
		}
		if tax == nil {
			tax = pricing.Spec.Tax
		}
	} else {
		if r.CustomChargePricing == "" {
			return c, decimal.Zero, fmt.Errorf("it has no catalog item, and no custom charge pricing is configured (invoiceWriter.customChargePricing)")
		}
		c.pricing = r.CustomChargePricing
	}
	if tax != nil && (tax.Code != "" || tax.Taxable != nil) {
		c.tax = &stripev1alpha1.StripeInvoiceLineTax{Code: tax.Code, Taxable: tax.Taxable}
		if tax.Taxable != nil && !*tax.Taxable {
			c.tax.Code = ""
		}
	}

	quantity, err1 := decimal.NewFromString(c.quantity)
	unit, err2 := decimal.NewFromString(c.unitAmount)
	if err1 != nil || err2 != nil {
		return c, decimal.Zero, fmt.Errorf("%q: its quantity or price isn't a number", c.name)
	}
	before := quantity.Mul(unit).Round(2)
	amount := before
	if c.discount != "" {
		percent, err := decimal.NewFromString(c.discount)
		if err != nil {
			return c, decimal.Zero, fmt.Errorf("%q: its discount isn't a number", c.name)
		}
		amount = before.Sub(before.Mul(percent).Div(decimal.NewFromInt(100)).Round(2))
	}
	c.quantity, c.unitAmount = quantity.String(), unit.String()
	c.amountBeforeDiscount = before.StringFixed(2)

	period := chargeDraftDefaultPeriod(account, now)
	if line.ServicePeriod != nil {
		period = *line.ServicePeriod
	}
	supply, ok := supplyPeriod(period)
	if !ok {
		return c, decimal.Zero, fmt.Errorf("%q: its service period isn't a pair of dates", c.name)
	}
	c.start, c.end = supply.Start.Time, supply.End.Time
	c.line = billingv1alpha1.InvoiceRequestLine{
		Description: c.name,
		Period:      &billingv1alpha1.InvoicePeriod{Start: supply.Start, End: supply.End},
		Amount:      amount.StringFixed(2),
		Quantity:    c.quantity,
		UnitAmount:  c.unitAmount,
	}
	return c, amount, nil
}

// chargeDraftDefaultPeriod is the service period billing gives a charge
// that leaves it out: the account's current billing period on now's UTC
// day, from the most recent billing day on or before it to the day before
// the next.
func chargeDraftDefaultPeriod(account *billingv1alpha1.BillingAccount, now time.Time) billingv1alpha1.ServicePeriod {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	next := nextBillingDay(account, today)
	start := nextBillingDay(account, today.AddDate(0, -1, -1))
	for following := nextBillingDay(account, start); !following.After(today); following = nextBillingDay(account, start) {
		start = following
	}
	return billingv1alpha1.ServicePeriod{
		Start: start.Format(time.DateOnly),
		End:   next.AddDate(0, 0, -1).Format(time.DateOnly),
	}
}
