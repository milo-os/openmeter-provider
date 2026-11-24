package controller

import (
	"context"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
)

// mapChargeToAccount enqueues the billing account a charge is for.
func mapChargeToAccount(_ context.Context, obj client.Object) []reconcile.Request {
	charge, ok := obj.(*billingv1alpha1.Charge)
	if !ok || charge.Spec.BillingAccountRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: charge.Namespace, Name: charge.Spec.BillingAccountRef.Name,
	}}}
}

// chargeInvoicedNowPredicate passes a Charge update that puts an
// invoice-now charge on its invoice: its OnInvoice condition turning to
// Added, which the charge controller records right after OpenMeter makes
// the invoice. That invoice then waits on the invoice writer, which runs in
// the account's reconcile; without this it would wait for the account's
// next syncedRequeueAfter. Nothing else about a charge matters to the
// account reconcile.
func chargeInvoicedNowPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			newCharge, ok := e.ObjectNew.(*billingv1alpha1.Charge)
			if !ok || newCharge.Spec.Invoice != billingv1alpha1.ChargeInvoiceNow {
				return false
			}
			oldCharge, ok := e.ObjectOld.(*billingv1alpha1.Charge)
			if !ok {
				return false
			}
			return chargeAdded(newCharge) && !chargeAdded(oldCharge)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// chargeAdded reports whether the charge's line is on its invoice in
// OpenMeter.
func chargeAdded(charge *billingv1alpha1.Charge) bool {
	c := apimeta.FindStatusCondition(charge.Status.Conditions, billingv1alpha1.ChargeConditionOnInvoice)
	return c != nil && c.Reason == ChargeReasonAdded
}
