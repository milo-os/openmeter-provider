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

	"github.com/go-logr/logr"
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

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

const (
	// SubscriptionFinalizer blocks deletion of a BillingEntitlement until
	// the customer's OpenMeter subscription is canceled (or handed over to
	// the account's next entitlement).
	SubscriptionFinalizer            = "openmeter.miloapis.com/subscription"
	billingEntitlementControllerName = "billingentitlement"

	// ConditionTypeOpenMeterSubscriptionSynced reports whether the billing
	// account's OpenMeter customer is subscribed to the entitled Offer's
	// plan. Owned by this provider; billing owns Ready and OfferAssigned.
	ConditionTypeOpenMeterSubscriptionSynced = "OpenMeterSubscriptionSynced"

	conditionReasonBillingAccountNotFound  = "BillingAccountNotFound"
	conditionReasonBillingAccountDeleting  = "BillingAccountDeleting"
	conditionReasonOfferNotFound           = "OfferNotFound"
	conditionReasonOfferNotAssignable      = "OfferNotAssignable"
	conditionReasonCustomerNotSynced       = "CustomerNotSynced"
	conditionReasonCustomerBillingNotReady = "CustomerBillingNotReady"
	conditionReasonPlanNotPublished        = "PlanNotPublished"
	conditionReasonMigrationScheduled      = "MigrationScheduled"
)

// Metadata written on the subscriptions this controller creates, so they can
// be traced back from OpenMeter.
const (
	subscriptionMetadataEntitlement    = "miloapis.com/billing-entitlement"
	subscriptionMetadataBillingAccount = "miloapis.com/billing-account"
	subscriptionMetadataOffer          = "miloapis.com/offer-name"
)

const (
	// customerBillingNotReadyRequeueAfter paces retries while the account
	// has no payment method. The BillingAccount watch usually wins; the
	// Stripe app data it syncs does not change the account object, though.
	customerBillingNotReadyRequeueAfter = 30 * time.Second
	// subscriptionResyncAfter re-checks a converged subscription, which can
	// be changed in OpenMeter directly.
	subscriptionResyncAfter = 30 * time.Minute
)

