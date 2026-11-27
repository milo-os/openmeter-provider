/*
Copyright 2026 Datum Technology Inc.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, version 3.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

const invoiceRequestControllerName = "invoicerequest"

// InvoiceRequestReconciler fulfils InvoiceRequests for billing accounts
// this provider invoices, writing only status.
//
// OpenMeter invoices in-advance flat fees by itself as soon as they're due,
// so a contract's setup fee and first month are invoiced, and sent, the
// moment its subscription starts. There's nothing for staff to issue, so
// Issue requests fail as Unsupported. A Preview shows what the account
// will be invoiced next, for its fixed charges (its Offer's OneTime and
// Recurring pricings):
//
//   - before a contract starts: the first invoice, worked out from the
//     Offer and the contract's start date, so staff can check it before
//     anything reaches the customer;
//   - otherwise: the draft invoice waiting to be issued, if there is one,
//     or else the charges waiting for later periods, priced by OpenMeter.
//
// A request is done once its phase is Succeeded or Failed; it's never
// revisited after that.
type InvoiceRequestReconciler struct {
	client.Client
	OpenMeterClient openmeter.Client
	Recorder        record.EventRecorder
	// Now returns the current time; time.Now when unset.
	Now func() time.Time
	// StripeInvoices is true when stripe-provider's StripeInvoice API is
	// served, so first-invoice previews are worked out by Stripe, with tax.
	// SetupWithManager sets it.
	StripeInvoices bool
	// CustomChargePricing is the ServicePricing, in milo-system, a charge
	// draft's line without a catalog item is previewed under, as the
	// invoice writer bills it (invoiceWriter.customChargePricing).
	CustomChargePricing string
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=invoicerequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=invoicerequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingentitlements,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=offers,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=servicepricings,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=charges,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=chargedrafts,verbs=get;list;watch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripeinvoices,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripecustomers,verbs=get;list;watch

// Reconcile fulfils a single InvoiceRequest.
func (r *InvoiceRequestReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("invoiceRequest", req.Name, "namespace", req.Namespace)

	var ir billingv1alpha1.InvoiceRequest
	if err := r.Get(ctx, req.NamespacedName, &ir); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !ir.DeletionTimestamp.IsZero() || invoiceRequestDone(&ir) {
		return ctrl.Result{}, nil
	}
	ctx = log.IntoContext(ctx, logger.WithValues("billingAccount", ir.Spec.BillingAccountRef.Name, "mode", ir.Spec.Mode))

	if ir.Spec.Mode != billingv1alpha1.InvoiceRequestModePreview {
		return ctrl.Result{}, r.fail(ctx, &ir, billingv1alpha1.InvoiceRequestReasonUnsupported, fmt.Sprintf(
			"%s isn't supported: OpenMeter invoices one-time and recurring charges by itself as soon as they're due, so a contract's first invoice is sent when it starts", ir.Spec.Mode))
	}

	if ir.Status.Phase == "" {
		ir.Status.Phase = billingv1alpha1.InvoiceRequestPhasePending
		ir.Status.ObservedGeneration = ir.Generation
		if err := r.Status().Update(ctx, &ir); err != nil {
			return ctrl.Result{}, fmt.Errorf("mark pending: %w", err)
		}
	}

	var account billingv1alpha1.BillingAccount
	if err := r.Get(ctx, types.NamespacedName{Namespace: ir.Namespace, Name: ir.Spec.BillingAccountRef.Name}, &account); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get billing account: %w", err)
		}
		return ctrl.Result{}, r.fail(ctx, &ir, billingv1alpha1.InvoiceRequestReasonFailed,
			fmt.Sprintf("BillingAccount %q not found", ir.Spec.BillingAccountRef.Name))
	}

	if ir.Spec.ContractDraft != nil {
		return r.previewContractDraft(ctx, &ir, &account)
	}
	if ir.Spec.ChargeDraftRef != nil {
		return r.previewChargeDraft(ctx, &ir, &account)
	}

	be, err := r.entitlementFor(ctx, &account)
	if err != nil {
		return ctrl.Result{}, err
	}
	if be != nil && be.Spec.Contract != nil {
		if startsAt := be.Spec.Contract.StartsAt.UTC(); r.now().Before(startsAt) {
			return r.previewFirstInvoice(ctx, &ir, &account, be, startsAt)
		}
	}

	customer, err := r.OpenMeterClient.GetCustomer(ctx, openmeter.CustomerKey(account.UID))
	if err != nil {
		if errors.Is(err, openmeter.ErrCustomerNotFound) {
			return ctrl.Result{}, r.fail(ctx, &ir, billingv1alpha1.InvoiceRequestReasonFailed,
				fmt.Sprintf("BillingAccount %q has no OpenMeter customer yet", account.Name))
		}
		return r.openMeterFailed(ctx, &ir, fmt.Errorf("get customer: %w", err))
	}
	return r.previewNextInvoice(ctx, &ir, &account, openmeter.CustomerID(customer.Id))
}

// previewFirstInvoice works out a contract's first invoice before it
// starts, from its Offer's flat pricings. Nothing is subscribed yet, so
// OpenMeter has nothing to show; the charges are flat fees, priced here.
//
// Charges staff added that are held for an invoice on the day the contract
// starts are previewed with them: they're invoiced that day too, contract
// or not (see ChargeReconciler.invoiceAt). A held charge due on another day
// goes on a different invoice (before the start, the account's invoice for
// that period; after it, one the contract's subscription decides once it
// exists), so it's counted in the message and left out.
func (r *InvoiceRequestReconciler) previewFirstInvoice(
	ctx context.Context,
	ir *billingv1alpha1.InvoiceRequest,
	account *billingv1alpha1.BillingAccount,
	be *billingv1alpha1.BillingEntitlement,
	startsAt time.Time,
) (ctrl.Result, error) {
	var offer billingv1alpha1.Offer
	if err := r.Get(ctx, types.NamespacedName{Name: be.Spec.OfferRef.Name}, &offer); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get offer: %w", err)
		}
		return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed,
			fmt.Sprintf("Offer %q not found", be.Spec.OfferRef.Name))
	}
	starts := startsAt.Format(time.DateOnly)

	invoiceDay := 0
	if contractBillsOnInvoiceDay(be.Spec.Contract) {
		invoiceDay = subscriptionBillingAnchor(account).Day()
	}
	charges, total, err := firstInvoiceCharges(&offer, startsAt, invoiceDay)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed, err.Error())
	}
	fees := len(charges)

	held, err := r.heldChargeList(ctx, account)
	if err != nil {
		return ctrl.Result{}, err
	}
	added, elsewhere := 0, 0
	amounts := []string{total}
	for i := range held {
		c := &held[i]
		if at := c.Status.InvoiceAt; at == nil || at.UTC().Format(time.DateOnly) != starts {
			elsewhere++
			continue
		}
		charge, _, err := r.chargeDraftCharge(ctx, billingv1alpha1.ChargeDraftLine{
			ServicePricingRef: c.Spec.ServicePricingRef,
			Description:       c.Spec.Description,
			Quantity:          c.Spec.Quantity,
			UnitAmount:        c.Spec.UnitAmount,
			Tax:               c.Spec.Tax,
			DiscountPercent:   c.Spec.DiscountPercent,
			ServicePeriod:     c.Spec.ServicePeriod,
			Note:              c.Spec.Note,
		}, account, r.now())
		if err != nil {
			return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed,
				fmt.Sprintf("charge %q: %v", c.Name, err))
		}
		// What billing says is due, as the next-invoice preview shows it.
		charge.line.Amount = c.Status.AmountDue
		charges = append(charges, charge)
		amounts = append(amounts, c.Status.AmountDue)
		added++
	}
	if total, err = openmeter.AddDecimals(amounts...); err != nil {
		return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed, err.Error())
	}

	other := ""
	if elsewhere > 0 {
		other = fmt.Sprintf(". %s due on another day %s on a different invoice and not shown",
			countOf(elsewhere, "added charge", "added charges"), isAre(elsewhere))
	}
	switch {
	case len(charges) == 0:
		return ctrl.Result{}, r.succeed(ctx, ir, account.Spec.CurrencyCode, nil, "0",
			fmt.Sprintf("Offer %q has no one-time or recurring charges to invoice when the contract starts on %s", offer.Name, starts)+other)
	case fees == 0:
		return r.previewFirstInvoiceResult(ctx, ir, account, charges, total, fmt.Sprintf(
			"Offer %q has no one-time or recurring charges of its own; %s to the account %s invoiced when the contract starts on %s",
			offer.Name, countOf(added, "charge added", "charges added"), isAre(added), starts)+other)
	case added > 0:
		return r.previewFirstInvoiceResult(ctx, ir, account, charges, total, fmt.Sprintf(
			"The first invoice, sent when the contract starts on %s, with %s to the account that %s invoiced the same day",
			starts, countOf(added, "charge added", "charges added"), isAre(added))+other)
	}
	return r.previewFirstInvoiceResult(ctx, ir, account, charges, total,
		fmt.Sprintf("The first invoice, sent when the contract starts on %s", starts)+other)
}

// countOf is "1 charge" or "3 charges".
func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// previewNextInvoice shows the draft invoice waiting to be issued, or else
// the fixed charges waiting for later periods. A draft with nothing to pay is
// passed over: moving an account onto a contract ends its old subscription,
// whose last hours of usage wait in a draft for the collection period, often
// at zero, while the contract's own fees have already been invoiced.
func (r *InvoiceRequestReconciler) previewNextInvoice(
	ctx context.Context,
	ir *billingv1alpha1.InvoiceRequest,
	account *billingv1alpha1.BillingAccount,
	customerID openmeter.CustomerID,
) (ctrl.Result, error) {
	draft, err := r.OpenMeterClient.DraftInvoice(ctx, customerID)
	if err != nil {
		return r.openMeterFailed(ctx, ir, fmt.Errorf("get draft invoice: %w", err))
	}
	if draft != nil {
		lines, total, err := requestLines(openmeter.LineItems(*draft))
		if err != nil {
			return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed, err.Error())
		}
		zero, err := openmeter.IsZeroDecimal(total)
		if err != nil {
			return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed, err.Error())
		}
		if !zero {
			return ctrl.Result{}, r.succeed(ctx, ir, draft.Currency, lines, total,
				fmt.Sprintf("Draft invoice %s, which OpenMeter issues and sends when its collection period ends", draft.Id))
		}
	}

	upcoming, err := r.OpenMeterClient.UpcomingFixedCharges(ctx, customerID)
	if err != nil {
		return r.openMeterFailed(ctx, ir, fmt.Errorf("list upcoming fixed charges: %w", err))
	}
	held, heldTotal, err := r.heldCharges(ctx, account)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(upcoming) == 0 && len(held) == 0 {
		return ctrl.Result{}, r.succeed(ctx, ir, account.Spec.CurrencyCode, nil, "0",
			"No one-time or recurring charges are waiting to be invoiced")
	}
	lines, total, err := requestLines(upcoming)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed, err.Error())
	}
	if total, err = openmeter.AddDecimals(total, heldTotal); err != nil {
		return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed, err.Error())
	}
	currency := account.Spec.CurrencyCode
	if len(upcoming) > 0 {
		currency = upcoming[0].Currency
	}
	return ctrl.Result{}, r.succeed(ctx, ir, currency, append(lines, held...), total,
		"One-time and recurring charges waiting for later billing periods")
}

// heldCharges are the account's held Charges (see heldChargeList) as
// preview lines with their total.
func (r *InvoiceRequestReconciler) heldCharges(ctx context.Context, account *billingv1alpha1.BillingAccount) ([]billingv1alpha1.InvoiceRequestLine, string, error) {
	held, err := r.heldChargeList(ctx, account)
	if err != nil {
		return nil, "", err
	}
	var lines []billingv1alpha1.InvoiceRequestLine
	amounts := []string{"0"}
	for _, c := range held {
		line := billingv1alpha1.InvoiceRequestLine{Description: c.Spec.Description, Amount: c.Status.AmountDue}
		if p := c.Spec.ServicePeriod; p != nil {
			start, errStart := time.Parse(chargeDateLayout, p.Start)
			last, errEnd := time.Parse(chargeDateLayout, p.End)
			if errStart == nil && errEnd == nil {
				line.Period = &billingv1alpha1.InvoicePeriod{
					Start: metav1.NewTime(start),
					End:   metav1.NewTime(last.AddDate(0, 0, 1).Add(-time.Second)),
				}
			}
		}
		lines = append(lines, line)
		amounts = append(amounts, c.Status.AmountDue)
	}
	total, err := openmeter.AddDecimals(amounts...)
	if err != nil {
		return nil, "", fmt.Errorf("held charges' total: %w", err)
	}
	return lines, total, nil
}

// heldChargeList are the account's next-invoice Charges that the Charge
// controller is still holding in billing (see DefaultChargeHold). They
// aren't on OpenMeter's invoice yet, so its upcoming charges don't include
// them. None where billing doesn't serve the Charge API.
func (r *InvoiceRequestReconciler) heldChargeList(ctx context.Context, account *billingv1alpha1.BillingAccount) ([]billingv1alpha1.Charge, error) {
	var charges billingv1alpha1.ChargeList
	if err := r.List(ctx, &charges, client.InNamespace(account.Namespace)); err != nil {
		if apimeta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list charges: %w", err)
	}
	var held []billingv1alpha1.Charge
	for _, c := range charges.Items {
		if c.Spec.BillingAccountRef.Name != account.Name || !c.DeletionTimestamp.IsZero() ||
			c.Spec.Invoice == billingv1alpha1.ChargeInvoiceNow ||
			c.Status.Phase != billingv1alpha1.ChargePhaseScheduled || c.Status.AmountDue == "" ||
			apimeta.IsStatusConditionTrue(c.Status.Conditions, billingv1alpha1.ChargeConditionOnInvoice) {
			continue
		}
		held = append(held, c)
	}
	return held, nil
}

// entitlementFor returns the account's BillingEntitlement that isn't being
// deleted, or nil. Billing admits one per account.
func (r *InvoiceRequestReconciler) entitlementFor(ctx context.Context, account *billingv1alpha1.BillingAccount) (*billingv1alpha1.BillingEntitlement, error) {
	var list billingv1alpha1.BillingEntitlementList
	if err := r.List(ctx, &list, client.InNamespace(account.Namespace),
		client.MatchingFields{EntitlementBillingAccountRefField: account.Name}); err != nil {
		return nil, fmt.Errorf("list billing entitlements for account: %w", err)
	}
	for i := range list.Items {
		if list.Items[i].DeletionTimestamp.IsZero() {
			return &list.Items[i], nil
		}
	}
	return nil, nil
}

// firstCharge is one charge on a contract's first invoice, with what a
// Stripe preview of it needs.
type firstCharge struct {
	line billingv1alpha1.InvoiceRequestLine
	// pricing is the ServicePricing charged.
	pricing string
	// name is the charge's name without its quantity, which Stripe shows
	// itself.
	name string
	// quantity and unitAmount come to amountBeforeDiscount: the fee's
	// quantity and amount, or, for a prorated first period, 1 and the
	// prorated amount.
	quantity, unitAmount, amountBeforeDiscount string
	// discount is the percentage off, as a decimal string, or empty.
	discount   string
	start, end time.Time
	// tax overrides the pricing's tax code, for a charge that gives its own.
	tax *stripev1alpha1.StripeInvoiceLineTax
	// note is for staff, kept on the Stripe line's metadata.
	note string
}

// firstInvoiceCharges are the charges a contract starting at startsAt is
// first invoiced for, with their total: each OneTime pricing, and the
// first period of each Recurring one billed in advance (one billed in
// arrears is invoiced when its period ends), each times its quantity and after
// the Offer's discounts. A contract billed on the invoice day (invoiceDay
// above 0) has a first period that ends on each fee's first billing date,
// prorated as OpenMeter prorates it. Names match the Offer's plan (see
// addFlatFee). Usage isn't included.
func firstInvoiceCharges(offer *billingv1alpha1.Offer, startsAt time.Time, invoiceDay int) ([]firstCharge, string, error) {
	var charges []firstCharge
	var amounts []string
	for _, sp := range offer.Spec.ServicePricings {
		if feeInArrears(sp) {
			// Invoiced at the end of its first period, not when it starts.
			continue
		}
		var period *billingv1alpha1.InvoicePeriod
		var part, whole time.Duration
		end := startsAt
		switch sp.Spec.ChargeType {
		case billingv1alpha1.ChargeTypeOneTime:
		case billingv1alpha1.ChargeTypeRecurring:
			months := intervalMonths(sp.Spec.Interval)
			end = startsAt.AddDate(0, months, 0)
			if invoiceDay > 0 {
				if next := nextBillingDate(startsAt, invoiceDay, sp.Spec.Interval); next.After(startsAt) {
					end = next
				}
			}
			part, whole = end.Sub(startsAt), end.Sub(end.AddDate(0, -months, 0))
			period = &billingv1alpha1.InvoicePeriod{
				Start: metav1.NewTime(startsAt),
				End:   metav1.NewTime(end.Add(-time.Second)),
			}
		default:
			continue
		}
		var percentage int32
		if sp.Discount != nil {
			percentage = sp.Discount.Percentage
		}
		quantity := feeQuantity(sp)
		charged, err := openmeter.MultiplyAmount(sp.Spec.Amount, quantity)
		if err != nil {
			return nil, "", fmt.Errorf("service pricing %q: %w", sp.Name, err)
		}
		c := firstCharge{
			pricing: sp.Name, name: lineName(sp),
			quantity: fmt.Sprint(quantity), unitAmount: sp.Spec.Amount,
			start: startsAt, end: startsAt,
		}
		if percentage > 0 {
			c.discount = fmt.Sprint(percentage)
		}
		if period != nil {
			c.end = period.End.Time
		}
		// A one-time fee's supply date, when the contract gives one, is its
		// line's period, as on the invoice the writer writes.
		if sp.Spec.ChargeType == billingv1alpha1.ChargeTypeOneTime && sp.ServicePeriod != nil {
			if supply, ok := supplyPeriod(*sp.ServicePeriod); ok {
				c.start, c.end = supply.Start.Time, supply.End.Time
				period = &billingv1alpha1.InvoicePeriod{Start: supply.Start, End: supply.End}
			}
		}
		if part < whole {
			if charged, err = openmeter.ProrateAmount(charged, part, whole); err != nil {
				return nil, "", fmt.Errorf("service pricing %q: %w", sp.Name, err)
			}
			// A prorated period is one of its whole, so its unit price is the
			// prorated amount: quantity × unit price must come to it.
			c.quantity, c.unitAmount = "1", charged
		}
		c.amountBeforeDiscount = charged
		amount, err := openmeter.DiscountedAmount(charged, percentage)
		if err != nil {
			return nil, "", fmt.Errorf("service pricing %q: %w", sp.Name, err)
		}
		c.line = billingv1alpha1.InvoiceRequestLine{Description: feeName(sp), Period: period, Amount: amount}
		charges = append(charges, c)
		amounts = append(amounts, amount)
	}
	total, err := openmeter.AddDecimals(amounts...)
	if err != nil {
		return nil, "", fmt.Errorf("total: %w", err)
	}
	return charges, total, nil
}

// requestLines maps OpenMeter invoice lines to an InvoiceRequest's, with
// their total, each before tax.
func requestLines(in []om.InvoiceLine) ([]billingv1alpha1.InvoiceRequestLine, string, error) {
	lines := make([]billingv1alpha1.InvoiceRequestLine, 0, len(in))
	amounts := make([]string, 0, len(in))
	for _, line := range in {
		amount, err := openmeter.AmountBeforeTax(line.Totals)
		if err != nil {
			return nil, "", fmt.Errorf("line %s: %w", line.Id, err)
		}
		out := billingv1alpha1.InvoiceRequestLine{Description: line.Name, Amount: amount}
		// One-time charges cover an instant rather than a period.
		if line.Period.To.After(line.Period.From) {
			out.Period = &billingv1alpha1.InvoicePeriod{
				Start: metav1.NewTime(line.Period.From.Truncate(time.Second)),
				End:   metav1.NewTime(line.Period.To.Add(-time.Nanosecond).Truncate(time.Second)),
			}
		}
		lines = append(lines, out)
		amounts = append(amounts, amount)
	}
	total, err := openmeter.AddDecimals(amounts...)
	if err != nil {
		return nil, "", fmt.Errorf("total: %w", err)
	}
	return lines, total, nil
}

func invoiceRequestDone(ir *billingv1alpha1.InvoiceRequest) bool {
	return ir.Status.Phase == billingv1alpha1.InvoiceRequestPhaseSucceeded ||
		ir.Status.Phase == billingv1alpha1.InvoiceRequestPhaseFailed
}

// succeed completes the request with a preview.
func (r *InvoiceRequestReconciler) succeed(
	ctx context.Context,
	ir *billingv1alpha1.InvoiceRequest,
	currency string,
	lines []billingv1alpha1.InvoiceRequestLine,
	total string,
	msg string,
) error {
	ir.Status.CurrencyCode = currency
	ir.Status.Lines = lines
	ir.Status.Total = total
	return r.complete(ctx, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded, msg)
}

// fail completes the request unsuccessfully. A request is never retried
// once failed: staff create a new one.
func (r *InvoiceRequestReconciler) fail(ctx context.Context, ir *billingv1alpha1.InvoiceRequest, reason, msg string) error {
	if err := r.complete(ctx, ir, billingv1alpha1.InvoiceRequestPhaseFailed, reason, msg); err != nil {
		return err
	}
	r.event(ir, "Warning", reason, msg)
	return nil
}

func (r *InvoiceRequestReconciler) complete(
	ctx context.Context,
	ir *billingv1alpha1.InvoiceRequest,
	phase billingv1alpha1.InvoiceRequestPhase,
	reason, msg string,
) error {
	now := metav1.NewTime(r.now())
	ir.Status.Phase = phase
	ir.Status.CompletedAt = &now
	ir.Status.ObservedGeneration = ir.Generation
	apimeta.SetStatusCondition(&ir.Status.Conditions, metav1.Condition{
		Type:               billingv1alpha1.InvoiceRequestConditionCompleted,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: ir.Generation,
	})
	if err := r.Status().Update(ctx, ir); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	return nil
}

// openMeterFailed fails the request on a permanent OpenMeter error, and
// returns a transient one for a rate-limited retry, leaving it Pending.
func (r *InvoiceRequestReconciler) openMeterFailed(ctx context.Context, ir *billingv1alpha1.InvoiceRequest, err error) (ctrl.Result, error) {
	if openmeter.IsPermanent(err) {
		return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed, err.Error())
	}
	return ctrl.Result{}, err
}

func (r *InvoiceRequestReconciler) event(ir *billingv1alpha1.InvoiceRequest, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(ir, eventType, reason, msg)
	}
}

func (r *InvoiceRequestReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return systemNow()
}

// SetupWithManager registers the controller with the manager. Billing
// added InvoiceRequest after this provider's other kinds, so a control
// plane on an older billing skips the controller instead of failing to
// start: watching an API that isn't served would stop the whole manager.
// Upgrading billing later needs a restart to turn it on.
func (r *InvoiceRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	gvk := billingv1alpha1.GroupVersion.WithKind("InvoiceRequest")
	if _, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		if apimeta.IsNoMatchError(err) {
			mgr.GetLogger().Info("billing's InvoiceRequest API isn't served, so invoice requests are off", "kind", gvk.String())
			return nil
		}
		return fmt.Errorf("look up %s: %w", gvk, err)
	}
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("openmeter-provider") //nolint:staticcheck // SA1019: GetEventRecorder (events/v1) is a larger migration.
	}
	b := ctrl.NewControllerManagedBy(mgr).
		Named(invoiceRequestControllerName).
		For(&billingv1alpha1.InvoiceRequest{})
	// Previews go through Stripe only where stripe-provider serves
	// StripeInvoice; elsewhere they're the provider's own, without tax.
	sigvk := stripev1alpha1.GroupVersion.WithKind("StripeInvoice")
	if _, err := mgr.GetRESTMapper().RESTMapping(sigvk.GroupKind(), sigvk.Version); err == nil {
		r.StripeInvoices = true
		b = b.Owns(&stripev1alpha1.StripeInvoice{})
	} else if !apimeta.IsNoMatchError(err) {
		return fmt.Errorf("look up %s: %w", sigvk, err)
	} else {
		mgr.GetLogger().Info("stripe-provider's StripeInvoice API isn't served, so invoice previews don't include tax", "kind", sigvk.String())
	}
	return b.Complete(r)
}
