// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

const (
	// MeterFinalizer blocks deletion of a MeterDefinition until the
	// matching OpenMeter meter has been removed (or a 404 confirms it is
	// already gone). The name intentionally shares the
	// openmeter.miloapis.com domain with sibling finalizers.
	MeterFinalizer = "openmeter.miloapis.com/meter"

	// Event reasons specific to the meter-definition reconciler.
	EventReasonSynced       = "Synced"
	EventReasonSyncFailed   = "SyncFailed"
	EventReasonDeleted      = "Deleted"
	EventReasonDeleteFailed = "DeleteFailed"

	// syncReason* prefixes tag the failure reasons surfaced in events and
	// logs so operators can grep a stable prefix.
	syncReasonPermanent = "Permanent"
	syncReasonTransient = "Transient"
	syncReasonInvalid   = "Invalid"

	// transientRequeueAfter is the delay before retrying transient
	// failures (OpenMeter down, 5xx, 429). permanentRequeueAfter backs off
	// permanent failures (e.g. an invalid spec OpenMeter keeps rejecting)
	// so a leftover is retried without a pod restart.
	transientRequeueAfter = 15 * time.Second
	permanentRequeueAfter = 5 * time.Minute

	// meterControllerName is the controller label value used for the
	// SetupWithManager "Named" call.
	meterControllerName = "meterdefinition"
)

// MeterDefinitionReconciler reconciles a billing.miloapis.com
// MeterDefinition into an OpenMeter meter via the openmeter.Client. State
// surfacing happens via Kubernetes Events — the reconciler does NOT write to
// MeterDefinition.status: billing's own controller owns the conditions there,
// and we deliberately avoid a dual-writer race on that field.
type MeterDefinitionReconciler struct {
	client.Client

	// OpenMeterClient is the typed wrapper around the OpenMeter REST API.
	OpenMeterClient openmeter.Client

	// Recorder emits Kubernetes events onto reconciled MeterDefinitions.
	Recorder record.EventRecorder

	// Finalizers manages MeterFinalizer's add-on-create and
	// remove-after-cleanup bookkeeping (see SetupWithManager, and
	// meterLinkFinalizer's Finalize method for the cleanup itself).
	Finalizers finalizer.Finalizers

	// Log is the reconciler-scoped logger. Each Reconcile call derives a
	// per-reconcile logger with meter/slug values.
	Log logr.Logger
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=meterdefinitions,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=meterdefinitions/finalizers,verbs=update
// The reconciler surface state via Kubernetes Events (Synced/Deleted/SyncFailed),
// written by the controller-runtime event recorder to core-v1 Events.
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile runs a single sync iteration for a MeterDefinition.
func (r *MeterDefinitionReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("meter", req.Name)

	var md billingv1alpha1.MeterDefinition
	if err := r.Get(ctx, req.NamespacedName, &md); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "failed to get MeterDefinition")
		return ctrl.Result{}, err
	}

	slug := openmeter.MeterSlug(md.Spec.MeterName)
	logger = logger.WithValues("uid", md.UID, "slug", slug, "meterName", md.Spec.MeterName)

	// Run finalizers: adds MeterFinalizer if absent (and not being
	// deleted), or — if being deleted — runs meterLinkFinalizer.Finalize
	// (DeleteMeter) and drops the finalizer once it succeeds.
	//
	// Persist before returning any error, so a finalizer that succeeded
	// isn't re-run on every retry because a sibling failed — see the
	// equivalent block in billingaccount_controller.go. Only one finalizer
	// is registered here today, but the ordering is the correct shape and
	// costs nothing.
	finalizeResult, finalizeErr := r.Finalizers.Finalize(ctx, &md)
	if finalizeResult.Updated {
		if err := r.Update(ctx, &md); err != nil {
			if apierrors.IsConflict(err) {
				logger.Info("conflict persisting finalizer change; requeueing")
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("persisting finalizer change: %w", err)
		}
	}
	if finalizeErr != nil {
		return ctrl.Result{}, fmt.Errorf("running finalizers: %w", finalizeErr)
	}
	if finalizeResult.Updated {
		return ctrl.Result{}, nil
	}

	if !md.DeletionTimestamp.IsZero() {
		logger.Info("MeterDefinition is being deleted, skipping reconciliation")
		return ctrl.Result{}, nil
	}

	desired, err := desiredMeter(&md)
	if err != nil {
		// Only reachable if the aggregation mapping falls behind the CRD's
		// enum (see openmeter.MeterAggregation) — a code bug, not a data
		// problem. Route through the same permanent-error handling as an
		// EnsureMeter failure so it gets an event and a periodic requeue
		// rather than wedging or silently miscategorizing usage.
		return r.handleOpenMeterError(logger, &md, "desiredMeter", err)
	}
	logger = logger.WithValues("aggregation", desired.Aggregation)

	meter, err := r.OpenMeterClient.EnsureMeter(ctx, desired)
	if err != nil {
		return r.handleOpenMeterError(logger, &md, "EnsureMeter", err)
	}

	logger.Info("reconciled meter definition",
		"aggregation", desired.Aggregation,
		"groupBy", len(desired.GroupBy),
	)
	if r.Recorder != nil {
		r.Recorder.Eventf(&md, "Normal", EventReasonSynced,
			"OpenMeter meter %s synced (active)", meter.Slug)
	}
	return ctrl.Result{}, nil
}