// BillingEntitlementReconciler subscribes a billing account's OpenMeter
// customer to the plan of the Offer its BillingEntitlement references.
//
// The OpenMeter customer (key: BillingAccount UID) is created by the
// BillingAccountReconciler, the plan (key: Offer UID) by the
// OfferReconciler; this controller only links them. All subscription state
// lives in OpenMeter and is re-read every reconcile: billing allows one
// non-deleting entitlement per account, and OpenMeter one live subscription
// per customer, so "the customer's subscription" is unambiguous.
//
//   - offerRef switched: the subscription changes plan immediately.
//   - Offer re-published as a new plan version: the subscription migrates
//     immediately (see openmeter.nextSubscriptionStep for why not at the
//     next billing cycle).
//   - Entitlement deleted: the subscription is canceled immediately, unless
//     another entitlement for the same account exists to take it over.
//   - BillingAccount deleting: the subscription is canceled, because
//     OpenMeter refuses to delete a customer with live subscriptions and the
//     account's own finalizer would otherwise never complete.
type BillingEntitlementReconciler struct {
	client.Client
	OpenMeterClient openmeter.Client
	Recorder        record.EventRecorder
	Log             logr.Logger
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingentitlements,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingentitlements/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingentitlements/finalizers,verbs=update
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=offers,verbs=get;list;watch

// Reconcile syncs a single BillingEntitlement.
func (r *BillingEntitlementReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("billingEntitlement", req.Name, "namespace", req.Namespace)

	var be billingv1alpha1.BillingEntitlement
	if err := r.Get(ctx, req.NamespacedName, &be); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	logger = logger.WithValues("billingAccount", be.Spec.BillingAccountRef.Name, "offer", be.Spec.OfferRef.Name)

	if !be.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, logger, &be)
	}

	if !controllerutil.ContainsFinalizer(&be, SubscriptionFinalizer) {
		controllerutil.AddFinalizer(&be, SubscriptionFinalizer)
		if err := r.Update(ctx, &be); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	var account billingv1alpha1.BillingAccount
	if err := r.Get(ctx, types.NamespacedName{Namespace: be.Namespace, Name: be.Spec.BillingAccountRef.Name}, &account); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get billing account: %w", err)
		}
		// The BillingAccount watch requeues as soon as it appears. A deleted
		// account has no customer left (OpenMeter refuses to delete one with
		// live subscriptions), so there is nothing to cancel.
		return r.syncFailed(ctx, &be, conditionReasonBillingAccountNotFound,
			fmt.Errorf("BillingAccount %q not found", be.Spec.BillingAccountRef.Name), 0)
	}
	customerKey := openmeter.CustomerKey(account.UID)
	logger = logger.WithValues("customerKey", customerKey)

	if !account.DeletionTimestamp.IsZero() {
		return r.cancelForDeletingAccount(ctx, logger, &be, customerKey)
	}

	var offer billingv1alpha1.Offer
	if err := r.Get(ctx, types.NamespacedName{Name: be.Spec.OfferRef.Name}, &offer); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get offer: %w", err)
		}
		// The existing subscription (if any) is left running: the Offer may
		// be recreated, and canceling on a missing reference would stop
		// billing on what may be a transient cache miss.
		return r.syncFailed(ctx, &be, conditionReasonOfferNotFound,
			fmt.Errorf("offer %q not found", be.Spec.OfferRef.Name), 0)
	}
	if offer.Spec.LaunchStage != billingv1alpha1.OfferLaunchStageGA || len(offer.Spec.ServicePricings) == 0 {
		// Billing's webhook only admits assignable Offers, and an assignable
		// Offer never stops being one; this guards against a bypassed webhook.
		return r.syncFailed(ctx, &be, conditionReasonOfferNotAssignable,
			fmt.Errorf("offer %q is not GA with a servicePricings snapshot", offer.Name), 0)
	}

	planKey := planKeyForOffer(&offer)
	logger = logger.WithValues("planKey", planKey)

	state, err := r.OpenMeterClient.EnsureSubscription(ctx, openmeter.DesiredSubscription{
		CustomerKey: customerKey,
		PlanKey:     planKey,
		Metadata: map[string]string{
			subscriptionMetadataEntitlement:    be.Namespace + "/" + be.Name,
			subscriptionMetadataBillingAccount: account.Name,
			subscriptionMetadataOffer:          offer.Name,
		},
	})
	if err != nil {
		switch {
		case errors.Is(err, openmeter.ErrCustomerNotFound):
			logger.Info("waiting for the OpenMeter customer")
			return r.syncFailed(ctx, &be, conditionReasonCustomerNotSynced,
				fmt.Errorf("OpenMeter customer for BillingAccount %q does not exist yet", account.Name), transientRequeueAfter)
		case errors.Is(err, openmeter.ErrPlanNotPublished):
			logger.Info("waiting for the Offer's OpenMeter plan")
			return r.syncFailed(ctx, &be, conditionReasonPlanNotPublished, planNotPublishedError(&offer), transientRequeueAfter)
		case errors.Is(err, openmeter.ErrCustomerBillingNotReady):
			logger.Info("customer cannot be invoiced yet", "err", err.Error())
			return r.syncFailed(ctx, &be, conditionReasonCustomerBillingNotReady,
				fmt.Errorf("OpenMeter cannot invoice BillingAccount %q yet (does it have a default payment method?): %w", account.Name, err),
				customerBillingNotReadyRequeueAfter)
		}
		return r.openMeterFailed(ctx, logger, &be, fmt.Errorf("ensure subscription: %w", err))
	}

	reason, msg := conditionReasonSynced, subscriptionMessage(state)
	if state.Pending != nil {
		reason = conditionReasonMigrationScheduled
	}
	changed, err := r.setSyncedCondition(ctx, &be, metav1.ConditionTrue, reason, msg)
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		logger.Info("reconciled billing entitlement subscription", "subscriptionID", state.Current.Id)
		r.event(&be, "Normal", EventReasonSynced, msg)
	}
	// Subscriptions can be changed in OpenMeter directly; the resync
	// restores the entitled one without waiting for a Kubernetes event.
	return ctrl.Result{RequeueAfter: subscriptionResyncAfter}, nil
}

// subscriptionMessage describes a converged subscription for the condition.
func subscriptionMessage(state openmeter.SubscriptionState) string {
	cur := state.Current
	planKey, version := "", 0
	if cur.Plan != nil {
		planKey, version = cur.Plan.Key, cur.Plan.Version
	}
	msg := fmt.Sprintf("OpenMeter subscription %s on plan %s version %d is %s", cur.Id, planKey, version, cur.Status)
	if p := state.Pending; p != nil && p.Plan != nil {
		msg += fmt.Sprintf("; migrating to version %d at %s (subscription %s)",
			p.Plan.Version, p.ActiveFrom.UTC().Format(time.RFC3339), p.Id)
	}
	return msg
}

// planNotPublishedError explains why the Offer has no plan to subscribe to,
// quoting the OfferReconciler's condition when it has failed.
func planNotPublishedError(offer *billingv1alpha1.Offer) error {
	if c := apimeta.FindStatusCondition(offer.Status.Conditions, ConditionTypeOpenMeterPlanSynced); c != nil && c.Status == metav1.ConditionFalse {
		return fmt.Errorf("offer %q has no published OpenMeter plan: %s: %s", offer.Name, c.Reason, c.Message)
	}
	return fmt.Errorf("offer %q has no published OpenMeter plan yet", offer.Name)
}

