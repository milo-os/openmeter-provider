// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

const (
	// BillingProfileFinalizer blocks deletion of a BillingAccount until its
	// private per-account BillingProfile (and the customer override
	// pointing at it) has been removed. Registered independently of
	// CustomerLinkFinalizer (see SetupWithManager) so the two cleanups
	// retry independently of one another.
	BillingProfileFinalizer = "openmeter.miloapis.com/billing-profile"

	// BillingProfileIDAnnotation records the OpenMeter BillingProfile id
	// created for this BillingAccount. BillingProfile has no external "key"
	// field the way Customer/Meter do — unlike those, we cannot rediscover
	// it deterministically from the BillingAccount alone, so the id must be
	// persisted somewhere. An annotation is metadata-only (not spec/status),
	// so writing it doesn't compete with billing's own controller for
	// ownership of any field it cares about.
	BillingProfileIDAnnotation = "openmeter.miloapis.com/billing-profile-id"

	// OpenMeterCustomerIDAnnotation records OpenMeter's internal customer id
	// (a ULID) for this BillingAccount — required because
	// UpsertBillingProfileCustomerOverride/DeleteBillingProfileCustomerOverride
	// don't accept the external key the way EnsureCustomer/GetCustomer do
	// (see UpsertBillingProfileCustomerOverride's doc comment).
	//
	// Persisted here (rather than re-resolved via GetCustomer at delete
	// time) because GetCustomer-by-key excludes soft-deleted customers —
	// confirmed live: customerLinkFinalizer and billingProfileFinalizer run
	// independently in randomized order (see billingProfileFinalizer's doc
	// comment), and when customerLinkFinalizer's DeleteCustomer happens to
	// run first in the same pass, a subsequent GetCustomer 404s even though
	// the override itself is very much still alive on OpenMeter's side and
	// still blocks BillingProfile deletion ("profile is referenced by
	// customer overrides") — OpenMeter does not cascade-delete an override
	// when its customer is soft-deleted. An earlier version of this code
	// treated "customer not found" as "override is moot, skip it", which
	// left the override (and therefore the BillingProfile) permanently
	// undeletable. Persisting the id here, set once during the normal
	// reconcile right alongside BillingProfileIDAnnotation, sidesteps the
	// whole race.
	OpenMeterCustomerIDAnnotation = "openmeter.miloapis.com/customer-id"
)

// defaultPaymentTerms mirrors billingv1alpha1.PaymentTerms' own
// +kubebuilder:default values, used when spec.paymentTerms is nil (which
// should not normally happen once the CRD default has applied, but a
// defensive fallback keeps desiredBillingProfile total).
var defaultPaymentTerms = billingv1alpha1.PaymentTerms{
	NetDays:           30,
	InvoiceFrequency:  "Monthly",
	InvoiceDayOfMonth: 1,
}

// desiredBillingProfileFromAccount translates a BillingAccount's
// spec.paymentTerms into the openmeter-client DesiredBillingProfile shape.
// customerKey (the account's UID) becomes AccountKey, which
// EnsureBillingProfile uses to recognize a profile it already created for
// this account even if BillingProfileIDAnnotation never got persisted.
func desiredBillingProfileFromAccount(account *billingv1alpha1.BillingAccount, customerKey string) openmeter.DesiredBillingProfile {
	terms := defaultPaymentTerms
	if account.Spec.PaymentTerms != nil {
		terms = *account.Spec.PaymentTerms
	}
	return openmeter.DesiredBillingProfile{
		AccountKey:        customerKey,
		Name:              fmt.Sprintf("%s/%s payment terms", account.Namespace, account.Name),
		NetDays:           terms.NetDays,
		InvoiceFrequency:  terms.InvoiceFrequency,
		InvoiceDayOfMonth: terms.InvoiceDayOfMonth,
	}
}

