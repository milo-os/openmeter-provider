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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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

	// Log is the reconciler-scoped logger. Each Reconcile call derives a
	// per-reconcile logger with meter/slug values.
	Log logr.Logger
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=meterdefinitions,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=meterdefinitions/finalizers,verbs=update

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

	// Deletion path: release the OpenMeter meter, then drop the finalizer.
	if !md.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, logger, &md, slug)
	}

	// Ensure the finalizer is attached before we touch OpenMeter so we
	// never create a meter we cannot clean up.
	if !controllerutil.ContainsFinalizer(&md, MeterFinalizer) {
		controllerutil.AddFinalizer(&md, MeterFinalizer)
		if err := r.Update(ctx, &md); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		// Requeue naturally via the watch on the updated object.
		return ctrl.Result{}, nil
	}

	desired := desiredMeter(&md)
	logger = logger.WithValues("aggregation", desired.Aggregation)

	meter, err := r.OpenMeterClient.EnsureMeter(ctx, desired)
	if err != nil {
		return r.handleOpenMeterError(logger, &md, err)
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

// reconcileDelete removes the OpenMeter meter and then releases the
// finalizer. 404s from the OpenMeter side are treated as success,
// mirroring the DeleteMeter tolerant-not-found behavior.
func (r *MeterDefinitionReconciler) reconcileDelete(
	ctx context.Context,
	logger logr.Logger,
	md *billingv1alpha1.MeterDefinition,
	slug string,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(md, MeterFinalizer) {
		return ctrl.Result{}, nil
	}

	if err := r.OpenMeterClient.DeleteMeter(ctx, slug); err != nil {
		switch {
		case openmeter.IsTransient(err):
			logger.Info("DeleteMeter transient failure; requeueing",
				"err", err.Error())
			if r.Recorder != nil {
				r.Recorder.Eventf(md, "Warning", EventReasonDeleteFailed,
					"transient: %v", err)
			}
			return ctrl.Result{RequeueAfter: transientRequeueAfter}, nil
		default:
			logger.Error(err, "DeleteMeter permanent failure; finalizer blocks deletion")
			if r.Recorder != nil {
				r.Recorder.Eventf(md, "Warning", EventReasonDeleteFailed,
					"permanent: %v", err)
			}
			return ctrl.Result{RequeueAfter: permanentRequeueAfter}, nil
		}
	}

	logger.Info("OpenMeter meter deleted")
	if r.Recorder != nil {
		r.Recorder.Eventf(md, "Normal", EventReasonDeleted,
			"OpenMeter meter %s deleted", slug)
	}

	controllerutil.RemoveFinalizer(md, MeterFinalizer)
	if err := r.Update(ctx, md); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// handleOpenMeterError classifies the error, emits an event, and decides
// whether to requeue. Permanent failures requeue after permanentRequeueAfter
// so leftovers that later become adoptable are retried without a pod restart.
// Unclassified errors are treated as transient so a misclassification never
// wedges the reconcile loop.
func (r *MeterDefinitionReconciler) handleOpenMeterError(
	logger logr.Logger,
	md *billingv1alpha1.MeterDefinition,
	err error,
) (ctrl.Result, error) {
	switch {
	case openmeter.IsPermanent(err):
		logger.Error(err, "OpenMeter EnsureMeter permanent failure; requeueing",
			"requeueAfter", permanentRequeueAfter.String())
		if r.Recorder != nil {
			r.Recorder.Eventf(md, "Warning", EventReasonSyncFailed,
				"%s: %v", syncReasonPermanent, err)
		}
		return ctrl.Result{RequeueAfter: permanentRequeueAfter}, nil
	case openmeter.IsTransient(err):
		logger.Info("OpenMeter EnsureMeter transient failure; requeueing",
			"err", err.Error(),
			"requeueAfter", transientRequeueAfter.String())
		if r.Recorder != nil {
			r.Recorder.Eventf(md, "Warning", EventReasonSyncFailed,
				"%s: %v", syncReasonTransient, err)
		}
		return ctrl.Result{RequeueAfter: transientRequeueAfter}, nil
	default:
		logger.Error(err, "OpenMeter EnsureMeter unclassified failure; treating as transient")
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
func desiredMeter(md *billingv1alpha1.MeterDefinition) openmeter.DesiredMeter {
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
		Aggregation:   openmeter.MeterAggregation(md.Spec.Measurement.Aggregation),
		Description:   description,
		GroupBy:       groupBy,
		ValueProperty: "$.value",
	}
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
		r.Recorder = mgr.GetEventRecorderFor("openmeter-provider")
	}
	if r.Log.GetSink() == nil {
		r.Log = mgr.GetLogger().WithName("meterdefinition-controller")
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(meterControllerName).
		For(&billingv1alpha1.MeterDefinition{}).
		Complete(r)
}