// cancelForDeletingAccount cancels the customer's subscriptions so the
// BillingAccount's finalizer can delete the OpenMeter customer.
func (r *BillingEntitlementReconciler) cancelForDeletingAccount(
	ctx context.Context,
	logger logr.Logger,
	be *billingv1alpha1.BillingEntitlement,
	customerKey openmeter.CustomerKey,
) (ctrl.Result, error) {
	if err := r.OpenMeterClient.CancelSubscriptions(ctx, customerKey); err != nil {
		return r.openMeterFailed(ctx, logger, be, fmt.Errorf("cancel subscriptions of deleting BillingAccount: %w", err))
	}
	return r.syncFailed(ctx, be, conditionReasonBillingAccountDeleting,
		fmt.Errorf("BillingAccount %q is being deleted; OpenMeter subscriptions canceled", be.Spec.BillingAccountRef.Name), 0)
}

// reconcileDelete cancels the customer's subscription and releases the
// finalizer. When another (non-deleting) entitlement exists for the same
// account, the subscription is left for it to change to its own Offer:
// canceling first would end the period and split the invoice for nothing.
func (r *BillingEntitlementReconciler) reconcileDelete(
	ctx context.Context,
	logger logr.Logger,
	be *billingv1alpha1.BillingEntitlement,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(be, SubscriptionFinalizer) {
		return ctrl.Result{}, nil
	}

	successor, err := r.successorEntitlement(ctx, be)
	if err != nil {
		return ctrl.Result{}, err
	}

	var account billingv1alpha1.BillingAccount
	err = r.Get(ctx, types.NamespacedName{Namespace: be.Namespace, Name: be.Spec.BillingAccountRef.Name}, &account)
	switch {
	case apierrors.IsNotFound(err):
		// No account means its customer was deleted, which OpenMeter only
		// allows once no subscription is live.
		logger.Info("BillingAccount gone; nothing to cancel")
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("get billing account: %w", err)
	case successor != "":
		logger.Info("leaving subscription to the account's next entitlement", "successor", successor)
		r.event(be, "Normal", EventReasonDeleted,
			fmt.Sprintf("OpenMeter subscription left to BillingEntitlement %s", successor))
	default:
		customerKey := openmeter.CustomerKey(account.UID)
		if err := r.OpenMeterClient.CancelSubscriptions(ctx, customerKey); err != nil {
			// Releasing the finalizer would keep billing the customer for an
			// Offer it is no longer entitled to.
			requeue := transientRequeueAfter
			if openmeter.IsPermanent(err) {
				requeue = permanentRequeueAfter
			}
			logger.Error(err, "cancel subscriptions; keeping finalizer", "requeueAfter", requeue.String())
			r.event(be, "Warning", EventReasonDeleteFailed, fmt.Sprintf("cancel OpenMeter subscription: %v", err))
			return ctrl.Result{RequeueAfter: requeue}, nil
		}
		logger.Info("OpenMeter subscription canceled")
		r.event(be, "Normal", EventReasonDeleted, fmt.Sprintf("OpenMeter subscription of customer %s canceled", customerKey))
	}

	controllerutil.RemoveFinalizer(be, SubscriptionFinalizer)
	if err := r.Update(ctx, be); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// successorEntitlement returns the name of another non-deleting
// BillingEntitlement for the same account, or "" when there is none.
func (r *BillingEntitlementReconciler) successorEntitlement(ctx context.Context, be *billingv1alpha1.BillingEntitlement) (string, error) {
	var list billingv1alpha1.BillingEntitlementList
	if err := r.List(ctx, &list,
		client.InNamespace(be.Namespace),
		client.MatchingFields{EntitlementBillingAccountRefField: be.Spec.BillingAccountRef.Name},
	); err != nil {
		return "", fmt.Errorf("list billing entitlements for account: %w", err)
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.UID != be.UID && other.DeletionTimestamp.IsZero() {
			return other.Name, nil
		}
	}
	return "", nil
}

// openMeterFailed classifies an OpenMeter error into a requeue schedule.
func (r *BillingEntitlementReconciler) openMeterFailed(
	ctx context.Context,
	logger logr.Logger,
	be *billingv1alpha1.BillingEntitlement,
	err error,
) (ctrl.Result, error) {
	if openmeter.IsPermanent(err) {
		logger.Error(err, "OpenMeter permanent failure", "requeueAfter", permanentRequeueAfter.String())
		return r.syncFailed(ctx, be, conditionReasonOpenMeterError,
			fmt.Errorf("%s: %w", syncReasonPermanent, err), permanentRequeueAfter)
	}
	logger.Info("OpenMeter transient failure", "err", err.Error(), "requeueAfter", transientRequeueAfter.String())
	return r.syncFailed(ctx, be, conditionReasonOpenMeterError,
		fmt.Errorf("%s: %w", syncReasonTransient, err), transientRequeueAfter)
}

// syncFailed records a failed sync and schedules a retry (none when
// requeueAfter is zero: a watch brings the entitlement back). The Warning
// event is only emitted when the condition changes.
func (r *BillingEntitlementReconciler) syncFailed(
	ctx context.Context,
	be *billingv1alpha1.BillingEntitlement,
	reason string,
	cause error,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	changed, err := r.setSyncedCondition(ctx, be, metav1.ConditionFalse, reason, cause.Error())
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		r.event(be, "Warning", EventReasonSyncFailed, fmt.Sprintf("%s: %v", reason, cause))
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// setSyncedCondition patches the OpenMeterSubscriptionSynced condition when
// it changes, with an optimistic lock so a concurrent status write by
// billing's controller is never clobbered.
func (r *BillingEntitlementReconciler) setSyncedCondition(
	ctx context.Context,
	be *billingv1alpha1.BillingEntitlement,
	status metav1.ConditionStatus,
	reason, message string,
) (bool, error) {
	base := be.DeepCopy()
	changed := apimeta.SetStatusCondition(&be.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeOpenMeterSubscriptionSynced,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: be.Generation,
	})
	if !changed {
		return false, nil
	}
	if err := r.Status().Patch(ctx, be, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, fmt.Errorf("patch billing entitlement status: %w", err)
	}
	return true, nil
}

func (r *BillingEntitlementReconciler) event(be *billingv1alpha1.BillingEntitlement, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(be, eventType, reason, message)
	}
}