// handleOpenMeterError classifies the error, emits an event, and decides
// whether to requeue. Permanent failures requeue after permanentRequeueAfter
// so leftovers that later become adoptable are retried without a pod restart.
// Unclassified errors are treated as transient so a misclassification never
// wedges the reconcile loop.
// operation names the specific call that failed, so a spec-translation
// error isn't reported as an EnsureMeter failure.
func (r *MeterDefinitionReconciler) handleOpenMeterError(
	logger logr.Logger,
	md *billingv1alpha1.MeterDefinition,
	operation string,
	err error,
) (ctrl.Result, error) {
	switch {
	case openmeter.IsPermanent(err):
		logger.Error(err, fmt.Sprintf("OpenMeter %s permanent failure; requeueing", operation),
			"requeueAfter", permanentRequeueAfter.String())
		if r.Recorder != nil {
			r.Recorder.Eventf(md, "Warning", EventReasonSyncFailed,
				"%s: %v", syncReasonPermanent, err)
		}
		return ctrl.Result{RequeueAfter: permanentRequeueAfter}, nil
	case openmeter.IsTransient(err):
		logger.Info(fmt.Sprintf("OpenMeter %s transient failure; requeueing", operation),
			"err", err.Error(),
			"requeueAfter", transientRequeueAfter.String())
		if r.Recorder != nil {
			r.Recorder.Eventf(md, "Warning", EventReasonSyncFailed,
				"%s: %v", syncReasonTransient, err)
		}
		return ctrl.Result{RequeueAfter: transientRequeueAfter}, nil
	default:
		logger.Error(err, fmt.Sprintf("OpenMeter %s unclassified failure; treating as transient", operation))
		if r.Recorder != nil {
			r.Recorder.Eventf(md, "Warning", EventReasonSyncFailed,
				"%s: %v", syncReasonInvalid, err)
		}
		return ctrl.Result{RequeueAfter: transientRequeueAfter}, nil
	}
}

