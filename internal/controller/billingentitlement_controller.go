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
	conditionReasonContractNotStarted      = "ContractNotStarted"
	conditionReasonOfferChangeScheduled    = "OfferChangeScheduled"
)

// ContractStartedAnnotation records on a BillingEntitlement that its
// account has been moved onto its contract, with the contract's start as
// the value. From then on a change of offerRef amends the contract, and
// takes effect at the end of the billing period, rather than moving the
// account onto it there and then.
const ContractStartedAnnotation = "openmeter.miloapis.com/contractStartedAt"

// OfferChangeAtAnnotation records on a BillingEntitlement when an amendment
// to its contract is due (RFC 3339): the end of the billing period it was
// made in. A change that misses that boundary is made as soon as it can,
// rather than a period late. Removed once the change is made.
const OfferChangeAtAnnotation = "openmeter.miloapis.com/offerChangeAt"

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
//   - Contract starting later: nothing is subscribed until its start date,
//     then the subscription bills on the contract's day of the month. An
//     account moving onto a contract keeps its subscription until then, and
//     changes plan and billing day on the start date.
//   - offerRef switched on a started contract: the subscription changes
//     plan at the end of its billing period, so each invoice carries one
//     price.
type BillingEntitlementReconciler struct {
	client.Client
	OpenMeterClient openmeter.Client
	Recorder        record.EventRecorder
	Log             logr.Logger
	// Now returns the current time; time.Now when unset.
	Now func() time.Time
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
		return r.syncWaiting(ctx, &be, conditionReasonBillingAccountNotFound,
			fmt.Errorf("BillingAccount %q not found", be.Spec.BillingAccountRef.Name), 0)
	}
	customerKey := openmeter.CustomerKey(account.UID)
	logger = logger.WithValues("customerKey", customerKey)

	if !account.DeletionTimestamp.IsZero() {
		return r.cancelForDeletingAccount(ctx, &be, customerKey)
	}

	var offer billingv1alpha1.Offer
	if err := r.Get(ctx, types.NamespacedName{Name: be.Spec.OfferRef.Name}, &offer); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get offer: %w", err)
		}
		// The existing subscription (if any) is left running: the Offer may
		// be recreated, and canceling on a missing reference would stop
		// billing on what may be a transient cache miss.
		return r.syncWaiting(ctx, &be, conditionReasonOfferNotFound,
			fmt.Errorf("offer %q not found", be.Spec.OfferRef.Name), 0)
	}
	if offer.Spec.LaunchStage != billingv1alpha1.OfferLaunchStageGA || len(offer.Spec.ServicePricings) == 0 {
		// Billing's webhook only admits assignable Offers, and an assignable
		// Offer never stops being one; this guards against a bypassed webhook.
		return r.syncWaiting(ctx, &be, conditionReasonOfferNotAssignable,
			fmt.Errorf("offer %q is not GA with a servicePricings snapshot", offer.Name), 0)
	}

	planKey := planKeyForOffer(&offer)
	logger = logger.WithValues("planKey", planKey)

	now := r.now()
	anchor := subscriptionBillingAnchor(&account)
	desired := openmeter.DesiredSubscription{
		CustomerKey: customerKey,
		PlanKey:     planKey,
		Metadata: map[string]string{
			subscriptionMetadataEntitlement:    be.Namespace + "/" + be.Name,
			subscriptionMetadataBillingAccount: account.Name,
			subscriptionMetadataOffer:          offer.Name,
		},
		Now: now,
	}
	contractStarted := false
	if contract := be.Spec.Contract; contract != nil {
		// OpenMeter can't start a subscription in the past, and a scheduled
		// one can't be deleted cleanly (see nextSubscriptionStep), so a
		// contract starting later is subscribed on the day it starts. An
		// account already subscribed keeps that subscription until then.
		// Billing only admits contracts starting today or later.
		startsAt := contract.StartsAt.UTC()
		if wait := startsAt.Sub(now); wait > 0 {
			return r.syncWaiting(ctx, &be, conditionReasonContractNotStarted,
				fmt.Errorf("the contract starts at %s, when the account is subscribed to its Offer", startsAt.Format(time.RFC3339)), wait)
		}
		anchor = startsAt
		// Moving onto the contract changes plan at once and bills on the
		// contract's day; once moved, a change amends the contract and waits
		// for the end of the billing period.
		_, contractStarted = be.Annotations[ContractStartedAnnotation]
		desired.MoveAnchor = !contractStarted
		desired.ChangeAtPeriodEnd = contractStarted
		if due, err := time.Parse(time.RFC3339, be.Annotations[OfferChangeAtAnnotation]); err == nil {
			desired.ChangeDue = &due
		}
	}
	desired.BillingAnchor = &anchor
	state, err := r.OpenMeterClient.EnsureSubscription(ctx, desired)
	if err != nil {
		switch {
		case errors.Is(err, openmeter.ErrCustomerNotFound):
			logger.Info("waiting for the OpenMeter customer")
			return r.syncWaiting(ctx, &be, conditionReasonCustomerNotSynced,
				fmt.Errorf("OpenMeter customer for BillingAccount %q does not exist yet", account.Name), transientRequeueAfter)
		case errors.Is(err, openmeter.ErrPlanNotPublished):
			logger.Info("waiting for the Offer's OpenMeter plan")
			return r.syncWaiting(ctx, &be, conditionReasonPlanNotPublished, planNotPublishedError(&offer), transientRequeueAfter)
		case errors.Is(err, openmeter.ErrCustomerBillingNotReady):
			logger.Info("customer cannot be invoiced yet", "err", err.Error())
			return r.syncWaiting(ctx, &be, conditionReasonCustomerBillingNotReady,
				fmt.Errorf("OpenMeter cannot invoice BillingAccount %q yet (does it have a default payment method?): %w", account.Name, err),
				customerBillingNotReadyRequeueAfter)
		}
		return r.openMeterFailed(ctx, &be, fmt.Errorf("ensure subscription: %w", err))
	}

	if be.Spec.Contract != nil {
		if err := r.recordContractState(ctx, &be, state.ChangeAt); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Subscriptions can be changed in OpenMeter directly; the resync
	// restores the entitled one without waiting for a Kubernetes event.
	requeueAfter := subscriptionResyncAfter
	reason, msg := conditionReasonSynced, subscriptionMessage(state)
	switch {
	case state.ChangeAt != nil:
		reason = conditionReasonOfferChangeScheduled
		msg += fmt.Sprintf("; changes to Offer %q at the end of its billing period, %s",
			offer.Name, state.ChangeAt.UTC().Format(time.RFC3339))
		// Just after the boundary, so OpenMeter is in the new period too.
		requeueAfter = min(requeueAfter, state.ChangeAt.Sub(now)+changeAtPeriodEndDelay)
	case state.Pending != nil:
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
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// changeAtPeriodEndDelay is how long after a billing period ends the
// entitlement is reconciled to make the change deferred to it, allowing for
// the provider's clock running ahead of OpenMeter's. It must stay well
// inside openmeter's periodChangeTolerance.
const changeAtPeriodEndDelay = 5 * time.Second

// recordContractState records on a contract's entitlement that the account
// is on its contract, once it is (ContractStartedAnnotation), and when an
// amendment is due, while one is (OfferChangeAtAnnotation).
func (r *BillingEntitlementReconciler) recordContractState(ctx context.Context, be *billingv1alpha1.BillingEntitlement, changeAt *time.Time) error {
	want := map[string]string{}
	if _, started := be.Annotations[ContractStartedAnnotation]; started || changeAt == nil {
		want[ContractStartedAnnotation] = be.Spec.Contract.StartsAt.UTC().Format(time.RFC3339)
	}
	if changeAt != nil {
		want[OfferChangeAtAnnotation] = changeAt.UTC().Format(time.RFC3339)
	}
	base := be.DeepCopy()
	changed := false
	for _, key := range []string{ContractStartedAnnotation, OfferChangeAtAnnotation} {
		value, ok := want[key]
		current, has := be.Annotations[key]
		switch {
		case ok && (!has || current != value):
			if be.Annotations == nil {
				be.Annotations = map[string]string{}
			}
			be.Annotations[key] = value
			changed = true
		case !ok && has:
			delete(be.Annotations, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := r.Patch(ctx, be, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("record the contract's state: %w", err)
	}
	return nil
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
	be *billingv1alpha1.BillingEntitlement,
	customerKey openmeter.CustomerKey,
) (ctrl.Result, error) {
	if err := r.OpenMeterClient.CancelSubscriptions(ctx, customerKey); err != nil {
		return r.openMeterFailed(ctx, be, fmt.Errorf("cancel subscriptions of deleting BillingAccount: %w", err))
	}
	return r.syncWaiting(ctx, be, conditionReasonBillingAccountDeleting,
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
			r.event(be, "Warning", EventReasonDeleteFailed, fmt.Sprintf("cancel OpenMeter subscription: %v", err))
			return ctrl.Result{}, fmt.Errorf("cancel subscriptions; keeping finalizer: %w", err)
		}
		logger.Info("OpenMeter subscription canceled")
		r.event(be, "Normal", EventReasonDeleted, fmt.Sprintf("OpenMeter subscription of customer %s canceled", customerKey))
	}

	controllerutil.RemoveFinalizer(be, SubscriptionFinalizer)
	if err := r.Update(ctx, be); err != nil {
		if apierrors.IsNotFound(err) {
			// Ours was the last finalizer; the object is gone.
			return ctrl.Result{}, nil
		}
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

// openMeterFailed records an OpenMeter failure, tagged with its
// classification, and returns it so controller-runtime logs it, counts it
// in controller_runtime_reconcile_errors_total and retries with backoff.
func (r *BillingEntitlementReconciler) openMeterFailed(
	ctx context.Context,
	be *billingv1alpha1.BillingEntitlement,
	err error,
) (ctrl.Result, error) {
	cause := fmt.Errorf("%s: %w", syncReason(err), err)
	if condErr := r.setSyncedFalse(ctx, be, conditionReasonOpenMeterError, cause); condErr != nil {
		return ctrl.Result{}, errors.Join(cause, condErr)
	}
	return ctrl.Result{}, cause
}

// syncWaiting records that the entitlement is waiting on something expected
// (its BillingAccount, Offer, OpenMeter customer or plan) and checks again
// after requeueAfter (not at all when it is zero: a watch brings the
// entitlement back). Waiting is not a failure, so no error is returned.
func (r *BillingEntitlementReconciler) syncWaiting(
	ctx context.Context,
	be *billingv1alpha1.BillingEntitlement,
	reason string,
	cause error,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	if err := r.setSyncedFalse(ctx, be, reason, cause); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// setSyncedFalse sets the condition to False. The Warning event is only
// emitted when the condition changes.
func (r *BillingEntitlementReconciler) setSyncedFalse(
	ctx context.Context,
	be *billingv1alpha1.BillingEntitlement,
	reason string,
	cause error,
) error {
	changed, err := r.setSyncedCondition(ctx, be, metav1.ConditionFalse, reason, cause.Error())
	if err != nil {
		return err
	}
	if changed {
		r.event(be, "Warning", EventReasonSyncFailed, fmt.Sprintf("%s: %v", reason, cause))
	}
	return nil
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

// subscriptionBillingAnchor is the date the account's subscriptions bill
// from, unless a contract sets it: its invoice day of the month, the 1st
// under the standard terms, in January 2025. OpenMeter moves an anchor
// back to the nearest cycle boundary before a subscription starts, so only
// the day counts, and every plan bills monthly (usageBillingPeriod). The billing profile's anchored alignment
// says the same, but OpenMeter ignores it (see DesiredBillingProfile).
func subscriptionBillingAnchor(account *billingv1alpha1.BillingAccount) time.Time {
	day := billingv1alpha1.StandardPaymentTermsInvoiceDayOfMonth
	if terms := account.Spec.PaymentTerms; terms != nil && terms.InvoiceDayOfMonth > 0 {
		day = terms.InvoiceDayOfMonth
	}
	return time.Date(2025, time.January, int(day), 0, 0, 0, 0, time.UTC)
}

func (r *BillingEntitlementReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
