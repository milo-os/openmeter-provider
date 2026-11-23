// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	om "github.com/openmeterio/openmeter/api/client/go"
	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

const (
	// ChargeFinalizer holds a Charge until the provider has seen it go, so
	// a removed charge that was already on an invoice is reported.
	ChargeFinalizer = "openmeter.billing.miloapis.com/charge"

	chargeControllerName = "charge"

	// chargeDateLayout is how a charge's service period dates are written.
	chargeDateLayout = "2006-01-02"

	// Reasons on a Charge's Ready condition, and its events.
	ChargeReasonWaitingForInvoiceWriter = "WaitingForInvoiceWriter"
	ChargeReasonWaitingForCustomer      = "WaitingForCustomer"
	ChargeReasonWaitingForAmounts       = "WaitingForAmounts"
	ChargeReasonWaitingForInvoiceGroup  = "WaitingForInvoiceGroup"
	ChargeReasonScheduled               = "Scheduled"
	ChargeReasonInvoiced                = "Invoiced"
	ChargeReasonRefused                 = "Refused"
	// Reasons on a Charge's OnInvoice condition.
	ChargeReasonHeld                 = "Held"
	ChargeReasonAdding               = "Adding"
	ChargeReasonAdded                = "Added"
	EventReasonChargeInvoiced        = "ChargeInvoiced"
	EventReasonChargeAlreadyInvoiced = "ChargeAlreadyInvoiced"
	EventReasonChargeNotBilled       = "ChargeNotBilled"

	// chargeRecheckAfter is how often a scheduled charge is looked at for
	// being invoiced, besides whenever an Invoice in its namespace changes.
	chargeRecheckAfter = time.Hour
	// chargeWaitAfter is how soon a charge waiting on something that
	// doesn't trigger a reconcile of its own is looked at again.
	chargeWaitAfter = time.Minute
	// DefaultChargeHold is how long before its invoice date a next-invoice
	// charge is put on OpenMeter's invoice. Until then it's held in billing
	// alone, where removing it is just a delete: OpenMeter can only take a
	// line off an invoice by replacing the whole invoice, which can drop a
	// line subscription sync adds at the same time. A day leaves room to
	// retry before OpenMeter cuts the period's invoice.
	DefaultChargeHold = 24 * time.Hour
)