// desiredMeter translates a MeterDefinition into the openmeter.DesiredMeter
// shape.
//
// The slug is derived from the canonical reverse-DNS meterName (D2); the
// eventType is the full meterName, so ingest keys on the canonical
// identifier. GroupBy maps every declared dimension (plus the always-present
// project_name system dimension) to its JSONPath in the event data, so
// OpenMeter can group by them on query. The value comes from data.value.
func desiredMeter(md *billingv1alpha1.MeterDefinition) (openmeter.DesiredMeter, error) {
	aggregation, err := openmeter.MeterAggregation(md.Spec.Measurement.Aggregation)
	if err != nil {
		return openmeter.DesiredMeter{}, err
	}

	description := md.Spec.DisplayName
	if description == "" {
		description = md.Spec.MeterName
	}
	if md.Spec.Description != "" {
		description = md.Spec.Description
	}

	groupBy := make(map[string]string, len(md.Spec.Measurement.Dimensions)+1)
	for _, dim := range md.Spec.Measurement.Dimensions {
		groupBy[dim] = "$.dimensions." + dim
	}
	// project_name is injected on every valid event by the billing consumer;
	// expose it as a group-by dimension so queries can segment by project
	// without it being declared on the MeterDefinition.
	groupBy[billingv1alpha1.SystemDimensionProjectName] = "$.dimensions." + billingv1alpha1.SystemDimensionProjectName

	return openmeter.DesiredMeter{
		Slug:          openmeter.MeterSlug(md.Spec.MeterName),
		EventType:     md.Spec.MeterName,
		Aggregation:   aggregation,
		Description:   description,
		GroupBy:       groupBy,
		ValueProperty: "$.value",
	}, nil
}

// SetupWithManager registers the reconciler with mgr, wiring a watch on
// billing.miloapis.com/MeterDefinition. There is no periodic resync and no
// cross-resource watch — spec fields we care about are immutable (except
// DisplayName and Description), so relying on the object watch is sufficient.
func (r *MeterDefinitionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("openmeter-provider") //nolint:staticcheck // SA1019: GetEventRecorder (events/v1) is a larger migration.
	}
	if r.Log.GetSink() == nil {
		r.Log = mgr.GetLogger().WithName("meterdefinition-controller")
	}

	r.Finalizers = finalizer.NewFinalizers()
	if err := r.Finalizers.Register(MeterFinalizer, &meterLinkFinalizer{
		OpenMeterClient: r.OpenMeterClient,
		Recorder:        r.Recorder,
	}); err != nil {
		return fmt.Errorf("registering finalizer: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named(meterControllerName).
		For(&billingv1alpha1.MeterDefinition{}).
		Complete(r)
}

// meterLinkFinalizer removes the OpenMeter meter for a MeterDefinition being
// deleted. Registered under MeterFinalizer via finalizer.Finalizers (see
// SetupWithManager); the pkg/finalizer helper handles the add-on-create /
// remove-after-cleanup bookkeeping, and the caller persists via a plain
// client.Update — see customerLinkFinalizer's doc comment in
// billingaccount_controller.go for why that's sufficient without
// Server-Side Apply.
type meterLinkFinalizer struct {
	OpenMeterClient openmeter.Client
	Recorder        record.EventRecorder
}

// Finalize removes the OpenMeter meter for md's slug. 404s from OpenMeter
// are treated as success by DeleteMeter, mirroring its tolerant-not-found
// behavior; any other failure keeps the finalizer in place (blocking
// deletion) and returns the error so controller-runtime's default
// rate-limited backoff retries it.
func (f *meterLinkFinalizer) Finalize(ctx context.Context, obj client.Object) (finalizer.Result, error) {
	md, ok := obj.(*billingv1alpha1.MeterDefinition)
	if !ok {
		return finalizer.Result{}, fmt.Errorf("meterLinkFinalizer: object is not a MeterDefinition (%T)", obj)
	}
	logger := log.FromContext(ctx)
	slug := openmeter.MeterSlug(md.Spec.MeterName)

	if err := f.OpenMeterClient.DeleteMeter(ctx, slug); err != nil {
		if openmeter.IsTransient(err) {
			logger.Info("DeleteMeter transient failure", "err", err.Error())
		} else {
			logger.Error(err, "DeleteMeter failure; finalizer blocks deletion")
		}
		if f.Recorder != nil {
			f.Recorder.Eventf(md, "Warning", EventReasonDeleteFailed, "%v", err)
		}
		return finalizer.Result{}, err
	}

	logger.Info("OpenMeter meter deleted", "slug", slug)
	if f.Recorder != nil {
		f.Recorder.Eventf(md, "Normal", EventReasonDeleted,
			"OpenMeter meter %s deleted", slug)
	}
	return finalizer.Result{}, nil
}
