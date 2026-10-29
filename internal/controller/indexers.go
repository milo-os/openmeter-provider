// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
)

// EntitlementBillingAccountRefField and EntitlementOfferRefField index
// BillingEntitlements by the billing account and Offer they reference, so
// the BillingEntitlementReconciler can map account and Offer events to the
// entitlements they affect.
const (
	EntitlementBillingAccountRefField = ".spec.billingAccountRef.name"
	EntitlementOfferRefField          = ".spec.offerRef.name"
	CreditGrantBillingAccountRefField = ".spec.billingAccountRef.name"
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
		{&billingv1alpha1.BillingEntitlement{}, EntitlementBillingAccountRefField, entitlementBillingAccountRef},
		{&billingv1alpha1.BillingEntitlement{}, EntitlementOfferRefField, entitlementOfferRef},
		{&billingv1alpha1.CreditGrant{}, CreditGrantBillingAccountRefField, creditGrantBillingAccountRef},
	}
	for _, idx := range indexes {
		if err := fi.IndexField(ctx, idx.obj, idx.field, idx.extract); err != nil {
			return err
		}
	}
	return nil
}

func creditGrantBillingAccountRef(obj client.Object) []string {
	cg, ok := obj.(*billingv1alpha1.CreditGrant)
	if !ok {
		return nil
	}
	return []string{cg.Spec.BillingAccountRef.Name}
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
