// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/log"

	om "github.com/openmeterio/openmeter/api/client/go"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

const (
	// BillingProfileFinalizer blocks deletion of a BillingAccount until its
	// private per-account BillingProfile (and the customer override
	// pointing at it) has been removed. Registered independently of
	// CustomerLinkFinalizer (see SetupWithManager), but its cleanup only
	// starts once CustomerLinkFinalizer is gone — see billingProfileFinalizer.
	BillingProfileFinalizer = "openmeter.miloapis.com/billing-profile"

	// BillingProfileIDAnnotation records the OpenMeter BillingProfile id
	// created for this BillingAccount. BillingProfile has no external "key"
	// field the way Customer/Meter do, so the id is persisted here as a
	// fast path. An annotation is metadata-only (not spec/status), so
	// writing it doesn't compete with billing's own controller for
	// ownership of any field it cares about.
	//
	// It is NOT trusted on its own: the account's users can edit
	// annotations, so a profile id read from here is only adopted, updated,
	// or deleted after the profile's own metadata proves it belongs to this
	// account (openmeter.BillingProfileOwnershipMismatch). Otherwise the
	// lookup by account key takes over.
	BillingProfileIDAnnotation = "openmeter.miloapis.com/billing-profile-id"

	// OpenMeterCustomerIDAnnotation records OpenMeter's internal customer id
	// (a ULID) for this BillingAccount, for support lookups. It is rewritten
	// from EnsureCustomer's response on every reconcile, and never trusted
	// as input: the account's users can edit annotations, so the finalizer
	// resolves the customer ids whose overrides it removes from OpenMeter
	// itself, by the account's customer key (openmeter.Client's
	// CustomerIDsByKey, which also finds the customer after it has been
	// soft-deleted — the override outlives it and still blocks deleting the
	// BillingProfile), and only reports an annotation that disagrees.
	OpenMeterCustomerIDAnnotation = "openmeter.miloapis.com/customer-id"

	// EventReasonBillingProfileDrift is the Warning event reason for a
	// billing-profile problem that needs an operator: a refused
	// BillingProfileIDAnnotation/OpenMeterCustomerIDAnnotation, or apps
	// that differ from the default profile's.
	EventReasonBillingProfileDrift = "BillingProfileDrift"
)

// errWaitingForCustomerLink is billingProfileFinalizer's answer while the
// account still carries CustomerLinkFinalizer. It is not a failure — the
// cleanup is simply not due yet — so it is returned wrapped in a
// FinalizerWaitingError, which Reconcile turns into a quiet requeue.
var errWaitingForCustomerLink = errors.New("waiting for the OpenMeter customer to be deleted before removing its billing profile")

// desiredBillingProfileFromAccount translates a BillingAccount's payment
// terms into the openmeter-client DesiredBillingProfile shape. customerKey
// (the account's UID) becomes AccountKey, which EnsureBillingProfile uses
// to recognize a profile it already created for this account even if
// BillingProfileIDAnnotation never got persisted.
//
// Terms come from billing's model, not from spec.paymentTerms: billing
// v0.3.10 holds spec.paymentTerms to the standard terms (its webhook
// rejects anything else, since the account's own users can edit the spec)
// and publishes staff-granted terms from the active BillingArrangement on
// status.paymentArrangement. So:
//
//   - no arrangement (or one that is not of type Invoice): billing's
//     standard terms — net 30, monthly, on the 1st — and the default
//     profile's collection method (card);
//   - an Invoice arrangement: its invoice.netDays (defaulted to 30 by the
//     CRD) and collection by sending the invoice, since the account pays by
//     bank transfer/ACH/wire rather than by card.
//
// billing writes status.paymentArrangement on the BillingAccount itself,
// which the reconciler already watches without predicates, so arrangement
// changes trigger a reconcile without any extra watch.
func desiredBillingProfileFromAccount(account *billingv1alpha1.BillingAccount, customerKey openmeter.CustomerKey) openmeter.DesiredBillingProfile {
	desired := openmeter.DesiredBillingProfile{
		AccountKey:        customerKey,
		Name:              fmt.Sprintf("%s/%s payment terms", account.Namespace, account.Name),
		NetDays:           billingv1alpha1.StandardPaymentTermsNetDays,
		InvoiceFrequency:  billingv1alpha1.StandardPaymentTermsInvoiceFrequency,
		InvoiceDayOfMonth: billingv1alpha1.StandardPaymentTermsInvoiceDayOfMonth,
	}
	if arrangement := account.Status.PaymentArrangement; arrangement != nil &&
		arrangement.Type == billingv1alpha1.BillingArrangementTypeInvoice && arrangement.Invoice != nil {
		if arrangement.Invoice.NetDays > 0 {
			desired.NetDays = arrangement.Invoice.NetDays
		}
		desired.CollectionMethod = om.CollectionMethodSendInvoice
	}
	return desired
}

