// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
)

const (
	// stripePreviewWait is how long a preview waits for Stripe before it
	// falls back to the provider's own arithmetic, without tax.
	stripePreviewWait = time.Minute
	// stripePreviewRequeue is how often a preview checks on Stripe's.
	stripePreviewRequeue = 5 * time.Second
	// stripePreviewCurrency is the only currency Stripe previews are worked
	// out in. Accounts billed in any other currency get the provider's own
	// preview.
	stripePreviewCurrency = "USD"
)

// stripePreview is what came of asking Stripe for a preview.
type stripePreview struct {
	// done is false while Stripe is still working it out.
	done bool
	// invoice is Stripe's preview, when it worked one out.
	invoice *stripev1alpha1.StripeInvoice
	// fallback says why the preview is the provider's own, without tax.
	fallback string
}

// previewThroughStripe asks stripe-provider to work out the first invoice
// with a StripeInvoice, owned by the request, and reports what came of it.
// For an account not billed in stripePreviewCurrency, without a Stripe
// customer for the account, or when Stripe can't answer within
// stripePreviewWait, the request falls back to the provider's own
// arithmetic.
func (r *InvoiceRequestReconciler) previewThroughStripe(
	ctx context.Context,
	ir *billingv1alpha1.InvoiceRequest,
	account *billingv1alpha1.BillingAccount,
	charges []firstCharge,
) (stripePreview, error) {
	if !r.StripeInvoices {
		return stripePreview{done: true, fallback: "stripe-provider isn't installed"}, nil
	}
	if account.Spec.CurrencyCode != stripePreviewCurrency {
		return stripePreview{done: true, fallback: fmt.Sprintf(
			"Stripe previews are only in %s and the account is billed in %s", stripePreviewCurrency, account.Spec.CurrencyCode)}, nil
	}
	var customer stripev1alpha1.StripeCustomer
	if err := r.Get(ctx, types.NamespacedName{Namespace: account.Namespace, Name: account.Name}, &customer); err != nil {
		if !apierrors.IsNotFound(err) {
			return stripePreview{}, fmt.Errorf("get Stripe customer: %w", err)
		}
		return stripePreview{done: true, fallback: "the account has no Stripe customer yet"}, nil
	}
	if customer.Status.StripeCustomerID == "" {
		return stripePreview{done: true, fallback: "the account's Stripe customer isn't ready"}, nil
	}

	var invoice stripev1alpha1.StripeInvoice
	key := types.NamespacedName{Namespace: ir.Namespace, Name: stripeInvoiceName(ir)}
	if err := r.Get(ctx, key, &invoice); err != nil {
		if !apierrors.IsNotFound(err) {
			return stripePreview{}, fmt.Errorf("get StripeInvoice: %w", err)
		}
		desired, err := r.stripeInvoiceFor(ir, account, charges)
		if err != nil {
			return stripePreview{}, err
		}
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return stripePreview{}, fmt.Errorf("create StripeInvoice: %w", err)
		}
		return stripePreview{}, nil
	}

	ready := apimeta.FindStatusCondition(invoice.Status.Conditions, stripev1alpha1.StripeInvoiceConditionReady)
	switch {
	case ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == invoice.Generation:
		return stripePreview{done: true, invoice: &invoice}, nil
	case ready != nil && ready.Reason == "StripeError":
		return stripePreview{done: true, fallback: "Stripe couldn't work it out: " + ready.Message}, nil
	case r.now().Sub(ir.CreationTimestamp.Time) >= stripePreviewWait:
		why := "Stripe didn't answer in time"
		if ready != nil && ready.Message != "" {
			why = ready.Message
		}
		return stripePreview{done: true, fallback: why}, nil
	}
	return stripePreview{}, nil
}

// stripeInvoiceFor is the StripeInvoice that previews the charges.
func (r *InvoiceRequestReconciler) stripeInvoiceFor(
	ir *billingv1alpha1.InvoiceRequest,
	account *billingv1alpha1.BillingAccount,
	charges []firstCharge,
) (*stripev1alpha1.StripeInvoice, error) {
	purpose := stripev1alpha1.StripeInvoicePurposePreview
	if ir.Spec.Draft {
		purpose = stripev1alpha1.StripeInvoicePurposePreviewDraft
	}
	invoice := &stripev1alpha1.StripeInvoice{
		ObjectMeta: metav1.ObjectMeta{Name: stripeInvoiceName(ir), Namespace: ir.Namespace},
		Spec: stripev1alpha1.StripeInvoiceSpec{
			BillingAccountRef: stripev1alpha1.BillingAccountLocalRef{Name: account.Name},
			Purpose:           purpose,
			Currency:          stripePreviewCurrency,
		},
	}
	for i, c := range charges {
		line := stripev1alpha1.StripeInvoiceLine{
			Key:               chargeKey(i),
			Description:       c.name,
			ServicePricingRef: stripev1alpha1.ServicePricingRef{Namespace: billingv1alpha1.DefaultServicePricingNamespace, Name: c.pricing},
			Quantity:          c.quantity,
			UnitAmount:        c.unitAmount,
			Amount:            c.amountBeforeDiscount,
			Period: stripev1alpha1.StripeInvoiceLinePeriod{
				Start: metav1.NewTime(c.start), End: metav1.NewTime(c.end),
			},
			Metadata: map[string]string{"service_pricing": c.pricing},
		}
		line.DiscountPercent = c.discount
		line.Tax = c.tax
		line.Note = c.note
		invoice.Spec.Lines = append(invoice.Spec.Lines, line)
	}
	if err := controllerutil.SetControllerReference(ir, invoice, r.Scheme()); err != nil {
		return nil, fmt.Errorf("own StripeInvoice: %w", err)
	}
	return invoice, nil
}

