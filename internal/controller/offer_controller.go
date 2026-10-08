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
	"strings"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	om "github.com/openmeterio/openmeter/api/client/go"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

const (
	ProductPlanFinalizer = "openmeter.miloapis.com/product-plan"
	offerControllerName  = "offer"

	EventReasonSyncSkipped = "SyncSkipped"
)

// OfferReconciler syncs GA Offers into OpenMeter Product Plans.
type OfferReconciler struct {
	client.Client
	OpenMeterClient openmeter.Client
	Recorder        record.EventRecorder
	Log             logr.Logger
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=offers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=offers/finalizers,verbs=update
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=meterdefinitions,verbs=get;list;watch

// Reconcile syncs a single Offer.
func (r *OfferReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("offer", req.Name)

	var result ctrl.Result
	var reconcileErr error

	var offer billingv1alpha1.Offer
	if err := r.Get(ctx, req.NamespacedName, &offer); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	planKey := strings.ReplaceAll(string(offer.UID), "-", "_")
	logger = logger.WithValues("uid", planKey, "launchStage", offer.Spec.LaunchStage)

	if !offer.DeletionTimestamp.IsZero() {
		result, reconcileErr = r.reconcileDelete(ctx, logger, &offer, planKey)
		return result, reconcileErr
	}

	if !controllerutil.ContainsFinalizer(&offer, ProductPlanFinalizer) {
		controllerutil.AddFinalizer(&offer, ProductPlanFinalizer)
		if err := r.Update(ctx, &offer); err != nil {
			reconcileErr = fmt.Errorf("add finalizer: %w", err)
			return ctrl.Result{}, reconcileErr
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
		logger.Info("skipping sync: GA offer has empty servicePricings snapshot")
		if r.Recorder != nil {
			r.Recorder.Eventf(&offer, "Normal", EventReasonSyncSkipped,
				"GA Offer has no servicePricings snapshot yet")
		}
		return ctrl.Result{RequeueAfter: transientRequeueAfter}, nil
	}

	desired, err := r.desiredPlan(ctx, &offer)
	if err != nil {
		logger.Error(err, "build DesiredPlan")
		if r.Recorder != nil {
			r.Recorder.Eventf(&offer, "Warning", EventReasonSyncFailed, "%v", err)
		}
		return ctrl.Result{RequeueAfter: transientRequeueAfter}, nil
	}

	plan, err := r.OpenMeterClient.EnsurePlan(ctx, desired)
	if err != nil {
		return r.handleOpenMeterError(logger, &offer, err)
	}

	// Do not annotate the Offer after sync. GA Offers are immutable except
	// for kubernetes.io/display-name, so writing openmeter.miloapis.com/product-plan-id
	// is denied and the reconciler would retry forever. BillingEntitlement
	// already requeues while GetProductPlan returns not found.

	logger.Info("reconciled offer product plan", "planID", plan.Id, "items", len(desired.Phases))
	if r.Recorder != nil {
		r.Recorder.Eventf(&offer, "Normal", EventReasonSynced,
			"OpenMeter product plan %s synced", plan.Id)
	}
	return ctrl.Result{}, nil
}

func (r *OfferReconciler) reconcileDelete(
	ctx context.Context,
	logger logr.Logger,
	offer *billingv1alpha1.Offer,
	planKey string,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(offer, ProductPlanFinalizer) {
		return ctrl.Result{}, nil
	}

	logger.Info("Attempting to get plan by key for deletion", "planKey", planKey)
	plan, err := r.OpenMeterClient.GetPlanByKey(ctx, planKey)
	if err != nil {
		if errors.Is(err, openmeter.ErrPlanNotFound) {
			controllerutil.RemoveFinalizer(offer, ProductPlanFinalizer)
			return ctrl.Result{}, r.Update(ctx, offer)
		}
		logger.Error(err, "reconcileDelete GetPlanByKey unclassified failure; requeueing")
		return ctrl.Result{RequeueAfter: transientRequeueAfter}, nil
	}

	// Archive the plan's features that no other live plan references. Deleting
	// the plan alone leaves its usage features orphaned and active; those
	// orphans then block the meter deletion (OpenMeter rejects deleting a
	// meter with active features), wedging the MeterDefinition finalizer.
	// Features still referenced by another plan are left active — archiving
	// one would break the referencing plan on its next reconcile.
	featureKeys := planReferencedFeatureKeys(&plan)
	if err := r.OpenMeterClient.ArchiveFeaturesIfUnreferenced(ctx, featureKeys); err != nil {
		switch {
		case openmeter.IsTransient(err):
			logger.Info("archive features transient failure; requeueing", "err", err.Error())
			if r.Recorder != nil {
				r.Recorder.Eventf(offer, "Warning", EventReasonDeleteFailed, "transient: %v", err)
			}
			return ctrl.Result{RequeueAfter: transientRequeueAfter}, nil
		case openmeter.IsPermanent(err):
			logger.Error(err, "archive features permanent failure; releasing finalizer and leaving features")
			if r.Recorder != nil {
				r.Recorder.Eventf(offer, "Warning", EventReasonDeleteFailed,
					"permanent: %v; leaving OpenMeter features archived", err)
			}
		default:
			logger.Error(err, "archive features unclassified failure; requeueing")
			if r.Recorder != nil {
				r.Recorder.Eventf(offer, "Warning", EventReasonDeleteFailed, "unclassified: %v", err)
			}
			return ctrl.Result{RequeueAfter: transientRequeueAfter}, nil
		}
	}

	if err := r.OpenMeterClient.DeletePlan(ctx, plan.Id); err != nil {
		switch {
		case openmeter.IsTransient(err):
			logger.Info("DeletePlan transient failure; requeueing", "err", err.Error())
			if r.Recorder != nil {
				r.Recorder.Eventf(offer, "Warning", EventReasonDeleteFailed, "transient: %v", err)
			}
			return ctrl.Result{RequeueAfter: transientRequeueAfter}, nil
		case openmeter.IsPermanent(err):
			logger.Error(err, "DeletePlan permanent failure; releasing finalizer and leaving OpenMeter plan")
			if r.Recorder != nil {
				r.Recorder.Eventf(offer, "Warning", EventReasonDeleteFailed,
					"permanent: %v; leaving OpenMeter plan in place", err)
			}
		default:
			logger.Error(err, "DeletePlan unclassified failure; requeueing")
			if r.Recorder != nil {
				r.Recorder.Eventf(offer, "Warning", EventReasonDeleteFailed, "unclassified: %v", err)
			}
			return ctrl.Result{RequeueAfter: transientRequeueAfter}, nil
		}
	} else {
		logger.Info("OpenMeter product plan deleted")
		if r.Recorder != nil {
			r.Recorder.Eventf(offer, "Normal", EventReasonDeleted, "OpenMeter product plan %s deleted", planKey)
		}
	}

	controllerutil.RemoveFinalizer(offer, ProductPlanFinalizer)
	if err := r.Update(ctx, offer); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *OfferReconciler) handleOpenMeterError(
	logger logr.Logger,
	offer *billingv1alpha1.Offer,
	err error,
) (ctrl.Result, error) {
	switch {
	case openmeter.IsPermanent(err):
		logger.Error(err, "OpenMeter EnsurePlan permanent failure; requeueing",
			"requeueAfter", permanentRequeueAfter.String())
		if r.Recorder != nil {
			r.Recorder.Eventf(offer, "Warning", EventReasonSyncFailed, "%s: %v", syncReasonPermanent, err)
		}
		return ctrl.Result{RequeueAfter: permanentRequeueAfter}, nil
	case openmeter.IsTransient(err):
		logger.Info("OpenMeter EnsurePlan transient failure; requeueing",
			"err", err.Error(), "requeueAfter", transientRequeueAfter.String())
		if r.Recorder != nil {
			r.Recorder.Eventf(offer, "Warning", EventReasonSyncFailed, "%s: %v", syncReasonTransient, err)
		}
		return ctrl.Result{RequeueAfter: transientRequeueAfter}, nil
	default:
		logger.Error(err, "OpenMeter EnsurePlan unclassified failure; treating as transient")
		if r.Recorder != nil {
			r.Recorder.Eventf(offer, "Warning", EventReasonSyncFailed, "%s: %v", syncReasonInvalid, err)
		}
		return ctrl.Result{RequeueAfter: transientRequeueAfter}, nil
	}
}

func (r *OfferReconciler) desiredPlan(
	ctx context.Context,
	offer *billingv1alpha1.Offer,
) (openmeter.DesiredPlan, error) {
	name := offer.Annotations[billingv1alpha1.DisplayNameAnnotation]
	if name == "" {
		name = offer.Name
	}

	meterByName, err := r.meterAPINameIndex(ctx)
	if err != nil {
		return openmeter.DesiredPlan{}, err
	}

	var rateCards []om.RateCard
	usedKeys := make(map[string]struct{})
	// appendRateCard appends a rate card after enforcing key uniqueness.
	// OpenMeter rejects a plan whose phase contains two rate cards with the
	// same key (net error rate_card_duplicated_key), so a collision — e.g.
	// two usage rates that sanitize to the same feature key, or a flat fee
	// whose name collides with a feature key — is caught locally and surfaced
	// as a mapping error instead of a permanent OpenMeter 400.
	appendRateCard := func(key string, rc om.RateCard) error {
		if _, dup := usedKeys[key]; dup {
			return fmt.Errorf(
				"duplicate rate card key %q; two service pricings (or rates) on offer %q map to the same OpenMeter key", key, offer.Name)
		}
		usedKeys[key] = struct{}{}
		rateCards = append(rateCards, rc)
		return nil
	}

	for _, sp := range offer.Spec.ServicePricings {
		switch sp.Spec.ChargeType {
		case billingv1alpha1.ChargeTypeUsage:
			meterSlug, ok := meterByName[sp.Spec.Metric]
			if !ok {
				// The meter might not be synced yet, return transient error
				return openmeter.DesiredPlan{}, fmt.Errorf("meter %q not found for usage pricing %q", sp.Spec.Metric, sp.Name)
			}
			for _, rate := range sp.Spec.Rates {
				featureKey := fmt.Sprintf("%s_default", meterSlug)
				var advancedFilters map[string]om.FilterString
				if rate.Match != nil {
					// The feature key encodes the raw dimension and value into
					// regex-safe form; OpenMeter requires
					// `^[a-z0-9]+(?:_[a-z0-9]+)*$`. The filter below always uses
					// the raw value — the key is an identifier, the filter is
					// the actual match.
					featureKey = fmt.Sprintf("%s_%s_%s", meterSlug,
						sanitizeKeyPart(rate.Match.Dimension), sanitizeKeyPart(rate.Match.Value))
					val := rate.Match.Value
					advancedFilters = map[string]om.FilterString{
						rate.Match.Dimension: {
							Eq: &val,
						},
					}
				}

				feature, err := r.OpenMeterClient.EnsureFeature(ctx, openmeter.DesiredFeature{
					Key:                         featureKey,
					Name:                        featureKey,
					MeterSlug:                   meterSlug,
					AdvancedMeterGroupByFilters: advancedFilters,
				})
				if err != nil {
					return openmeter.DesiredPlan{}, fmt.Errorf("ensure feature %q: %w", featureKey, err)
				}

				rc := om.RateCardUsageBased{
					Type:           om.RateCardUsageBasedTypeUsageBased,
					Key:            feature.Key,
					Name:           sp.Spec.DisplayName,
					FeatureKey:     &feature.Key,
					BillingCadence: "P1M",
					Metadata: &om.Metadata{
						"miloapis.com/service-ref":  sp.Spec.ServiceRef,
						"miloapis.com/pricing-unit": sp.Spec.PricingUnit,
					},
				}
				if rc.Name == "" {
					rc.Name = featureKey
				}

				price := om.RateCardUsageBasedPrice{}
				if rate.Flat != "" {
					err := price.FromUnitPriceWithCommitments(om.UnitPriceWithCommitments{
						Type:   om.UnitPriceWithCommitmentsTypeUnit,
						Amount: om.Numeric(rate.Flat),
					})
					if err != nil {
						return openmeter.DesiredPlan{}, err
					}
				} else if len(rate.Tiered) > 0 {
					var tiers []om.PriceTier
					for _, t := range rate.Tiered {
						tier := om.PriceTier{
							UnitPrice: &om.UnitPrice{
								Amount: om.Numeric(t.Rate),
								Type:   om.UnitPriceTypeUnit,
							},
						}
						if t.UpTo != "" {
							v := om.Numeric(t.UpTo)
							tier.UpToAmount = &v
						}
						tiers = append(tiers, tier)
					}
					err := price.FromTieredPriceWithCommitments(om.TieredPriceWithCommitments{
						Type:  om.TieredPriceWithCommitmentsTypeTiered,
						Mode:  om.TieredPriceModeGraduated,
						Tiers: tiers,
					})
					if err != nil {
						return openmeter.DesiredPlan{}, err
					}
				}
				rc.Price = &price

				var wrapper om.RateCard
				if err := wrapper.FromRateCardUsageBased(rc); err != nil {
					return openmeter.DesiredPlan{}, err
				}
				if err := appendRateCard(feature.Key, wrapper); err != nil {
					return openmeter.DesiredPlan{}, err
				}
			}
		case billingv1alpha1.ChargeTypeOneTime, billingv1alpha1.ChargeTypeRecurring:
			rc := om.RateCardFlatFee{
				Type: om.RateCardFlatFeeTypeFlatFee,
				Key:  sanitizeKeyPart(sp.Name),
				Name: sp.Spec.DisplayName,
				Metadata: &om.Metadata{
					"miloapis.com/service-ref": sp.Spec.ServiceRef,
					"miloapis.com/trigger":     string(sp.Spec.Trigger),
				},
			}
			if rc.Name == "" {
				rc.Name = sp.Name
			}
			if sp.Spec.ChargeType == billingv1alpha1.ChargeTypeRecurring {
				cadence := "P1M"
				rc.BillingCadence = &cadence
			}
			price := om.FlatPriceWithPaymentTerm{
				Type:   om.FlatPriceWithPaymentTermTypeFlat,
				Amount: om.Numeric(sp.Spec.Amount),
			}
			rc.Price = &price

			var wrapper om.RateCard
			if err := wrapper.FromRateCardFlatFee(rc); err != nil {
				return openmeter.DesiredPlan{}, err
			}
			if err := appendRateCard(rc.Key, wrapper); err != nil {
				return openmeter.DesiredPlan{}, err
			}
		default:
			// A chargeType outside the three known values would silently
			// produce no rate cards and degrade the plan (OpenMeter rejects
			// a phase-less plan). ServicePricingSpec.chargeType is enum
			// validated, so this is only reachable when the snapshot was
			// produced by a version that knew a type we do not — surface it
			// loudly rather than half-syncing.
			return openmeter.DesiredPlan{}, fmt.Errorf(
				"unsupported charge type %q on service pricing %q", sp.Spec.ChargeType, sp.Name)
		}
	}

	if len(rateCards) == 0 {
		// Defensive: every known chargeType produces at least one rate card
		// (usage rates have MinItems=1), so this guards against a future
		// chargeType mapping that yields nothing — OpenMeter would reject a
		// plan with zero rate cards, and an empty plan is never the intent.
		return openmeter.DesiredPlan{}, fmt.Errorf(
			"offer %q produced no rate cards: nothing to sync to OpenMeter", offer.Name)
	}

	phase := om.PlanPhase{
		Key:       "default_phase",
		Name:      "Default Phase",
		RateCards: rateCards,
	}
	phases := []om.PlanPhase{phase}

	return openmeter.DesiredPlan{
		Key:         strings.ReplaceAll(string(offer.UID), "-", "_"),
		Name:        name,
		Description: offer.Name,
		Currency:    "USD",
		Phases:      phases,
	}, nil
}

// sanitizeKeyPart maps an arbitrary dimension or value string (which may
// carry dots, spaces, hyphens, or mixed case) onto a single OpenMeter-key
// safe segment. OpenMeter keys must match `^[a-z0-9]+(?:_[a-z0-9]+)*$`, so
// every run of characters outside [a-z0-9] collapses to one underscore,
// the input is lowercased, and surrounding underscore runs are dropped. The
// function never emits leading/trailing underscores, so joining sanitized
// segments with "_" always yields a valid key. The result is stable (same
// input -> same output), which is what keeps EnsureFeature idempotent.
func sanitizeKeyPart(s string) string {
	var b strings.Builder
	pendingUnderscore := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pendingUnderscore && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingUnderscore = false
			b.WriteRune(r)
		} else {
			pendingUnderscore = true
		}
	}
	return b.String()
}