// ChargeReconciler puts each billing Charge on its account's OpenMeter
// invoice as a pending line that keeps the charge's service period, and
// reports where it is.
//
// Charges are only scheduled on accounts whose BillingAccount has
// InvoiceWriterReady: their invoices are written to Stripe by the invoice
// writer, which builds each charge's line with its product, quantity,
// discount and note. Elsewhere a charge waits, Pending, and billing's
// webhook refuses new ones.
type ChargeReconciler struct {
	client.Client
	OpenMeterClient openmeter.Client
	Recorder        record.EventRecorder
	// Now returns the current time; time.Now when unset.
	Now func() time.Time
	// Hold is how long before its invoice date a next-invoice charge goes
	// on OpenMeter's invoice; DefaultChargeHold when zero.
	Hold time.Duration
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=charges,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=charges/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=charges/finalizers,verbs=update
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=invoices,verbs=get;list;watch

// Reconcile holds, schedules, follows and lets go of one Charge. A status
// write that lost to another change starts over from a fresh read.
func (r *ChargeReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	res, err := r.reconcile(ctx, req)
	if apierrors.IsConflict(err) {
		return ctrl.Result{Requeue: true}, nil
	}
	return res, err
}

func (r *ChargeReconciler) reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	var charge billingv1alpha1.Charge
	if err := r.Get(ctx, req.NamespacedName, &charge); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	account, err := r.accountFor(ctx, &charge)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !charge.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.reconcileDelete(ctx, &charge, account)
	}
	if !controllerutil.ContainsFinalizer(&charge, ChargeFinalizer) {
		patch := client.MergeFrom(charge.DeepCopy())
		controllerutil.AddFinalizer(&charge, ChargeFinalizer)
		if err := r.Patch(ctx, &charge, patch); err != nil {
			return ctrl.Result{}, err
		}
	}
	switch charge.Status.Phase {
	case billingv1alpha1.ChargePhaseInvoiced, billingv1alpha1.ChargePhaseFailed:
		return ctrl.Result{}, nil
	}

	if account == nil || !account.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if !apimeta.IsStatusConditionTrue(account.Status.Conditions, billingv1alpha1.BillingAccountConditionInvoiceWriterReady) {
		return ctrl.Result{}, r.setPending(ctx, &charge, ChargeReasonWaitingForInvoiceWriter,
			"The billing account's invoices aren't written by the invoice writer yet, so the charge waits")
	}
	if charge.Status.AmountDue == "" {
		return ctrl.Result{RequeueAfter: chargeWaitAfter}, r.setPending(ctx, &charge, ChargeReasonWaitingForAmounts,
			"Waiting for billing to work out the charge's amount")
	}

	customerKey := openmeter.CustomerKey(account.UID)
	customer, err := r.OpenMeterClient.GetCustomer(ctx, customerKey)
	if errors.Is(err, openmeter.ErrCustomerNotFound) {
		return ctrl.Result{RequeueAfter: chargeWaitAfter}, r.setPending(ctx, &charge, ChargeReasonWaitingForCustomer,
			"Waiting for the billing account's OpenMeter customer")
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	customerID := openmeter.CustomerID(customer.Id)

	ref, err := r.OpenMeterClient.FindChargeLine(ctx, customerID, string(charge.UID))
	if err != nil {
		return ctrl.Result{}, err
	}
	if ref == nil {
		if waiting, err := r.waitForInvoiceGroup(ctx, &charge); err != nil || waiting {
			return ctrl.Result{}, err
		}
		invoiceAt, err := r.invoiceAt(ctx, &charge, account, customerKey)
		if err != nil {
			return ctrl.Result{}, err
		}
		now := r.now()
		if charge.Spec.Invoice != billingv1alpha1.ChargeInvoiceNow {
			if until := invoiceAt.Add(-r.hold()); now.Before(until) {
				// Held: worked out again each time, as the billing period
				// can move, and woken in time to add it.
				return ctrl.Result{RequeueAfter: min(until.Sub(now), chargeRecheckAfter)},
					r.setHeld(ctx, &charge, invoiceAt, until)
			}
		}
		line, err := r.chargeLine(&charge, invoiceAt)
		if err != nil {
			return ctrl.Result{}, r.fail(ctx, &charge, err.Error())
		}
		// Marked first, with an optimistic write: from here the webhook
		// refuses to remove the charge, and a removal already under way
		// makes this write conflict, so the line is never added for a
		// charge that's going.
		if err := r.setAdding(ctx, &charge); err != nil {
			return ctrl.Result{}, err
		}
		created, err := r.OpenMeterClient.CreateChargeLine(ctx, customerID, line)
		if err != nil {
			if openmeter.IsPermanent(err) {
				return ctrl.Result{}, r.fail(ctx, &charge, fmt.Sprintf("OpenMeter refused the charge: %v", err))
			}
			return ctrl.Result{}, err
		}
		ref = &created
	}

	if charge.Spec.Invoice == billingv1alpha1.ChargeInvoiceNow && ref.InvoiceStatus == om.InvoiceStatusGathering {
		lineIDs, ready, err := r.invoiceGroupLines(ctx, customerID, &charge, ref)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			// The group's other charges get their lines on their own
			// reconciles, which bring this one back through the sibling
			// watch.
			return ctrl.Result{RequeueAfter: chargeWaitAfter}, nil
		}
		if err := r.OpenMeterClient.InvoiceChargeLinesNow(ctx, customerID, lineIDs...); err != nil {
			return ctrl.Result{}, err
		}
		if ref, err = r.OpenMeterClient.FindChargeLine(ctx, customerID, string(charge.UID)); err != nil {
			return ctrl.Result{}, err
		}
		if ref == nil {
			return ctrl.Result{RequeueAfter: chargeWaitAfter}, nil
		}
	}

	if ref.Pending() {
		return ctrl.Result{RequeueAfter: chargeRecheckAfter}, r.setScheduled(ctx, &charge, ref.InvoiceAt)
	}
	return r.setInvoiced(ctx, &charge, ref.InvoiceID)
}