// reconcileBillingProfile ensures a private BillingProfile exists for
// account reflecting its payment terms, and that the OpenMeter customer
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
	customerKey openmeter.CustomerKey,
	openMeterCustomerID openmeter.CustomerID,
) error {
	logger := log.FromContext(ctx)
	desired := desiredBillingProfileFromAccount(account, customerKey)
	existingID := account.Annotations[BillingProfileIDAnnotation]

	result, err := r.OpenMeterClient.EnsureBillingProfile(ctx, existingID, desired)
	if err != nil {
		return fmt.Errorf("ensure billing profile: %w", err)
	}
	if result.IgnoredExistingID != "" {
		msg := fmt.Sprintf("ignoring annotation %s=%s: %s; using the profile found by account key instead",
			BillingProfileIDAnnotation, result.IgnoredExistingID, result.IgnoredExistingReason)
		logger.Info(msg)
		r.warnBillingProfileDrift(account, msg)
	}
	if result.AppsDrift != "" {
		logger.Info("billing profile apps drifted from the default profile", "drift", result.AppsDrift)
		r.warnBillingProfileDrift(account, result.AppsDrift)
	}
	profile := result.Profile

	if profile.Id != existingID || account.Annotations[OpenMeterCustomerIDAnnotation] != string(openMeterCustomerID) {
		base := account.DeepCopy()
		if account.Annotations == nil {
			account.Annotations = map[string]string{}
		}
		account.Annotations[BillingProfileIDAnnotation] = profile.Id
		account.Annotations[OpenMeterCustomerIDAnnotation] = string(openMeterCustomerID)
		if err := r.Patch(ctx, account, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("annotate billing profile id: %w", err)
		}
	}

	if err := r.OpenMeterClient.UpsertBillingProfileCustomerOverride(ctx, openMeterCustomerID, profile.Id); err != nil {
		return fmt.Errorf("upsert billing profile customer override: %w", err)
	}
	return nil
}

func (r *BillingAccountReconciler) warnBillingProfileDrift(account *billingv1alpha1.BillingAccount, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(account, "Warning", EventReasonBillingProfileDrift, msg)
	}
}

// billingProfileFinalizer deletes a BillingAccount's private BillingProfile
// (and the customer overrides pointing at it) when the account is deleted.
// Registered under BillingProfileFinalizer via finalizer.Finalizers,
// independent of customerLinkFinalizer.
//
// Ordering matters even though pkg/finalizer runs the two in randomized
// (map) order: customerLinkFinalizer invoices the customer's pending lines
// before deleting the customer, and OpenMeter resolves the customer's
// billing profile through the override at that moment. Removing the
// override (and profile) first would issue those final invoices under the
// org default profile's terms instead of the account's. So this finalizer
// does nothing while CustomerLinkFinalizer is still on the object; once
// the customer is gone, it removes the overrides, then the profiles.
type billingProfileFinalizer struct {
	OpenMeterClient openmeter.Client
	// Recorder, when set, receives Warning events for refused annotations.
	Recorder record.EventRecorder
}

