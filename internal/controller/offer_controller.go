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
	ProductPlanFinalizer = "openmeter.miloapis.com/product-plan"
	offerControllerName  = "offer"

	EventReasonSyncSkipped = "SyncSkipped"

	// ConditionTypeOpenMeterPlanSynced reports whether the Offer's OpenMeter
	// plan is published and matches the Offer. It is owned by this provider;
	// the billing controller preserves foreign condition types.
	ConditionTypeOpenMeterPlanSynced = "OpenMeterPlanSynced"

	conditionReasonSynced         = "Synced"
	conditionReasonMeterNotFound  = "MeterNotFound"
	conditionReasonMeterNotSynced = "MeterNotSynced"
	conditionReasonInvalidPricing = "InvalidPricing"
	conditionReasonOpenMeterError = "OpenMeterError"
)

// OfferReconciler syncs GA Offers into published OpenMeter plans.
//
// Each GA Offer owns one OpenMeter plan key (its UID). The plan content is a
// pure function of the Offer snapshot and the MeterDefinitions it references
// (see buildOfferMapping); EnsurePlan keeps exactly one published version
// matching it, versioning the plan whenever the content changes (only the
// display-name annotation can change on a GA Offer, but a provider upgrade
// may change the mapping too). Usage features are created before the plan
// that references them and garbage-collected after the plan is deleted.
type OfferReconciler struct {
	client.Client
	OpenMeterClient openmeter.Client
	Recorder        record.EventRecorder
	Log             logr.Logger
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=offers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=offers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=offers/finalizers,verbs=update
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=meterdefinitions,verbs=get;list;watch

// Reconcile syncs a single Offer.
func (r *OfferReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("offer", req.Name)

	var offer billingv1alpha1.Offer
	if err := r.Get(ctx, req.NamespacedName, &offer); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	planKey := planKeyForOffer(&offer)
	logger = logger.WithValues("planKey", planKey, "launchStage", offer.Spec.LaunchStage)

	if !offer.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, logger, &offer, planKey)
	}

	if !controllerutil.ContainsFinalizer(&offer, ProductPlanFinalizer) {
		controllerutil.AddFinalizer(&offer, ProductPlanFinalizer)
		if err := r.Update(ctx, &offer); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Only GA Offers with a non-empty snapshot are synced. Draft Offers
	// keep the finalizer so a later publish can clean up on delete.
	if offer.Spec.LaunchStage != billingv1alpha1.OfferLaunchStageGA {
		logger.V(1).Info("skipping sync: offer is not GA")
		return ctrl.Result{}, nil
	}
	if len(offer.Spec.ServicePricings) == 0 {
		// The billing controller fills the snapshot asynchronously after
		// launchStage flips to GA.
		logger.Info("skipping sync: GA offer has empty servicePricings snapshot")
		r.event(&offer, "Normal", EventReasonSyncSkipped, "GA Offer has no servicePricings snapshot yet")
		return ctrl.Result{RequeueAfter: transientRequeueAfter}, nil
	}

	return r.reconcileSync(ctx, logger, &offer)
}

// reconcileSync converges the Offer's features and published plan.
func (r *OfferReconciler) reconcileSync(
	ctx context.Context,
	logger logr.Logger,
	offer *billingv1alpha1.Offer,
) (ctrl.Result, error) {
	meters, err := r.meterIndex(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	mapping, err := buildOfferMapping(offer, meters)
	if err != nil {
		var invalid *invalidOfferError
		switch {
		case errors.Is(err, errMeterNotFound):
			// The MeterDefinition watch requeues the Offer as soon as the
			// meter appears; the timed requeue is a fallback.
			logger.Info("waiting for MeterDefinition", "reason", err.Error())
			return r.syncFailed(ctx, offer, conditionReasonMeterNotFound, err, transientRequeueAfter)
		case errors.As(err, &invalid):
			logger.Error(err, "offer cannot be represented in OpenMeter")
			return r.syncFailed(ctx, offer, conditionReasonInvalidPricing, err, permanentRequeueAfter)
		default:
			logger.Error(err, "build OpenMeter mapping")
			return r.syncFailed(ctx, offer, conditionReasonInvalidPricing, err, transientRequeueAfter)
		}
	}

	// Features can only be created on meters OpenMeter already has. The
	// MeterDefinition controller creates them asynchronously, so check
	// first: a feature create against a missing meter is a 4xx that would
	// otherwise be backed off as permanent.
	for _, slug := range mapping.MeterSlugs {
		if _, err := r.OpenMeterClient.GetMeter(ctx, slug); err != nil {
			if errors.Is(err, openmeter.ErrMeterNotFound) {
				logger.Info("waiting for OpenMeter meter", "meterSlug", slug)
				return r.syncFailed(ctx, offer, conditionReasonMeterNotSynced,
					fmt.Errorf("OpenMeter meter %q does not exist yet", slug), transientRequeueAfter)
			}
			return r.openMeterFailed(ctx, logger, offer, fmt.Errorf("get meter %q: %w", slug, err))
		}
	}

	for _, feature := range mapping.Features {
		if _, err := r.OpenMeterClient.EnsureFeature(ctx, feature); err != nil {
			return r.openMeterFailed(ctx, logger, offer, fmt.Errorf("ensure feature %q: %w", feature.Key, err))
		}
	}

	plan, err := r.OpenMeterClient.EnsurePlan(ctx, mapping.Plan)
	if err != nil {
		return r.openMeterFailed(ctx, logger, offer, fmt.Errorf("ensure plan: %w", err))
	}

	// The plan reference lives in the status condition, never in an
	// annotation: GA Offers are immutable except for
	// kubernetes.io/display-name, so a metadata write would be denied.
	msg := fmt.Sprintf("OpenMeter plan %s version %d is %s", plan.Id, plan.Version, plan.Status)
	changed, err := r.setSyncedCondition(ctx, offer, metav1.ConditionTrue, conditionReasonSynced, msg)
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		logger.Info("reconciled offer plan", "planID", plan.Id, "version", plan.Version,
			"rateCards", len(mapping.Plan.Phases[0].RateCards), "features", len(mapping.Features))
		r.event(offer, "Normal", EventReasonSynced, msg)
	}
	return ctrl.Result{}, nil
}