// invoiceGroupMembers returns the other charges of charge's billing account
// in its invoice group, leaving out any being removed.
func (r *ChargeReconciler) invoiceGroupMembers(ctx context.Context, charge *billingv1alpha1.Charge) ([]billingv1alpha1.Charge, error) {
	group := charge.Spec.InvoiceGroup
	if group == nil || charge.Spec.Invoice != billingv1alpha1.ChargeInvoiceNow {
		return nil, nil
	}
	var list billingv1alpha1.ChargeList
	if err := r.List(ctx, &list, client.InNamespace(charge.Namespace)); err != nil {
		return nil, err
	}
	var members []billingv1alpha1.Charge
	for _, m := range list.Items {
		if m.UID == charge.UID || !m.DeletionTimestamp.IsZero() ||
			m.Spec.BillingAccountRef.Name != charge.Spec.BillingAccountRef.Name ||
			m.Spec.InvoiceGroup == nil || m.Spec.InvoiceGroup.Name != group.Name {
			continue
		}
		members = append(members, m)
	}
	return members, nil
}

// waitForInvoiceGroup holds a charge in an invoice group, Pending, until
// all the group's charges are added, so none goes on an invoice without
// the rest. A group stays closed once any of its charges is on its way to
// the invoice, so a charge removed after that doesn't hold up the others.
func (r *ChargeReconciler) waitForInvoiceGroup(ctx context.Context, charge *billingv1alpha1.Charge) (bool, error) {
	group := charge.Spec.InvoiceGroup
	members, err := r.invoiceGroupMembers(ctx, charge)
	if err != nil || group == nil || charge.Spec.Invoice != billingv1alpha1.ChargeInvoiceNow {
		return false, err
	}
	added := int32(len(members)) + 1
	if added >= group.Size {
		return false, nil
	}
	for i := range members {
		if apimeta.IsStatusConditionTrue(members[i].Status.Conditions, billingv1alpha1.ChargeConditionOnInvoice) ||
			members[i].Status.Phase == billingv1alpha1.ChargePhaseInvoiced {
			return false, nil
		}
	}
	return true, r.setPending(ctx, charge, ChargeReasonWaitingForInvoiceGroup, fmt.Sprintf(
		"Waiting for the rest of invoice group %q: %d of its %d charges are added", group.Name, added, group.Size))
}

// invoiceGroupLines returns the lines to invoice now with charge's line
// ref: just its own, or, in an invoice group, the pending lines of all the
// group's charges once each has one. ready is false while one of them is
// still getting its line. A charge OpenMeter refused is left out, so the
// rest, which can no longer be removed, still go out together.
func (r *ChargeReconciler) invoiceGroupLines(ctx context.Context, customerID openmeter.CustomerID,
	charge *billingv1alpha1.Charge, ref *openmeter.ChargeLineRef) (lineIDs []string, ready bool, err error) {
	lineIDs = []string{ref.LineID}
	members, err := r.invoiceGroupMembers(ctx, charge)
	if err != nil {
		return nil, false, err
	}
	for i := range members {
		m := &members[i]
		if m.Status.Phase == billingv1alpha1.ChargePhaseFailed {
			continue
		}
		mref, err := r.OpenMeterClient.FindChargeLine(ctx, customerID, string(m.UID))
		if err != nil {
			return nil, false, err
		}
		switch {
		case mref == nil:
			return nil, false, nil
		case mref.InvoiceStatus == om.InvoiceStatusGathering:
			lineIDs = append(lineIDs, mref.LineID)
		}
	}
	return lineIDs, true, nil
}

// accountFor reads the charge's billing account, or nil when it's gone.
func (r *ChargeReconciler) accountFor(ctx context.Context, charge *billingv1alpha1.Charge) (*billingv1alpha1.BillingAccount, error) {
	var account billingv1alpha1.BillingAccount
	key := types.NamespacedName{Namespace: charge.Namespace, Name: charge.Spec.BillingAccountRef.Name}
	if err := r.Get(ctx, key, &account); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &account, nil
}

