// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
)

// stripeCustomer is the resolved Stripe identity for a BillingAccount's
// default payment method.
type stripeCustomer struct {
	CustomerID      string
	PaymentMethodID string
}

// resolveStripeCustomer returns the Stripe customer/payment-method ids for
// the account's default payment method when
// DefaultPaymentMethodReady=True. Mirrors amberflo-provider's
// resolveStripeCustomerID resolution path exactly:
//
//	BA.spec.defaultPaymentMethodRef.name
//	  → PaymentMethod (must be Active)
//	  → StripePaymentMethod with the same name
//	  → status.stripeCustomerId / status.stripePaymentMethodId
//
// Returns a zero stripeCustomer (not an error) when the gate is not met or
// the Stripe ids are not yet available — callers must leave any
// previously-synced Stripe app data alone in that case rather than clear
// it, the same way amberflo-provider preserves ExtraTraits across a
// transient DefaultPaymentMethodReady flap.
func (r *BillingAccountReconciler) resolveStripeCustomer(
	ctx context.Context,
	account *billingv1alpha1.BillingAccount,
) (stripeCustomer, error) {
	if !apimeta.IsStatusConditionTrue(account.Status.Conditions, billingv1alpha1.BillingAccountConditionDefaultPaymentMethodReady) {
		return stripeCustomer{}, nil
	}
	if account.Spec.DefaultPaymentMethodRef == nil || account.Spec.DefaultPaymentMethodRef.Name == "" {
		return stripeCustomer{}, nil
	}

	pmName := account.Spec.DefaultPaymentMethodRef.Name
	ns := account.Namespace

	var pm billingv1alpha1.PaymentMethod
	if err := r.Get(ctx, types.NamespacedName{Name: pmName, Namespace: ns}, &pm); err != nil {
		if apierrors.IsNotFound(err) {
			return stripeCustomer{}, nil
		}
		return stripeCustomer{}, fmt.Errorf("get PaymentMethod %s/%s: %w", ns, pmName, err)
	}
	if pm.Status.Phase != billingv1alpha1.PaymentMethodPhaseActive {
		return stripeCustomer{}, nil
	}

	// Looked up by the PaymentMethod's own name, because stripe-provider
	// creates the child with Name: pm.Name (verified in its
	// PaymentMethodWatcher). That's a cross-repo coupling rather than a
	// contract — spec.paymentMethodRef is the authoritative link, and is
	// what mapStripePaymentMethodToAccount walks in the other direction.
	// If stripe-provider ever switches to generated names this lookup goes
	// quietly NotFound and Stripe app data simply never syncs, so the miss
	// is logged rather than swallowed.
	var spm stripev1alpha1.StripePaymentMethod
	if err := r.Get(ctx, types.NamespacedName{Name: pmName, Namespace: ns}, &spm); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info(
				"no StripePaymentMethod found for the account's default PaymentMethod; skipping Stripe app-data sync",
				"paymentMethod", pmName, "namespace", ns)
			return stripeCustomer{}, nil
		}
		return stripeCustomer{}, fmt.Errorf("get StripePaymentMethod %s/%s: %w", ns, pmName, err)
	}
	if spm.Status.StripeCustomerID == "" {
		return stripeCustomer{}, nil
	}
	return stripeCustomer{
		CustomerID:      spm.Status.StripeCustomerID,
		PaymentMethodID: spm.Status.StripePaymentMethodID,
	}, nil
}

// mapPaymentMethodToAccount enqueues the parent BillingAccount when a
// PaymentMethod referencing it changes (e.g. phase reaching Active).
func mapPaymentMethodToAccount(_ context.Context, obj client.Object) []reconcile.Request {
	pm, ok := obj.(*billingv1alpha1.PaymentMethod)
	if !ok {
		return nil
	}
	if pm.Spec.BillingAccountRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Name:      pm.Spec.BillingAccountRef.Name,
			Namespace: pm.Namespace,
		},
	}}
}

// mapStripePaymentMethodToAccount resolves the parent PaymentMethod, then
// enqueues its BillingAccount so a stripeCustomerId appearing on
// StripePaymentMethod.status re-triggers Stripe app-data sync without
// waiting for an unrelated BillingAccount write.
func (r *BillingAccountReconciler) mapStripePaymentMethodToAccount(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	spm, ok := obj.(*stripev1alpha1.StripePaymentMethod)
	if !ok {
		return nil
	}
	pmName := spm.Spec.PaymentMethodRef.Name
	if pmName == "" {
		return nil
	}
	var pm billingv1alpha1.PaymentMethod
	if err := r.Get(ctx, types.NamespacedName{Name: pmName, Namespace: spm.Namespace}, &pm); err != nil {
		return nil
	}
	if pm.Spec.BillingAccountRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Name:      pm.Spec.BillingAccountRef.Name,
			Namespace: pm.Namespace,
		},
	}}
}