// openMeterFailed classifies an OpenMeter error into a requeue schedule.
func (r *OfferReconciler) openMeterFailed(
	ctx context.Context,
	logger logr.Logger,
	offer *billingv1alpha1.Offer,
	err error,
) (ctrl.Result, error) {
	switch {
	case openmeter.IsPermanent(err):
		logger.Error(err, "OpenMeter permanent failure", "requeueAfter", permanentRequeueAfter.String())
		return r.syncFailed(ctx, offer, conditionReasonOpenMeterError,
			fmt.Errorf("%s: %w", syncReasonPermanent, err), permanentRequeueAfter)
	case openmeter.IsTransient(err):
		logger.Info("OpenMeter transient failure", "err", err.Error(), "requeueAfter", transientRequeueAfter.String())
		return r.syncFailed(ctx, offer, conditionReasonOpenMeterError,
			fmt.Errorf("%s: %w", syncReasonTransient, err), transientRequeueAfter)
	default:
		logger.Error(err, "OpenMeter unclassified failure; treating as transient")
		return r.syncFailed(ctx, offer, conditionReasonOpenMeterError,
			fmt.Errorf("%s: %w", syncReasonTransient, err), transientRequeueAfter)
	}
}

// syncFailed records a failed sync on the Offer and schedules a retry. The
// Warning event is only emitted when the condition changes, so a persistent
// failure does not flood the event stream every requeue.
func (r *OfferReconciler) syncFailed(
	ctx context.Context,
	offer *billingv1alpha1.Offer,
	reason string,
	cause error,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	changed, err := r.setSyncedCondition(ctx, offer, metav1.ConditionFalse, reason, cause.Error())
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		r.event(offer, "Warning", EventReasonSyncFailed, fmt.Sprintf("%s: %v", reason, cause))
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// setSyncedCondition patches the OpenMeterPlanSynced condition when it
// changes. The patch carries an optimistic lock so it never clobbers a
// concurrent status write by the billing controller; a conflict is returned
// and the reconcile retried.
func (r *OfferReconciler) setSyncedCondition(
	ctx context.Context,
	offer *billingv1alpha1.Offer,
	status metav1.ConditionStatus,
	reason, message string,
) (bool, error) {
	base := offer.DeepCopy()
	changed := apimeta.SetStatusCondition(&offer.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeOpenMeterPlanSynced,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: offer.Generation,
	})
	if !changed {
		return false, nil
	}
	if err := r.Status().Patch(ctx, offer, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, fmt.Errorf("patch offer status: %w", err)
	}
	return true, nil
}