// invoiceAt is when charge is invoiced: now for an invoice-now charge,
// otherwise the end of the subscription's current billing period, or the
// account's next billing day when there's no subscription.
func (r *ChargeReconciler) invoiceAt(ctx context.Context, charge *billingv1alpha1.Charge,
	account *billingv1alpha1.BillingAccount, customerKey openmeter.CustomerKey) (time.Time, error) {
	now := r.now()
	if charge.Spec.Invoice == billingv1alpha1.ChargeInvoiceNow {
		return now, nil
	}
	end, ok, err := r.OpenMeterClient.CurrentBillingPeriodEnd(ctx, customerKey)
	if err != nil {
		return time.Time{}, err
	}
	if !ok {
		end = nextBillingDay(account, now)
	}
	return end, nil
}

func (r *ChargeReconciler) hold() time.Duration {
	if r.Hold > 0 {
		return r.Hold
	}
	return DefaultChargeHold
}

// chargeLine is the pending line for charge, invoiced at invoiceAt.
func (r *ChargeReconciler) chargeLine(charge *billingv1alpha1.Charge, invoiceAt time.Time) (openmeter.ChargeLine, error) {
	if charge.Spec.ServicePeriod == nil {
		return openmeter.ChargeLine{}, errors.New("the charge has no service period")
	}
	from, err := time.Parse(chargeDateLayout, charge.Spec.ServicePeriod.Start)
	if err != nil {
		return openmeter.ChargeLine{}, fmt.Errorf("service period start: %w", err)
	}
	last, err := time.Parse(chargeDateLayout, charge.Spec.ServicePeriod.End)
	if err != nil {
		return openmeter.ChargeLine{}, fmt.Errorf("service period end: %w", err)
	}

	taxCode := ""
	if tax := charge.Spec.Tax; tax != nil {
		if tax.Taxable != nil && !*tax.Taxable {
			taxCode = openmeter.NontaxableTaxCode
		} else {
			taxCode = tax.Code
		}
	}
	currency := charge.Spec.Currency
	if currency == "" {
		currency = "USD"
	}
	return openmeter.ChargeLine{
		Charge:          charge.Namespace + "/" + charge.Name,
		ChargeUID:       string(charge.UID),
		Currency:        currency,
		Name:            charge.Spec.Description,
		Amount:          charge.Status.AmountDue,
		Quantity:        charge.Spec.Quantity,
		UnitAmount:      charge.Spec.UnitAmount,
		DiscountPercent: charge.Spec.DiscountPercent,
		// OpenMeter's periods are half-open, so the line runs to the start
		// of the day after the charge's last.
		From:      from,
		To:        last.AddDate(0, 0, 1),
		InvoiceAt: invoiceAt,
		TaxCode:   taxCode,
	}, nil
}

// nextBillingDay is the account's next billing date after now, for an
// account without a live subscription: its contract's billing day once the
// contract has started, otherwise its invoice day. Months shorter than the
// day bill on their last day.
func nextBillingDay(account *billingv1alpha1.BillingAccount, now time.Time) time.Time {
	day := int(billingv1alpha1.StandardPaymentTermsInvoiceDayOfMonth)
	if account.Spec.PaymentTerms != nil && account.Spec.PaymentTerms.InvoiceDayOfMonth > 0 {
		day = int(account.Spec.PaymentTerms.InvoiceDayOfMonth)
	}
	if c := account.Status.Contract; c != nil && c.BillingDayOfMonth > 0 && !c.StartsAt.After(now) {
		day = int(c.BillingDayOfMonth)
	}
	now = now.UTC()
	for months := 0; ; months++ {
		first := time.Date(now.Year(), now.Month()+time.Month(months), 1, 0, 0, 0, 0, time.UTC)
		d := min(day, first.AddDate(0, 1, -1).Day())
		if date := time.Date(first.Year(), first.Month(), d, 0, 0, 0, 0, time.UTC); date.After(now) {
			return date
		}
	}
}

