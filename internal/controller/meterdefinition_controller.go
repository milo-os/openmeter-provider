// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
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
	// EventReasonDeleteSkipped is emitted when the finalizer releases a
	// MeterDefinition without deleting the OpenMeter meter under its slug,
	// because that meter belongs to a different meterName (slug collision).
	EventReasonDeleteSkipped = "DeleteSkipped"

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

	// meterResyncInterval is the periodic re-check of a successfully synced
	// meter. Nothing on the Kubernetes side changes when an OpenMeter meter
	// is deleted or edited out of band, so without it such drift would never
	// be repaired.
	meterResyncInterval = 30 * time.Minute

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
func (r *MeterDefinitionReconciler) Reconcile(ctx context.Context, req reconcile.Request) (res ctrl.Result, err error) {
	defer observeReconcile("meterdefinition", time.Now(), &res, &err)

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
	//
	// The change is persisted as a merge patch of metadata.finalizers
	// (with an optimistic lock), not a full Update: MeterDefinition is owned
	// by billing, and a full Update from this binary's compiled-in type
	// would strip any spec field added by a newer billing API version.
	base := md.DeepCopy()
	finalizeResult, finalizeErr := r.Finalizers.Finalize(ctx, &md)
	if finalizeResult.Updated {
		patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
		if err := r.Patch(ctx, &md, patch); err != nil {
			switch {
			case apierrors.IsConflict(err):
				logger.Info("conflict persisting finalizer change; requeueing")
				return ctrl.Result{Requeue: true}, nil
			case apierrors.IsNotFound(err):
				// Our finalizer was the last one; the object is gone.
				return ctrl.Result{}, nil
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

	// Every phase (Draft, Published, Deprecated, Retired) keeps its meter:
	// OpenMeter stores events regardless of meter state, and the submission
	// consumer forwards late events for Deprecated/Retired meters, which
	// must stay queryable for billing. The meter is only removed when the
	// MeterDefinition itself is deleted (see meterLinkFinalizer).
	desired, err := desiredMeter(&md)
	if err != nil {
		// Reachable for a dimension set OpenMeter cannot represent (two
		// dimensions sanitizing to the same group-by key, or a name no
		// JSONPath form can address), or if the aggregation mapping falls
		// behind the CRD's enum (a code bug). Route through the same
		// permanent-error handling as an EnsureMeter failure so it gets an
		// event and a periodic requeue rather than wedging or silently
		// miscategorizing usage.
		return r.handleOpenMeterError(logger, &md, "desiredMeter", err)
	}
	logger = logger.WithValues("aggregation", desired.Aggregation)

	meter, changed, err := r.OpenMeterClient.EnsureMeter(ctx, desired)
	if err != nil {
		return r.handleOpenMeterError(logger, &md, "EnsureMeter", err)
	}

	if !changed {
		// Billing rewrites the MeterDefinition's status on its own schedule,
		// and every write triggers a reconcile; only a real change is worth
		// a log line and an event.
		logger.V(1).Info("meter definition already in sync")
		return ctrl.Result{RequeueAfter: meterResyncInterval}, nil
	}
	logger.Info("reconciled meter definition",
		"aggregation", desired.Aggregation,
		"groupBy", len(desired.GroupBy),
	)
	if r.Recorder != nil {
		r.Recorder.Eventf(&md, "Normal", EventReasonSynced,
			"OpenMeter meter %s synced", meter.Slug)
	}
	return ctrl.Result{RequeueAfter: meterResyncInterval}, nil
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
// identifier. GroupBy maps every declared dimension plus every system
// dimension the billing pipeline injects (status.systemDimensions, falling
// back to project_name before billing has populated it) to its JSONPath in
// the event data, keyed by openmeter.GroupByKey, so OpenMeter can group by
// them on query. The valueProperty depends on the aggregation (see
// openmeter.MeterValueProperty).
func desiredMeter(md *billingv1alpha1.MeterDefinition) (openmeter.DesiredMeter, error) {
	aggregation, err := openmeter.MeterAggregation(md.Spec.Measurement.Aggregation)
	if err != nil {
		return openmeter.DesiredMeter{}, err
	}

	name := md.Spec.DisplayName
	if name == "" {
		name = md.Spec.MeterName
	}
	description := name
	if md.Spec.Description != "" {
		description = md.Spec.Description
	}

	groupBy, err := openmeter.MeterGroupBy(openmeter.MeterDimensions(md))
	if err != nil {
		return openmeter.DesiredMeter{}, err
	}

	return openmeter.DesiredMeter{
		Slug:          openmeter.MeterSlug(md.Spec.MeterName),
		EventType:     md.Spec.MeterName,
		Aggregation:   aggregation,
		Name:          name,
		Description:   description,
		GroupBy:       groupBy,
		ValueProperty: openmeter.MeterValueProperty(md.Spec.Measurement.Aggregation),
	}, nil
}

// SetupWithManager registers the reconciler with mgr, wiring a watch on
// billing.miloapis.com/MeterDefinition. Successful reconciles also requeue
// every meterResyncInterval, so a meter deleted or edited directly in
// OpenMeter is repaired without a MeterDefinition change.
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
// remove-after-cleanup bookkeeping, and the caller persists it as a merge
// patch of metadata.finalizers.
type meterLinkFinalizer struct {
	OpenMeterClient openmeter.Client
	Recorder        record.EventRecorder
}

// Finalize removes the OpenMeter meter for md's slug — but only if that
// meter is md's own. Slugs are not injective (see openmeter.MeterSlug), so
// the meter under md's slug may belong to another MeterDefinition whose
// meterName collides; deleting it (and archiving its features) would break
// that other meter. The meter's eventType is the full meterName, so it
// identifies the owner unambiguously.
//
// OpenMeter's own not-found answer (from GetMeter or DeleteMeter) counts as
// success; any other failure keeps the finalizer in place (blocking
// deletion) and returns the error so controller-runtime's default
// rate-limited backoff retries it.
func (f *meterLinkFinalizer) Finalize(ctx context.Context, obj client.Object) (finalizer.Result, error) {
	md, ok := obj.(*billingv1alpha1.MeterDefinition)
	if !ok {
		return finalizer.Result{}, fmt.Errorf("meterLinkFinalizer: object is not a MeterDefinition (%T)", obj)
	}
	logger := log.FromContext(ctx)
	slug := openmeter.MeterSlug(md.Spec.MeterName)

	meter, err := f.OpenMeterClient.GetMeter(ctx, slug)
	switch {
	case errors.Is(err, openmeter.ErrMeterNotFound):
		logger.Info("OpenMeter meter already absent", "slug", slug)
		if f.Recorder != nil {
			f.Recorder.Eventf(md, "Normal", EventReasonDeleted,
				"OpenMeter meter %s already absent", slug)
		}
		return finalizer.Result{}, nil
	case err != nil:
		return finalizer.Result{}, f.deleteFailed(logger, md, "GetMeter", err)
	}
	if meter.EventType != md.Spec.MeterName {
		logger.Info("OpenMeter meter under this slug belongs to another meterName; leaving it in place",
			"slug", slug, "meterEventType", meter.EventType)
		if f.Recorder != nil {
			f.Recorder.Eventf(md, "Warning", EventReasonDeleteSkipped,
				"OpenMeter meter %s belongs to meterName %q, not %q; not deleting it",
				slug, meter.EventType, md.Spec.MeterName)
		}
		return finalizer.Result{}, nil
	}

	// Archive the meter's features that no live plan references before the
	// meter itself is deleted. OpenMeter rejects deleting a meter that still
	// has active features (409 conflict), which would wedge this finalizer
	// forever. Features still referenced by a live plan are deliberately left
	// active and the subsequent DeleteMeter 409 keeps the finalizer in place —
	// the MeterDefinition cannot go while another Offer's plan still depends
	// on its meter, which is the correct dependency ordering.
	if err := f.OpenMeterClient.ArchiveUnreferencedMeterFeatures(ctx, slug); err != nil {
		return finalizer.Result{}, f.deleteFailed(logger, md, "ArchiveUnreferencedMeterFeatures", err)
	}

	if err := f.OpenMeterClient.DeleteMeter(ctx, slug); err != nil {
		return finalizer.Result{}, f.deleteFailed(logger, md, "DeleteMeter", err)
	}

	logger.Info("OpenMeter meter deleted", "slug", slug)
	if f.Recorder != nil {
		f.Recorder.Eventf(md, "Normal", EventReasonDeleted,
			"OpenMeter meter %s deleted", slug)
	}
	return finalizer.Result{}, nil
}

// deleteFailed logs and records a failed cleanup step and returns err, so
// the finalizer stays in place and the reconcile is retried with backoff.
func (f *meterLinkFinalizer) deleteFailed(logger logr.Logger, md *billingv1alpha1.MeterDefinition, operation string, err error) error {
	if openmeter.IsTransient(err) {
		logger.Info(operation+" transient failure; finalizer blocks deletion", "err", err.Error())
	} else {
		logger.Error(err, operation+" failure; finalizer blocks deletion")
	}
	if f.Recorder != nil {
		f.Recorder.Eventf(md, "Warning", EventReasonDeleteFailed, "%s: %v", operation, err)
	}
	return err
}