// reconcileDelete removes every version of the Offer's plan, then archives
// the features they referenced that no other plan still uses. Feature GC
// must follow plan deletion: before it, the Offer's own plan references the
// features and nothing would be archived. Deleted versions are listed too,
// so a retry after a partial failure still knows which features to collect.
func (r *OfferReconciler) reconcileDelete(
	ctx context.Context,
	logger logr.Logger,
	offer *billingv1alpha1.Offer,
	planKey string,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(offer, ProductPlanFinalizer) {
		return ctrl.Result{}, nil
	}

	versions, err := r.OpenMeterClient.ListPlanVersions(ctx, planKey, true)
	if err != nil {
		// Never release the finalizer without knowing whether a published
		// plan exists: it would stay assignable with no Offer behind it.
		requeue := transientRequeueAfter
		if openmeter.IsPermanent(err) {
			requeue = permanentRequeueAfter
		}
		logger.Error(err, "list plan versions for deletion; keeping finalizer", "requeueAfter", requeue.String())
		r.event(offer, "Warning", EventReasonDeleteFailed, fmt.Sprintf("list plan versions: %v", err))
		return ctrl.Result{RequeueAfter: requeue}, nil
	}

	var featureKeys []string
	seen := map[string]struct{}{}
	for _, v := range versions {
		for _, key := range openmeter.PlanFeatureKeys(v) {
			if _, dup := seen[key]; !dup {
				seen[key] = struct{}{}
				featureKeys = append(featureKeys, key)
			}
		}
	}

	deleted := 0
	for _, v := range versions {
		if v.DeletedAt != nil {
			continue
		}
		if err := r.OpenMeterClient.DeletePlan(ctx, v); err != nil {
			if !openmeter.IsPermanent(err) {
				logger.Info("DeletePlan failed; requeueing", "planID", v.Id, "err", err.Error())
				r.event(offer, "Warning", EventReasonDeleteFailed, fmt.Sprintf("delete plan %s: %v", v.Id, err))
				return ctrl.Result{RequeueAfter: transientRequeueAfter}, nil
			}
			// Permanent: OpenMeter refuses this version for good. Holding
			// the finalizer would block the Offer forever, so the version is
			// left behind and surfaced instead.
			logger.Error(err, "DeletePlan permanent failure; leaving plan version in OpenMeter", "planID", v.Id)
			r.event(offer, "Warning", EventReasonDeleteFailed,
				fmt.Sprintf("permanent: %v; leaving OpenMeter plan %s version %d in place", err, v.Id, v.Version))
			continue
		}
		deleted++
	}

	if err := r.OpenMeterClient.ArchiveFeaturesIfUnreferenced(ctx, featureKeys); err != nil {
		if !openmeter.IsPermanent(err) {
			logger.Info("archive features failed; requeueing", "err", err.Error())
			r.event(offer, "Warning", EventReasonDeleteFailed, fmt.Sprintf("archive features: %v", err))
			return ctrl.Result{RequeueAfter: transientRequeueAfter}, nil
		}
		// Leftover features are harmless to billing; the MeterDefinition
		// finalizer archives them before deleting the meter.
		logger.Error(err, "archive features permanent failure; leaving features active")
		r.event(offer, "Warning", EventReasonDeleteFailed,
			fmt.Sprintf("permanent: %v; leaving OpenMeter features active", err))
	}

	if deleted > 0 {
		logger.Info("OpenMeter plan deleted", "versions", deleted)
		r.event(offer, "Normal", EventReasonDeleted,
			fmt.Sprintf("OpenMeter plan %s deleted (%d versions)", planKey, deleted))
	}

	controllerutil.RemoveFinalizer(offer, ProductPlanFinalizer)
	if err := r.Update(ctx, offer); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// meterIndex maps every MeterDefinition's spec.meterName to what the
// mapping needs. The slug must come from spec.meterName — the same
// derivation the MeterDefinition controller uses when it creates the meter.
// metadata.name is an unrelated Kubernetes name, and deriving the slug from
// it points features at a meter that does not exist.
func (r *OfferReconciler) meterIndex(ctx context.Context) (map[string]meterInfo, error) {
	var list billingv1alpha1.MeterDefinitionList
	if err := r.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list MeterDefinitions: %w", err)
	}
	out := make(map[string]meterInfo, len(list.Items))
	for i := range list.Items {
		md := &list.Items[i]
		if md.Spec.MeterName == "" {
			continue
		}
		out[md.Spec.MeterName] = meterInfo{
			Name:       md.Spec.MeterName,
			Slug:       openmeter.MeterSlug(md.Spec.MeterName),
			Dimensions: md.Spec.Measurement.Dimensions,
		}
	}
	return out, nil
}

// offersForMeter enqueues every GA Offer that prices md's metric, so an
// Offer waiting on its meter converges as soon as the meter is defined.
func (r *OfferReconciler) offersForMeter(ctx context.Context, obj client.Object) []reconcile.Request {
	md, ok := obj.(*billingv1alpha1.MeterDefinition)
	if !ok || md.Spec.MeterName == "" {
		return nil
	}
	var offers billingv1alpha1.OfferList
	if err := r.List(ctx, &offers); err != nil {
		log.FromContext(ctx).Error(err, "list Offers for MeterDefinition", "meterDefinition", md.Name)
		return nil
	}
	var reqs []reconcile.Request
	for _, offer := range offers.Items {
		if offer.Spec.LaunchStage != billingv1alpha1.OfferLaunchStageGA {
			continue
		}
		for _, sp := range offer.Spec.ServicePricings {
			if sp.Spec.ChargeType == billingv1alpha1.ChargeTypeUsage && sp.Spec.Metric == md.Spec.MeterName {
				reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: offer.Name}})
				break
			}
		}
	}
	return reqs
}

func (r *OfferReconciler) event(offer *billingv1alpha1.Offer, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(offer, eventType, reason, message)
	}
}

// SetupWithManager registers the Offer reconciler.
func (r *OfferReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("openmeter-provider") //nolint:staticcheck // SA1019: GetEventRecorder (events/v1) is a larger migration.
	}
	if r.Log.GetSink() == nil {
		r.Log = mgr.GetLogger().WithName("offer-controller")
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(offerControllerName).
		For(&billingv1alpha1.Offer{}).
		Watches(&billingv1alpha1.MeterDefinition{}, handler.EnqueueRequestsFromMapFunc(r.offersForMeter)).
		Complete(r)
}