// succeedFromStripe completes the request with Stripe's preview: its
// amounts, discounts and tax for each charge, and the draft when one was
// asked for.
func (r *InvoiceRequestReconciler) succeedFromStripe(
	ctx context.Context,
	ir *billingv1alpha1.InvoiceRequest,
	charges []firstCharge,
	invoice *stripev1alpha1.StripeInvoice,
	msg string,
) error {
	byKey := map[string]stripev1alpha1.StripeInvoiceLineStatus{}
	for _, l := range invoice.Status.Lines {
		byKey[l.Key] = l
	}
	lines := make([]billingv1alpha1.InvoiceRequestLine, 0, len(charges))
	for i, c := range charges {
		got := byKey[chargeKey(i)]
		line := c.line
		line.Description = c.name
		line.Quantity = c.quantity
		line.UnitAmount = c.unitAmount
		line.Discount = got.Discount
		line.Tax = got.Tax
		if after, err := subtract(got.Amount, got.Discount); err == nil && got.Amount != "" {
			line.Amount = after
		}
		lines = append(lines, line)
	}
	total, err := subtract(invoice.Status.Subtotal, invoice.Status.Discount)
	if err != nil {
		return fmt.Errorf("total from Stripe: %w", err)
	}
	if apimeta.IsStatusConditionFalse(invoice.Status.Conditions, stripev1alpha1.StripeInvoiceConditionAmountsMatch) {
		cond := apimeta.FindStatusCondition(invoice.Status.Conditions, stripev1alpha1.StripeInvoiceConditionAmountsMatch)
		msg += ". Stripe's amounts differ from the Offer's: " + cond.Message
	}
	ir.Status.CurrencyCode = stripePreviewCurrency
	ir.Status.Lines = lines
	ir.Status.Total = total
	ir.Status.Tax = invoice.Status.Tax
	ir.Status.TotalWithTax = invoice.Status.Total
	ir.Status.StartingBalance = invoice.Status.StartingBalance
	ir.Status.AmountDue = invoice.Status.AmountDue
	msg += carriedBalance(invoice.Status.StartingBalance)
	if ir.Spec.Draft && invoice.Status.DashboardURL != "" {
		ir.Status.Draft = &billingv1alpha1.InvoiceRequestDraft{URL: invoice.Status.DashboardURL, ExpiresAt: invoice.Status.ExpiresAt}
	}
	return r.complete(ctx, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded, msg)
}

// previewFirstInvoiceResult completes a first-invoice preview from Stripe's
// preview, or from the provider's own arithmetic when Stripe has none, or
// asks to be called again while Stripe works it out.
func (r *InvoiceRequestReconciler) previewFirstInvoiceResult(
	ctx context.Context,
	ir *billingv1alpha1.InvoiceRequest,
	account *billingv1alpha1.BillingAccount,
	charges []firstCharge,
	total, msg string,
) (ctrl.Result, error) {
	preview, err := r.previewThroughStripe(ctx, ir, account, charges)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !preview.done {
		return ctrl.Result{RequeueAfter: stripePreviewRequeue}, nil
	}
	if preview.invoice != nil {
		return ctrl.Result{}, r.succeedFromStripe(ctx, ir, charges, preview.invoice, msg+". Worked out by Stripe, tax included")
	}
	var lines []billingv1alpha1.InvoiceRequestLine
	for _, c := range charges {
		lines = append(lines, c.line)
	}
	msg += ". Tax isn't included: " + preview.fallback
	if ir.Spec.Draft {
		msg += ", so there's no draft to open"
	}
	return ctrl.Result{}, r.succeed(ctx, ir, account.Spec.CurrencyCode, lines, total, msg)
}

// stripeInvoiceName is the name of a request's StripeInvoice.
func stripeInvoiceName(ir *billingv1alpha1.InvoiceRequest) string {
	name := ir.Name + "-stripe"
	if len(name) > 253 {
		name = strings.TrimRight(name[:253], "-.")
	}
	return name
}

func chargeKey(i int) string { return fmt.Sprintf("charge-%d", i) }

// subtract is a - b for decimal strings; an empty b is zero.
func subtract(a, b string) (string, error) {
	x, err := decimal.NewFromString(a)
	if err != nil {
		return "", err
	}
	if b == "" {
		return x.StringFixed(2), nil
	}
	y, err := decimal.NewFromString(b)
	if err != nil {
		return "", err
	}
	return x.Sub(y).StringFixed(2), nil
}

// carriedBalance is what the request's message says about a starting
// balance Stripe carries onto the invoice, or nothing without one.
func carriedBalance(balance string) string {
	b, err := decimal.NewFromString(balance)
	switch {
	case err != nil || b.IsZero():
		return ""
	case b.IsPositive():
		return fmt.Sprintf(". The account already owes %s, which Stripe adds to this invoice", b.StringFixed(2))
	default:
		return fmt.Sprintf(". The account has %s in credit, which Stripe takes off this invoice", b.Neg().StringFixed(2))
	}
}
