// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
)

// BindingBillingAccountRefField is the field index for listing
// BillingAccountBindings by the billing account they reference. The
// BillingAccountReconciler uses it to re-fetch the project set for a given
// account so the OpenMeter customer's usage attribution stays consistent as
// bindings come and go.
const BindingBillingAccountRefField = ".spec.billingAccountRef.name"

// EntitlementBillingAccountRefField and EntitlementOfferRefField index
// BillingEntitlements by the billing account and Offer they reference, so
// the BillingEntitlementReconciler can map account and Offer events to the
// entitlements they affect.
const (
	EntitlementBillingAccountRefField = ".spec.billingAccountRef.name"
	EntitlementOfferRefField          = ".spec.offerRef.name"
)

// AddIndexers installs the field indexers used by the openmeter-provider
// reconcilers. It accepts a FieldIndexer (rather than a Manager) so envtest
// setup can share the same wiring.
func AddIndexers(ctx context.Context, fi client.FieldIndexer) error {
	indexes := []struct {
		obj     client.Object
		field   string
		extract client.IndexerFunc
	}{
		{&billingv1alpha1.BillingAccountBinding{}, BindingBillingAccountRefField, func(obj client.Object) []string {
			binding, ok := obj.(*billingv1alpha1.BillingAccountBinding)
			if !ok {
				return nil
			}
			return []string{binding.Spec.BillingAccountRef.Name}
		}},
		{&billingv1alpha1.BillingEntitlement{}, EntitlementBillingAccountRefField, entitlementBillingAccountRef},
		{&billingv1alpha1.BillingEntitlement{}, EntitlementOfferRefField, entitlementOfferRef},
	}
	for _, idx := range indexes {
		if err := fi.IndexField(ctx, idx.obj, idx.field, idx.extract); err != nil {
			return err
		}
	}
	return nil
}

func entitlementBillingAccountRef(obj client.Object) []string {
	be, ok := obj.(*billingv1alpha1.BillingEntitlement)
	if !ok {
		return nil
	}
	return []string{be.Spec.BillingAccountRef.Name}
}

func entitlementOfferRef(obj client.Object) []string {
	be, ok := obj.(*billingv1alpha1.BillingEntitlement)
	if !ok {
		return nil
	}
	return []string{be.Spec.OfferRef.Name}
}