// reconcileBillingProfile ensures a private BillingProfile exists for
// account reflecting its spec.paymentTerms, and that the OpenMeter customer
// is overridden to use it. The profile id is persisted on
// BillingProfileIDAnnotation via a plain metadata patch — cheap and safe to
// call every reconcile since Patch is a no-op when nothing changed.
//
// customerKey is our own external key (account.UID), used only for the
// AccountKey dedup tag on DesiredBillingProfile — it's unrelated to
// OpenMeter's own id scheme. openMeterCustomerID is OpenMeter's internal
// customer id (the Id field on the om.Customer EnsureCustomer returned in
// Reconcile), required because UpsertBillingProfileCustomerOverride's
// endpoint does not accept the external key (see its doc comment).
func (r *BillingAccountReconciler) reconcileBillingProfile(
	ctx context.Context,
	account *billingv1alpha1.BillingAccount,
	customerKey string,
	openMeterCustomerID string,
) error {
	desired := desiredBillingProfileFromAccount(account, customerKey)
	existingID := account.Annotations[BillingProfileIDAnnotation]

	profile, err := r.OpenMeterClient.EnsureBillingProfile(ctx, existingID, desired)
	if err != nil {
		return fmt.Errorf("ensure billing profile: %w", err)
	}

	if profile.Id != existingID || account.Annotations[OpenMeterCustomerIDAnnotation] != openMeterCustomerID {
		base := account.DeepCopy()
		if account.Annotations == nil {
			account.Annotations = map[string]string{}
		}
		account.Annotations[BillingProfileIDAnnotation] = profile.Id
		account.Annotations[OpenMeterCustomerIDAnnotation] = openMeterCustomerID
		if err := r.Patch(ctx, account, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("annotate billing profile id: %w", err)
		}
	}

	if err := r.OpenMeterClient.UpsertBillingProfileCustomerOverride(ctx, openMeterCustomerID, profile.Id); err != nil {
		return fmt.Errorf("upsert billing profile customer override: %w", err)
	}
	return nil
}

// billingProfileFinalizer deletes a BillingAccount's private BillingProfile
// (and the customer override pointing at it) when the account is deleted.
// Registered under BillingProfileFinalizer via finalizer.Finalizers,
// independent of customerLinkFinalizer — each cleanup is idempotent and
// safe regardless of which finalizer's Finalize runs (or retries) first.
type billingProfileFinalizer struct {
	OpenMeterClient openmeter.Client
}

// Finalize removes the customer's billing profile override, then the
// billing profile itself. Both steps tolerate NotFound (already gone) via
// their respective DeleteX methods; any other failure keeps the finalizer
// in place and returns the error so controller-runtime's default
// rate-limited backoff retries it.
//
// Reads OpenMeterCustomerIDAnnotation rather than resolving the customer's
// internal id via GetCustomer here — see that annotation's doc comment for
// why: GetCustomer-by-key would 404 once customerLinkFinalizer's
// DeleteCustomer has already run (the two finalizers run independently, in
// randomized order, in the same call), even though the override itself is
// still very much alive on OpenMeter's side and still blocks BillingProfile
// deletion.
func (f *billingProfileFinalizer) Finalize(ctx context.Context, obj client.Object) (finalizer.Result, error) {
	account, ok := obj.(*billingv1alpha1.BillingAccount)
	if !ok {
		return finalizer.Result{}, fmt.Errorf("billingProfileFinalizer: object is not a BillingAccount (%T)", obj)
	}

	if customerID := account.Annotations[OpenMeterCustomerIDAnnotation]; customerID != "" {
		if err := f.OpenMeterClient.DeleteBillingProfileCustomerOverride(ctx, customerID); err != nil {
			return finalizer.Result{}, fmt.Errorf("delete billing profile customer override: %w", err)
		}
	}
	// An empty annotation means the account was deleted before its first
	// successful reconcile past EnsureCustomer (the annotation is set
	// alongside BillingProfileIDAnnotation) — no override could have been
	// created either, so there's nothing to clean up here.

	profileID := account.Annotations[BillingProfileIDAnnotation]
	if profileID == "" {
		// Never got as far as creating one (e.g. deleted before the first
		// successful reconcile past finalizer-add) — nothing to clean up.
		return finalizer.Result{}, nil
	}
	if err := f.OpenMeterClient.DeleteBillingProfile(ctx, profileID); err != nil {
		return finalizer.Result{}, fmt.Errorf("delete billing profile %s: %w", profileID, err)
	}
	return finalizer.Result{}, nil
}
