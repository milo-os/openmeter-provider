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

// AddIndexers installs the field indexers used by the openmeter-provider
// reconcilers. It accepts a FieldIndexer (rather than a Manager) so envtest
// setup can share the same wiring.
func AddIndexers(ctx context.Context, fi client.FieldIndexer) error {
	return fi.IndexField(
		ctx,
		&billingv1alpha1.BillingAccountBinding{},
		BindingBillingAccountRefField,
		func(obj client.Object) []string {
			binding, ok := obj.(*billingv1alpha1.BillingAccountBinding)
			if !ok {
				return nil
			}
			return []string{binding.Spec.BillingAccountRef.Name}
		},
	)
}