// entitlementsForOffer enqueues the entitlements referencing an Offer, so a
// newly published plan (or plan version) reaches its subscribers.
func (r *BillingEntitlementReconciler) entitlementsForOffer(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.listRequests(ctx, client.MatchingFields{EntitlementOfferRefField: obj.GetName()})
}

// entitlementsForAccount enqueues the entitlements of a BillingAccount: its
// customer appearing, becoming billable, or being deleted all change what
// the entitlement can do.
func (r *BillingEntitlementReconciler) entitlementsForAccount(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.listRequests(ctx,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{EntitlementBillingAccountRefField: obj.GetName()})
}

// siblingEntitlements enqueues the other entitlements of the same account.
// When an entitlement is deleted while its successor already exists, the
// successor may have observed the old subscription as converged before the
// deletion canceled it; requeueing it restores the subscription.
func (r *BillingEntitlementReconciler) siblingEntitlements(ctx context.Context, obj client.Object) []reconcile.Request {
	be, ok := obj.(*billingv1alpha1.BillingEntitlement)
	if !ok {
		return nil
	}
	var reqs []reconcile.Request
	for _, req := range r.listRequests(ctx,
		client.InNamespace(be.Namespace),
		client.MatchingFields{EntitlementBillingAccountRefField: be.Spec.BillingAccountRef.Name}) {
		if req.Name != be.Name {
			reqs = append(reqs, req)
		}
	}
	return reqs
}

func (r *BillingEntitlementReconciler) listRequests(ctx context.Context, opts ...client.ListOption) []reconcile.Request {
	var list billingv1alpha1.BillingEntitlementList
	if err := r.List(ctx, &list, opts...); err != nil {
		log.FromContext(ctx).Error(err, "list BillingEntitlements for watch")
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: list.Items[i].Namespace,
			Name:      list.Items[i].Name,
		}})
	}
	return reqs
}

// SetupWithManager registers the BillingEntitlement reconciler. Its field
// indexes are installed by AddIndexers.
func (r *BillingEntitlementReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("openmeter-provider") //nolint:staticcheck // SA1019: GetEventRecorder (events/v1) is a larger migration.
	}
	if r.Log.GetSink() == nil {
		r.Log = mgr.GetLogger().WithName("billingentitlement-controller")
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(billingEntitlementControllerName).
		For(&billingv1alpha1.BillingEntitlement{}).
		Watches(&billingv1alpha1.BillingEntitlement{}, handler.EnqueueRequestsFromMapFunc(r.siblingEntitlements)).
		Watches(&billingv1alpha1.Offer{}, handler.EnqueueRequestsFromMapFunc(r.entitlementsForOffer)).
		Watches(&billingv1alpha1.BillingAccount{}, handler.EnqueueRequestsFromMapFunc(r.entitlementsForAccount)).
		Complete(r)
}
