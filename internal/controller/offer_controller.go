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
					featureKey = fmt.Sprintf("%s_%s_%s", meterSlug, rate.Match.Dimension, rate.Match.Value)
					val := rate.Match.Value
					advancedFilters = map[string]om.FilterString{
						rate.Match.Dimension: {
							Eq: &val,
						},
					}
				}
				featureKey = strings.ReplaceAll(featureKey, "-", "_")
				featureKey = strings.ToLower(featureKey)

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
				rateCards = append(rateCards, wrapper)
			}
		case billingv1alpha1.ChargeTypeOneTime, billingv1alpha1.ChargeTypeRecurring:
			rc := om.RateCardFlatFee{
				Type: om.RateCardFlatFeeTypeFlatFee,
				Key:  strings.ReplaceAll(sp.Name, "-", "_"),
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
			rateCards = append(rateCards, wrapper)
		}
	}

	phase := om.PlanPhase{
		Key:       "default_phase",
		Name:      "Default Phase",
		RateCards: rateCards,
	}

	// OpenMeter requires phases to not be completely empty sometimes, but if it is empty, we still pass it.
	var phases []om.PlanPhase
	if len(rateCards) > 0 {
		phases = append(phases, phase)
	}

	return openmeter.DesiredPlan{
		Key:         strings.ReplaceAll(string(offer.UID), "-", "_"),
		Name:        name,
		Description: offer.Name,
		Currency:    "USD",
		Phases:      phases,
	}, nil
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
		// Assuming the slug logic remains the same for OpenMeter
		out[md.Spec.MeterName] = string(openmeter.MeterSlug(md.Name))
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
