// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

// stripeCustomer is the resolved Stripe identity for a BillingAccount's
// default payment method.
type stripeCustomer struct {
	CustomerID      openmeter.StripeCustomerID
	PaymentMethodID string
	// NoPaymentMethod reports that the account definitely has no usable
	// default payment method right now: no defaultPaymentMethodRef,
	// DefaultPaymentMethodReady=False, or the referenced PaymentMethod/
	// StripePaymentMethod gone or not Active. Distinct from the zero value
	// with this false, which means "not known yet" (condition not reported,
	// or Unknown; StripePaymentMethod not reporting its ids yet).
	NoPaymentMethod bool
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
// Returns no ids (not an error) when the gate is not met or the Stripe ids
// are not yet available, with NoPaymentMethod telling apart "there is no
// usable payment method" (the stored default payment method must be
// cleared: stripe-provider detaches the card when its StripePaymentMethod
// goes away, and OpenMeter would keep putting it on every Stripe invoice)
// from "not known yet" (whatever was synced is left alone, the way
// amberflo-provider preserves ExtraTraits across a transient flap).
func (r *BillingAccountReconciler) resolveStripeCustomer(
	ctx context.Context,
	account *billingv1alpha1.BillingAccount,
) (stripeCustomer, error) {
	none := stripeCustomer{NoPaymentMethod: true}
	if account.Spec.DefaultPaymentMethodRef == nil || account.Spec.DefaultPaymentMethodRef.Name == "" {
		return none, nil
	}
	ready := apimeta.FindStatusCondition(account.Status.Conditions, billingv1alpha1.BillingAccountConditionDefaultPaymentMethodReady)
	switch {
	case ready == nil || ready.Status == metav1.ConditionUnknown:
		return stripeCustomer{}, nil
	case ready.Status == metav1.ConditionFalse:
		return none, nil
	}

	pmName := account.Spec.DefaultPaymentMethodRef.Name
	ns := account.Namespace

	var pm billingv1alpha1.PaymentMethod
	if err := r.Get(ctx, types.NamespacedName{Name: pmName, Namespace: ns}, &pm); err != nil {
		if apierrors.IsNotFound(err) {
			return none, nil
		}
		return stripeCustomer{}, fmt.Errorf("get PaymentMethod %s/%s: %w", ns, pmName, err)
	}
	if pm.Status.Phase != billingv1alpha1.PaymentMethodPhaseActive {
		return none, nil
	}

	// Looked up by the PaymentMethod's own name, because stripe-provider
	// creates the child with Name: pm.Name (verified in its
	// PaymentMethodWatcher). That's a cross-repo coupling rather than a
	// contract — spec.paymentMethodRef is the authoritative link, and is
	// what mapStripePaymentMethodToAccount walks in the other direction.
	// If stripe-provider ever switches to generated names this lookup goes
	// quietly NotFound and Stripe app data simply never syncs, so the miss
	// is logged rather than swallowed. A missing StripePaymentMethod behind
	// an Active PaymentMethod otherwise means it was deleted — and
	// stripe-provider detaches the card when it is — so it counts as no
	// payment method.
	var spm stripev1alpha1.StripePaymentMethod
	if err := r.Get(ctx, types.NamespacedName{Name: pmName, Namespace: ns}, &spm); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info(
				"no StripePaymentMethod found for the account's default PaymentMethod; treating it as no payment method",
				"paymentMethod", pmName, "namespace", ns)
			return none, nil
		}
		return stripeCustomer{}, fmt.Errorf("get StripePaymentMethod %s/%s: %w", ns, pmName, err)
	}
	if spm.Status.StripeCustomerID == "" {
		return stripeCustomer{}, nil
	}
	return stripeCustomer{
		CustomerID:      openmeter.StripeCustomerID(spm.Status.StripeCustomerID),
		PaymentMethodID: spm.Status.StripePaymentMethodID,
	}, nil
}