// reconcileDelete lets a removed charge go. It never takes a line off an
// OpenMeter invoice: billing's webhook only lets a charge be removed before
// its line is added (OnInvoice), or when its account or namespace is going.
// When the account itself is being deleted, a line already added is left
// for the account's final invoice, which its finalizer issues before it
// deletes the customer; a charge still held isn't billed, and says so.
func (r *ChargeReconciler) reconcileDelete(ctx context.Context, charge *billingv1alpha1.Charge, account *billingv1alpha1.BillingAccount) error {
	if !controllerutil.ContainsFinalizer(charge, ChargeFinalizer) {
		return nil
	}
	onInvoice := apimeta.IsStatusConditionTrue(charge.Status.Conditions, billingv1alpha1.ChargeConditionOnInvoice)

	switch {
	case account == nil || !account.DeletionTimestamp.IsZero():
		if !onInvoice && charge.Status.Phase != billingv1alpha1.ChargePhaseInvoiced {
			r.event(charge, "Warning", EventReasonChargeNotBilled,
				"The charge wasn't billed: its billing account was deleted before the charge reached an invoice")
		}
	case onInvoice:
		// The webhook refuses this, so it's a charge whose namespace is
		// going while its account isn't yet. Say where it's billed.
		customer, err := r.OpenMeterClient.GetCustomer(ctx, openmeter.CustomerKey(account.UID))
		if err != nil && !errors.Is(err, openmeter.ErrCustomerNotFound) {
			return err
		}
		if err == nil {
			ref, err := r.OpenMeterClient.FindChargeLine(ctx, openmeter.CustomerID(customer.Id), string(charge.UID))
			if err != nil {
				return err
			}
			if ref != nil {
				r.event(charge, "Warning", EventReasonChargeAlreadyInvoiced,
					fmt.Sprintf("The charge was removed after it was put on OpenMeter invoice %s, so it's still billed; correct the invoice with a credit note once it's issued", ref.InvoiceID))
			}
		}
	}

	patch := client.MergeFrom(charge.DeepCopy())
	controllerutil.RemoveFinalizer(charge, ChargeFinalizer)
	return r.Patch(ctx, charge, patch)
}

// setPending records why the charge waits.
func (r *ChargeReconciler) setPending(ctx context.Context, charge *billingv1alpha1.Charge, reason, msg string) error {
	return r.patchStatus(ctx, charge, func(s *billingv1alpha1.ChargeStatus) {
		s.Phase = billingv1alpha1.ChargePhasePending
		s.RemovableUntil = nil
		r.setReady(charge, s, metav1.ConditionFalse, reason, msg)
	})
}

// setHeld records that the charge is due on the invoice at invoiceAt, and
// is held in billing until it goes on it, at until.
func (r *ChargeReconciler) setHeld(ctx context.Context, charge *billingv1alpha1.Charge, invoiceAt, until time.Time) error {
	return r.patchStatus(ctx, charge, func(s *billingv1alpha1.ChargeStatus) {
		s.Phase = billingv1alpha1.ChargePhaseScheduled
		at := metav1.NewTime(invoiceAt.UTC().Truncate(time.Second))
		removable := metav1.NewTime(until.UTC().Truncate(time.Second))
		s.InvoiceAt, s.RemovableUntil = &at, &removable
		r.setReady(charge, s, metav1.ConditionTrue, ChargeReasonScheduled,
			fmt.Sprintf("Due on the invoice OpenMeter issues on %s", at.UTC().Format(time.RFC3339)))
		r.setOnInvoice(charge, s, metav1.ConditionFalse, ChargeReasonHeld,
			fmt.Sprintf("Held in billing, and can be removed, until it goes on the invoice at %s", removable.UTC().Format(time.RFC3339)))
	})
}

// setAdding records that the charge's line is about to be added. The
// write is what makes the charge unremovable, so it must land before the
// line is created: it's optimistic, and fails if the charge changed.
func (r *ChargeReconciler) setAdding(ctx context.Context, charge *billingv1alpha1.Charge) error {
	return r.patchStatus(ctx, charge, func(s *billingv1alpha1.ChargeStatus) {
		s.RemovableUntil = nil
		r.setOnInvoice(charge, s, metav1.ConditionTrue, ChargeReasonAdding, "Being put on its invoice in OpenMeter")
	})
}