// Finalize removes every override of the account's customers, then every
// billing profile belonging to the account. Each delete tolerates NotFound
// (already gone); any other failure keeps the finalizer in place and
// returns the error so controller-runtime's rate-limited backoff retries
// it.
//
// Neither step trusts the account's annotations (see
// BillingProfileIDAnnotation and OpenMeterCustomerIDAnnotation): the
// customers are resolved by the account's customer key, deleted ones
// included, and the profiles are the annotated one when its metadata
// proves it belongs to the account plus every profile tagged with the
// account's key. That lookup also covers an account deleted between
// creating its profile and persisting the annotation, which would
// otherwise orphan the profile and its override.
func (f *billingProfileFinalizer) Finalize(ctx context.Context, obj client.Object) (finalizer.Result, error) {
	account, ok := obj.(*billingv1alpha1.BillingAccount)
	if !ok {
		return finalizer.Result{}, fmt.Errorf("billingProfileFinalizer: object is not a BillingAccount (%T)", obj)
	}
	if controllerutil.ContainsFinalizer(account, CustomerLinkFinalizer) {
		// Slow requeue: removing CustomerLinkFinalizer updates the object,
		// which triggers the next pass immediately anyway.
		return finalizer.Result{}, NewFinalizerWaiting("WaitingForCustomerLink", permanentRequeueAfter, errWaitingForCustomerLink)
	}
	accountKey := openmeter.CustomerKey(account.UID)

	customerIDs, err := f.OpenMeterClient.CustomerIDsByKey(ctx, accountKey)
	if err != nil {
		return finalizer.Result{}, fmt.Errorf("resolve customers of billing account: %w", err)
	}
	if annotated := openmeter.CustomerID(account.Annotations[OpenMeterCustomerIDAnnotation]); annotated != "" && !slices.Contains(customerIDs, annotated) {
		f.warn(ctx, account, fmt.Sprintf("ignoring annotation %s=%s: not a customer of this billing account",
			OpenMeterCustomerIDAnnotation, annotated))
	}
	for _, customerID := range customerIDs {
		if err := f.OpenMeterClient.DeleteBillingProfileCustomerOverride(ctx, customerID); err != nil {
			return finalizer.Result{}, fmt.Errorf("delete billing profile customer override of %s: %w", customerID, err)
		}
	}

	profileIDs, err := f.accountBillingProfileIDs(ctx, account, accountKey)
	if err != nil {
		return finalizer.Result{}, err
	}
	for _, profileID := range profileIDs {
		if err := f.OpenMeterClient.DeleteBillingProfile(ctx, profileID); err != nil {
			return finalizer.Result{}, fmt.Errorf("delete billing profile %s: %w", profileID, err)
		}
	}
	return finalizer.Result{}, nil
}

// accountBillingProfileIDs returns the ids of the live billing profiles
// that belong to account: the annotated one when its metadata vouches for
// it, and every profile tagged with accountKey.
func (f *billingProfileFinalizer) accountBillingProfileIDs(
	ctx context.Context,
	account *billingv1alpha1.BillingAccount,
	accountKey openmeter.CustomerKey,
) ([]string, error) {
	var ids []string
	if annotated := account.Annotations[BillingProfileIDAnnotation]; annotated != "" {
		profile, err := f.OpenMeterClient.GetBillingProfile(ctx, annotated)
		switch {
		case errors.Is(err, openmeter.ErrBillingProfileNotFound):
			// Already deleted (or never existed); the key lookup below
			// still catches any other profile of this account.
		case err != nil:
			return nil, fmt.Errorf("get billing profile %s: %w", annotated, err)
		default:
			if reason := openmeter.BillingProfileOwnershipMismatch(profile, accountKey); reason != "" {
				f.warn(ctx, account, fmt.Sprintf("ignoring annotation %s=%s: %s",
					BillingProfileIDAnnotation, annotated, reason))
			} else {
				ids = append(ids, profile.Id)
			}
		}
	}

	found, err := f.OpenMeterClient.FindBillingProfilesByAccountKey(ctx, accountKey)
	if err != nil {
		return nil, fmt.Errorf("find billing profiles of billing account: %w", err)
	}
	for _, profile := range found {
		if !slices.Contains(ids, profile.Id) {
			ids = append(ids, profile.Id)
		}
	}
	return ids, nil
}

func (f *billingProfileFinalizer) warn(ctx context.Context, account *billingv1alpha1.BillingAccount, msg string) {
	log.FromContext(ctx).Info(msg)
	if f.Recorder != nil {
		f.Recorder.Event(account, "Warning", EventReasonBillingProfileDrift, msg)
	}
}
