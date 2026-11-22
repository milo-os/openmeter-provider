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
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	om "github.com/openmeterio/openmeter/api/client/go"
	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

const (
	// ChargeFinalizer holds a Charge until its line is off OpenMeter's
	// invoice, or the charge has been billed.
	ChargeFinalizer = "openmeter.billing.miloapis.com/charge"

	chargeControllerName = "charge"

	// chargeDateLayout is how a charge's service period dates are written.
	chargeDateLayout = "2006-01-02"

	// Reasons on a Charge's Ready condition, and its events.
	ChargeReasonWaitingForInvoiceWriter = "WaitingForInvoiceWriter"
	ChargeReasonWaitingForCustomer      = "WaitingForCustomer"
	ChargeReasonWaitingForAmounts       = "WaitingForAmounts"
	ChargeReasonScheduled               = "Scheduled"
	ChargeReasonInvoiced                = "Invoiced"
	ChargeReasonRefused                 = "Refused"
	EventReasonChargeInvoiced           = "ChargeInvoiced"
	EventReasonChargeAlreadyInvoiced    = "ChargeAlreadyInvoiced"
	EventReasonChargeNotBilled          = "ChargeNotBilled"

	// chargeRecheckAfter is how often a scheduled charge is looked at for
	// being invoiced, besides whenever an Invoice in its namespace changes.
	chargeRecheckAfter = time.Hour
	// chargeWaitAfter is how soon a charge waiting on something that
	// doesn't trigger a reconcile of its own is looked at again.
	chargeWaitAfter = time.Minute
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
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=charges,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=charges/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=charges/finalizers,verbs=update
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=invoices,verbs=get;list;watch

// Reconcile schedules, follows and removes one Charge.
func (r *ChargeReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
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
		line, err := r.chargeLine(ctx, &charge, account, customerKey)
		if err != nil {
			return ctrl.Result{}, r.fail(ctx, &charge, err.Error())
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
		if err := r.OpenMeterClient.InvoiceChargeLineNow(ctx, customerID, ref.LineID); err != nil {
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

// chargeLine is the pending line for charge.
func (r *ChargeReconciler) chargeLine(ctx context.Context, charge *billingv1alpha1.Charge,
	account *billingv1alpha1.BillingAccount, customerKey openmeter.CustomerKey) (openmeter.ChargeLine, error) {
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

	now := r.now()
	invoiceAt := now
	if charge.Spec.Invoice != billingv1alpha1.ChargeInvoiceNow {
		end, ok, err := r.OpenMeterClient.CurrentBillingPeriodEnd(ctx, customerKey)
		if err != nil {
			return openmeter.ChargeLine{}, err
		}
		if !ok {
			end = nextBillingDay(account, now)
		}
		invoiceAt = end
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

// reconcileDelete takes the charge's line off OpenMeter's invoice before
// letting the charge go. When the account itself is being deleted, the
// line is left: the account's finalizer invoices pending lines before it
// deletes the customer, so a scheduled charge is billed, not lost.
func (r *ChargeReconciler) reconcileDelete(ctx context.Context, charge *billingv1alpha1.Charge, account *billingv1alpha1.BillingAccount) error {
	if !controllerutil.ContainsFinalizer(charge, ChargeFinalizer) {
		return nil
	}
	logger := log.FromContext(ctx)

	switch {
	case account == nil || !account.DeletionTimestamp.IsZero():
		if charge.Status.Phase == "" || charge.Status.Phase == billingv1alpha1.ChargePhasePending {
			r.event(charge, "Warning", EventReasonChargeNotBilled,
				"The charge wasn't billed: its billing account was deleted before the charge reached an invoice")
		}
	default:
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
				err := openmeter.ErrLineInvoiced
				if ref.Pending() {
					err = r.OpenMeterClient.RemoveChargeLine(ctx, ref.InvoiceID, ref.LineID)
				}
				switch {
				case errors.Is(err, openmeter.ErrLineInvoiced):
					r.event(charge, "Warning", EventReasonChargeAlreadyInvoiced,
						fmt.Sprintf("The charge was removed after it was invoiced on OpenMeter invoice %s; correct the invoice with a credit note", ref.InvoiceID))
				case err != nil:
					return err
				default:
					logger.Info("removed charge line", "invoice", ref.InvoiceID, "line", ref.LineID)
				}
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
		r.setReady(charge, s, metav1.ConditionFalse, reason, msg)
	})
}

// setScheduled records that the charge is on an invoice to come.
func (r *ChargeReconciler) setScheduled(ctx context.Context, charge *billingv1alpha1.Charge, invoiceAt time.Time) error {
	return r.patchStatus(ctx, charge, func(s *billingv1alpha1.ChargeStatus) {
		s.Phase = billingv1alpha1.ChargePhaseScheduled
		at := metav1.NewTime(invoiceAt.UTC().Truncate(time.Second))
		s.InvoiceAt = &at
		r.setReady(charge, s, metav1.ConditionTrue, ChargeReasonScheduled,
			fmt.Sprintf("On the invoice OpenMeter issues on %s", at.UTC().Format(time.RFC3339)))
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
		r.setReady(charge, s, metav1.ConditionTrue, ChargeReasonInvoiced, fmt.Sprintf("Invoiced on %s", name))
	})
	if err == nil {
		r.event(charge, "Normal", EventReasonChargeInvoiced, fmt.Sprintf("Invoiced on %s", name))
	}
	return ctrl.Result{}, err
}

// fail records that the charge can't be billed as it is.
func (r *ChargeReconciler) fail(ctx context.Context, charge *billingv1alpha1.Charge, msg string) error {
	r.event(charge, "Warning", ChargeReasonRefused, msg)
	return r.patchStatus(ctx, charge, func(s *billingv1alpha1.ChargeStatus) {
		s.Phase = billingv1alpha1.ChargePhaseFailed
		r.setReady(charge, s, metav1.ConditionFalse, ChargeReasonRefused, msg)
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
// writes the amounts.
func (r *ChargeReconciler) patchStatus(ctx context.Context, charge *billingv1alpha1.Charge, change func(*billingv1alpha1.ChargeStatus)) error {
	before := charge.DeepCopy()
	change(&charge.Status)
	if chargeStatusUnchanged(&before.Status, &charge.Status) {
		return nil
	}
	return r.Status().Patch(ctx, charge, client.MergeFrom(before))
}

func chargeStatusUnchanged(a, b *billingv1alpha1.ChargeStatus) bool {
	if a.Phase != b.Phase || !a.InvoiceAt.Equal(b.InvoiceAt) ||
		(a.InvoiceRef == nil) != (b.InvoiceRef == nil) ||
		(a.InvoiceRef != nil && a.InvoiceRef.Name != b.InvoiceRef.Name) {
		return false
	}
	x := apimeta.FindStatusCondition(a.Conditions, billingv1alpha1.ChargeConditionReady)
	y := apimeta.FindStatusCondition(b.Conditions, billingv1alpha1.ChargeConditionReady)
	if (x == nil) != (y == nil) {
		return false
	}
	return x == nil || (x.Status == y.Status && x.Reason == y.Reason && x.Message == y.Message)
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