// setScheduled records that the charge is on an invoice to come.
func (r *ChargeReconciler) setScheduled(ctx context.Context, charge *billingv1alpha1.Charge, invoiceAt time.Time) error {
	return r.patchStatus(ctx, charge, func(s *billingv1alpha1.ChargeStatus) {
		s.Phase = billingv1alpha1.ChargePhaseScheduled
		at := metav1.NewTime(invoiceAt.UTC().Truncate(time.Second))
		s.InvoiceAt, s.RemovableUntil = &at, nil
		r.setReady(charge, s, metav1.ConditionTrue, ChargeReasonScheduled,
			fmt.Sprintf("On the invoice OpenMeter issues on %s", at.UTC().Format(time.RFC3339)))
		r.setOnInvoice(charge, s, metav1.ConditionTrue, ChargeReasonAdded, "On its invoice in OpenMeter")
	})
}

// setInvoiced records that the charge is on an issued invoice, once the
// Invoice projection for it exists.
func (r *ChargeReconciler) setInvoiced(ctx context.Context, charge *billingv1alpha1.Charge, openMeterInvoiceID string) (ctrl.Result, error) {
	var invoices billingv1alpha1.InvoiceList
	if err := r.List(ctx, &invoices, client.InNamespace(charge.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	name := ""
	for _, inv := range invoices.Items {
		if inv.Annotations[InvoiceKeyAnnotation] == openMeterInvoiceID {
			name = inv.Name
			break
		}
	}
	if name == "" {
		// The invoice sync hasn't projected it yet; it will, and its
		// Invoice event brings the charge back here.
		return ctrl.Result{RequeueAfter: chargeWaitAfter}, nil
	}
	err := r.patchStatus(ctx, charge, func(s *billingv1alpha1.ChargeStatus) {
		s.Phase = billingv1alpha1.ChargePhaseInvoiced
		s.InvoiceRef = &billingv1alpha1.ChargeInvoiceRef{Name: name}
		s.RemovableUntil = nil
		r.setOnInvoice(charge, s, metav1.ConditionTrue, ChargeReasonAdded, "On its invoice in OpenMeter")
		r.setReady(charge, s, metav1.ConditionTrue, ChargeReasonInvoiced, fmt.Sprintf("Invoiced on %s", name))
	})
	if err == nil {
		r.event(charge, "Normal", EventReasonChargeInvoiced, fmt.Sprintf("Invoiced on %s", name))
	}
	return ctrl.Result{}, err
}

// fail records that the charge can't be billed as it is. It isn't on an
// invoice, so staff can remove it.
func (r *ChargeReconciler) fail(ctx context.Context, charge *billingv1alpha1.Charge, msg string) error {
	r.event(charge, "Warning", ChargeReasonRefused, msg)
	return r.patchStatus(ctx, charge, func(s *billingv1alpha1.ChargeStatus) {
		s.Phase = billingv1alpha1.ChargePhaseFailed
		s.RemovableUntil = nil
		r.setReady(charge, s, metav1.ConditionFalse, ChargeReasonRefused, msg)
		r.setOnInvoice(charge, s, metav1.ConditionFalse, ChargeReasonRefused, "Not on an invoice: OpenMeter refused it")
	})
}

func (r *ChargeReconciler) setOnInvoice(charge *billingv1alpha1.Charge, s *billingv1alpha1.ChargeStatus,
	status metav1.ConditionStatus, reason, msg string) {
	apimeta.SetStatusCondition(&s.Conditions, metav1.Condition{
		Type:               billingv1alpha1.ChargeConditionOnInvoice,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: charge.Generation,
	})
}

func (r *ChargeReconciler) setReady(charge *billingv1alpha1.Charge, s *billingv1alpha1.ChargeStatus,
	status metav1.ConditionStatus, reason, msg string) {
	apimeta.SetStatusCondition(&s.Conditions, metav1.Condition{
		Type:               billingv1alpha1.ChargeConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: charge.Generation,
	})
}

// patchStatus applies change to the charge's status and writes it, unless
// nothing changed. It writes only the fields this provider owns; billing
// writes the amounts. The write is optimistic: billing's webhook decides
// whether a charge can be removed from OnInvoice, so a write from a stale
// copy mustn't undo it, and adding a line mustn't race a removal.
func (r *ChargeReconciler) patchStatus(ctx context.Context, charge *billingv1alpha1.Charge, change func(*billingv1alpha1.ChargeStatus)) error {
	before := charge.DeepCopy()
	change(&charge.Status)
	if chargeStatusUnchanged(&before.Status, &charge.Status) {
		return nil
	}
	return r.Status().Patch(ctx, charge, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func chargeStatusUnchanged(a, b *billingv1alpha1.ChargeStatus) bool {
	if a.Phase != b.Phase || !a.InvoiceAt.Equal(b.InvoiceAt) || !a.RemovableUntil.Equal(b.RemovableUntil) ||
		(a.InvoiceRef == nil) != (b.InvoiceRef == nil) ||
		(a.InvoiceRef != nil && a.InvoiceRef.Name != b.InvoiceRef.Name) {
		return false
	}
	for _, t := range []string{billingv1alpha1.ChargeConditionReady, billingv1alpha1.ChargeConditionOnInvoice} {
		x := apimeta.FindStatusCondition(a.Conditions, t)
		y := apimeta.FindStatusCondition(b.Conditions, t)
		if (x == nil) != (y == nil) {
			return false
		}
		if x != nil && (x.Status != y.Status || x.Reason != y.Reason || x.Message != y.Message) {
			return false
		}
	}
	return true
}

func (r *ChargeReconciler) event(charge *billingv1alpha1.Charge, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(charge, eventType, reason, msg)
	}
}

func (r *ChargeReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager sets up the controller with the Manager. It's skipped
// where billing doesn't serve the Charge API yet.
func (r *ChargeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	gvk := billingv1alpha1.GroupVersion.WithKind("Charge")
	if _, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		if apimeta.IsNoMatchError(err) {
			mgr.GetLogger().Info("billing's Charge API isn't served, so charges are off", "kind", gvk.String())
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
	return ctrl.NewControllerManagedBy(mgr).
		Named(chargeControllerName).
		For(&billingv1alpha1.Charge{}).
		// An account moving to the invoice writer schedules its charges.
		Watches(&billingv1alpha1.BillingAccount{}, handler.EnqueueRequestsFromMapFunc(r.chargesOfAccount)).
		// An issued invoice's projection marks its charges invoiced.
		Watches(&billingv1alpha1.Invoice{}, handler.EnqueueRequestsFromMapFunc(r.openChargesInNamespace)).
		// A charge joining or moving on in an invoice group wakes the rest.
		Watches(&billingv1alpha1.Charge{}, handler.EnqueueRequestsFromMapFunc(r.invoiceGroupSiblings)).
		Complete(r)
}

// chargesOfAccount maps a BillingAccount to its charges.
func (r *ChargeReconciler) chargesOfAccount(ctx context.Context, obj client.Object) []reconcile.Request {
	var charges billingv1alpha1.ChargeList
	if err := r.List(ctx, &charges, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, c := range charges.Items {
		if c.Spec.BillingAccountRef.Name == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&c)})
		}
	}
	return reqs
}

// invoiceGroupSiblings maps a charge in an invoice group to the group's
// other charges.
func (r *ChargeReconciler) invoiceGroupSiblings(ctx context.Context, obj client.Object) []reconcile.Request {
	charge, ok := obj.(*billingv1alpha1.Charge)
	if !ok || charge.Spec.InvoiceGroup == nil {
		return nil
	}
	members, err := r.invoiceGroupMembers(ctx, charge)
	if err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(members))
	for i := range members {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&members[i])})
	}
	return reqs
}

// openChargesInNamespace maps an Invoice to the charges in its namespace
// that aren't invoiced yet.
func (r *ChargeReconciler) openChargesInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	var charges billingv1alpha1.ChargeList
	if err := r.List(ctx, &charges, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, c := range charges.Items {
		if c.Status.Phase == billingv1alpha1.ChargePhaseScheduled {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&c)})
		}
	}
	return reqs
}