// planReferencedFeatureKeys collects the feature keys referenced by a plan's
// usage rate cards. These are the features the plan's rate cards point at; on
// offer deletion they become garbage (unless another plan still references
// them) and are archived before the plan itself is deleted.
func planReferencedFeatureKeys(plan *om.Plan) []string {
	var keys []string
	for _, phase := range plan.Phases {
		for _, rc := range phase.RateCards {
			usage, err := rc.AsRateCardUsageBased()
			if err != nil {
				// Flat-fee and other rate cards carry no feature reference.
				continue
			}
			if usage.FeatureKey != nil && *usage.FeatureKey != "" {
				keys = append(keys, *usage.FeatureKey)
			}
		}
	}
	return keys
}

func (r *OfferReconciler) meterAPINameIndex(ctx context.Context) (map[string]string, error) {
	var list billingv1alpha1.MeterDefinitionList
	if err := r.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list MeterDefinitions: %w", err)
	}
	out := make(map[string]string, len(list.Items))
	for i := range list.Items {
		md := &list.Items[i]
		if md.Spec.MeterName == "" {
			continue
		}
		// The slug must be derived from spec.meterName — the same derivation
		// the MeterDefinition controller uses when it creates the meter
		// (see meterdefinition_controller.go). metadata.name is a distinct
		// Kubernetes resource name that almost never equals the canonical
		// reverse-DNS meterName, and deriving the slug from the wrong field
		// makes attribute-based features point at a meter slug that does not
		// exist, failing every feature create.
		out[md.Spec.MeterName] = string(openmeter.MeterSlug(md.Spec.MeterName))
	}
	return out, nil
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
		Complete(r)
}