// reconcileStripeAppData converges the OpenMeter customer's Stripe app data
// with the account's default payment method, and returns a short summary of
// the resulting state for the Synced fingerprint.
//
//   - A resolved Stripe customer is upserted (with its payment method, or
//     none — which clears a stored one).
//   - No usable default payment method, while OpenMeter still stores one:
//     re-upsert the stored Stripe customer without it. Otherwise OpenMeter
//     keeps putting a detached card on every Stripe invoice
//     (app/stripe/appinvoice.go uses the stored id as the invoice's
//     default_payment_method).
//   - "Not known yet" leaves whatever is stored alone.
//
// Two conditions are reported (log + one Warning event while they persist)
// rather than failed, since nothing this controller does resolves them and
// failing would suppress the Synced event and back off the other syncs:
//
//   - The customer's billing profile has no Stripe app (e.g. a Sandbox
//     install): StripeAppNotConfigured.
//   - The account is billed by invoice (an active Invoice
//     BillingArrangement) but OpenMeter has no Stripe customer for it:
//     StripeCustomerMissing. OpenMeter's Stripe app can't invoice a
//     customer without Stripe app data ("customer has no data for stripe
//     app"), and the only Stripe customer id this controller can learn comes
//     from a StripePaymentMethod, which an invoice-billed account need not
//     have. OpenMeter can mint a Stripe customer itself, but only as a side
//     effect of creating a Stripe Checkout session, and that customer would
//     lack stripe-provider's metadata.billing_account tag — stripe-provider
//     would then create a second Stripe customer for the same account as
//     soon as a payment method is added. So this is surfaced, not papered
//     over: the Stripe customer id has to be published independently of
//     payment methods (by stripe-provider) for these accounts to invoice.
func (r *BillingAccountReconciler) reconcileStripeAppData(
	ctx context.Context,
	logger logr.Logger,
	account *billingv1alpha1.BillingAccount,
	customerID openmeter.CustomerID,
) (string, error) {
	prefix := noticePrefix(types.NamespacedName{Namespace: account.Namespace, Name: account.Name})
	notConfiguredKey := prefix + EventReasonStripeAppNotConfigured
	missingKey := prefix + EventReasonStripeCustomerMissing

	stripe, err := r.resolveStripeCustomer(ctx, account)
	if err != nil {
		return "", fmt.Errorf("resolve stripe customer: %w", err)
	}

	if stripe.CustomerID != "" {
		err := r.OpenMeterClient.EnsureCustomerStripeAppData(ctx, customerID, stripe.CustomerID, stripe.PaymentMethodID)
		switch {
		case errors.Is(err, openmeter.ErrStripeAppNotConfigured):
			r.reportStripeAppNotConfigured(logger, account, notConfiguredKey, err)
			return "stripe-app-not-configured", nil
		case err != nil:
			return "", fmt.Errorf("ensure customer stripe app data: %w", err)
		}
		r.notices.forget(notConfiguredKey)
		r.notices.forget(missingKey)
		return fmt.Sprintf("stripe:%s/%s", stripe.CustomerID, stripe.PaymentMethodID), nil
	}

	invoiceTerms := hasInvoiceArrangement(account)
	if !invoiceTerms {
		r.notices.forget(missingKey)
	}
	if !stripe.NoPaymentMethod && !invoiceTerms {
		// Not known yet: keep whatever is stored.
		return "pending", nil
	}

	current, err := r.OpenMeterClient.GetCustomerStripeAppData(ctx, customerID)
	switch {
	case errors.Is(err, openmeter.ErrStripeAppNotConfigured):
		r.reportStripeAppNotConfigured(logger, account, notConfiguredKey, err)
		return "stripe-app-not-configured", nil
	case errors.Is(err, openmeter.ErrStripeAppDataNotFound):
		r.notices.forget(notConfiguredKey)
		if invoiceTerms {
			msg := fmt.Sprintf(
				"account is billed by invoice (BillingArrangement %s) but OpenMeter has no Stripe customer for it, so OpenMeter cannot invoice it through Stripe; a Stripe customer id must be published for the account (stripe-provider only exposes one on a StripePaymentMethod)",
				account.Status.PaymentArrangement.Name)
			logger.Info("Stripe customer missing for an invoice-billed account", "arrangement", account.Status.PaymentArrangement.Name)
			if r.notices.changed(missingKey, msg) && r.Recorder != nil {
				r.Recorder.Event(account, "Warning", EventReasonStripeCustomerMissing, msg)
			}
			return "stripe-customer-missing", nil
		}
		return "none", nil
	case err != nil:
		return "", fmt.Errorf("get customer stripe app data: %w", err)
	}
	r.notices.forget(notConfiguredKey)
	r.notices.forget(missingKey)

	if stripe.NoPaymentMethod && current.StripeDefaultPaymentMethodId != nil && *current.StripeDefaultPaymentMethodId != "" {
		logger.Info("clearing the stale Stripe default payment method: the account has no usable default payment method",
			"stripeCustomerId", current.StripeCustomerId, "stripePaymentMethodId", *current.StripeDefaultPaymentMethodId)
		if err := r.OpenMeterClient.EnsureCustomerStripeAppData(ctx, customerID,
			openmeter.StripeCustomerID(current.StripeCustomerId), ""); err != nil {
			if errors.Is(err, openmeter.ErrStripeAppNotConfigured) {
				r.reportStripeAppNotConfigured(logger, account, notConfiguredKey, err)
				return "stripe-app-not-configured", nil
			}
			return "", fmt.Errorf("clear stale stripe default payment method: %w", err)
		}
		return fmt.Sprintf("stripe:%s/", current.StripeCustomerId), nil
	}
	pm := ""
	if current.StripeDefaultPaymentMethodId != nil {
		pm = *current.StripeDefaultPaymentMethodId
	}
	return fmt.Sprintf("stripe:%s/%s", current.StripeCustomerId, pm), nil
}

// reportStripeAppNotConfigured logs and, once while it persists, emits a
// Warning event for openmeter.ErrStripeAppNotConfigured.
func (r *BillingAccountReconciler) reportStripeAppNotConfigured(
	logger logr.Logger,
	account *billingv1alpha1.BillingAccount,
	key string,
	err error,
) {
	logger.Info("customer's OpenMeter billing profile has no Stripe app; skipping Stripe app-data sync", "err", err.Error())
	if r.notices.changed(key, err.Error()) && r.Recorder != nil {
		r.Recorder.Event(account, "Warning", EventReasonStripeAppNotConfigured,
			"Stripe app data not synced: the customer's OpenMeter billing profile has no Stripe app (the org default billing profile must use the Stripe app)")
	}
}

// hasInvoiceArrangement reports whether the account is currently billed by
// invoice: billing publishes the active BillingArrangement on
// status.paymentArrangement and clears it when none is active.
func hasInvoiceArrangement(account *billingv1alpha1.BillingAccount) bool {
	arr := account.Status.PaymentArrangement
	return arr != nil && arr.Type == billingv1alpha1.BillingArrangementTypeInvoice
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
