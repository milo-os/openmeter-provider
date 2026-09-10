// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

func TestDesiredBillingProfileFromAccount_NilPaymentTermsUsesDefaults(t *testing.T) {
	account := baseBillingAccount()
	account.Spec.PaymentTerms = nil

	got := desiredBillingProfileFromAccount(account, "test-account-key")
	if got.NetDays != 30 || got.InvoiceFrequency != "Monthly" || got.InvoiceDayOfMonth != 1 {
		t.Errorf("desiredBillingProfileFromAccount() = %+v, want the billingv1alpha1.PaymentTerms CRD defaults (30/Monthly/1)", got)
	}
}

func TestDesiredBillingProfileFromAccount_ExplicitPaymentTerms(t *testing.T) {
	account := baseBillingAccount()
	account.Spec.PaymentTerms = &billingv1alpha1.PaymentTerms{
		NetDays:           60,
		InvoiceFrequency:  "Quarterly",
		InvoiceDayOfMonth: 15,
	}

	got := desiredBillingProfileFromAccount(account, "test-account-key")
	if got.NetDays != 60 || got.InvoiceFrequency != "Quarterly" || got.InvoiceDayOfMonth != 15 {
		t.Errorf("desiredBillingProfileFromAccount() = %+v, want explicit spec.paymentTerms values", got)
	}
}

// fakeBillingProfileOpenMeterClient is a minimal openmeter.Client double for
// exercising billingProfileFinalizer.Finalize's branches without a real
// server. Only DeleteBillingProfileCustomerOverride/DeleteBillingProfile are
// ever called by Finalize (it no longer calls GetCustomer at all — see
// OpenMeterCustomerIDAnnotation's doc comment for why); the rest panic if
// called, so an unexpected call fails the test loudly instead of silently
// no-opping.
type fakeBillingProfileOpenMeterClient struct {
	openmeter.Client // panics on any unimplemented method if called

	deleteOverrideCalls []string
	deleteOverrideErr   error
	deleteProfileCalls  []string
	deleteProfileErr    error
}

func (f *fakeBillingProfileOpenMeterClient) DeleteBillingProfileCustomerOverride(_ context.Context, customerID string) error {
	f.deleteOverrideCalls = append(f.deleteOverrideCalls, customerID)
	return f.deleteOverrideErr
}

func (f *fakeBillingProfileOpenMeterClient) DeleteBillingProfile(_ context.Context, id string) error {
	f.deleteProfileCalls = append(f.deleteProfileCalls, id)
	return f.deleteProfileErr
}

func accountWithProfileAnnotations() *billingv1alpha1.BillingAccount {
	account := baseBillingAccount()
	account.Annotations = map[string]string{
		BillingProfileIDAnnotation:    "profile-123",
		OpenMeterCustomerIDAnnotation: "01M23AAAXB1MDSQTY0WWY9P0G8",
	}
	return account
}

func TestBillingProfileFinalizer_DeletesOverrideThenProfile(t *testing.T) {
	fake := &fakeBillingProfileOpenMeterClient{}
	f := &billingProfileFinalizer{OpenMeterClient: fake}

	if _, err := f.Finalize(context.Background(), accountWithProfileAnnotations()); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	if len(fake.deleteOverrideCalls) != 1 || fake.deleteOverrideCalls[0] != "01M23AAAXB1MDSQTY0WWY9P0G8" {
		t.Errorf("DeleteBillingProfileCustomerOverride calls = %v, want [01M23AAAXB1MDSQTY0WWY9P0G8] (from OpenMeterCustomerIDAnnotation, not re-resolved via GetCustomer)", fake.deleteOverrideCalls)
	}
	if len(fake.deleteProfileCalls) != 1 || fake.deleteProfileCalls[0] != "profile-123" {
		t.Errorf("DeleteBillingProfile calls = %v, want [profile-123]", fake.deleteProfileCalls)
	}
}

// TestBillingProfileFinalizer_DeletesOverrideEvenAfterCustomerIsGone is the
// exact bug this design fixes: customerLinkFinalizer and this finalizer run
// independently, in randomized order, in the same call. If Finalize had
// re-resolved the customer's internal id via GetCustomer here (as an
// earlier version did), a customerLinkFinalizer that already ran in this
// same pass would make GetCustomer 404 — even though the override itself is
// still alive on OpenMeter's side and still blocks BillingProfile deletion
// ("profile is referenced by customer overrides"). Reading the annotation
// instead sidesteps that: it doesn't matter whether the customer is already
// gone, the id was captured before deletion ever started.
func TestBillingProfileFinalizer_DeletesOverrideEvenAfterCustomerIsGone(t *testing.T) {
	fake := &fakeBillingProfileOpenMeterClient{}
	f := &billingProfileFinalizer{OpenMeterClient: fake}

	if _, err := f.Finalize(context.Background(), accountWithProfileAnnotations()); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	if len(fake.deleteOverrideCalls) != 1 {
		t.Errorf("DeleteBillingProfileCustomerOverride calls = %v, want exactly one call regardless of the customer's own deletion state", fake.deleteOverrideCalls)
	}
	if len(fake.deleteProfileCalls) != 1 || fake.deleteProfileCalls[0] != "profile-123" {
		t.Errorf("DeleteBillingProfile calls = %v, want [profile-123]", fake.deleteProfileCalls)
	}
}

func TestBillingProfileFinalizer_NoCustomerIDAnnotationSkipsOverrideButDeletesProfile(t *testing.T) {
	fake := &fakeBillingProfileOpenMeterClient{}
	f := &billingProfileFinalizer{OpenMeterClient: fake}

	account := baseBillingAccount()
	account.Annotations = map[string]string{
		BillingProfileIDAnnotation: "profile-123",
		// OpenMeterCustomerIDAnnotation deliberately absent: simulates an
		// account deleted before its first successful reconcile past
		// EnsureCustomer ever persisted it.
	}

	if _, err := f.Finalize(context.Background(), account); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if len(fake.deleteOverrideCalls) != 0 {
		t.Errorf("DeleteBillingProfileCustomerOverride calls = %v, want none (no customer id was ever persisted)", fake.deleteOverrideCalls)
	}
	if len(fake.deleteProfileCalls) != 1 || fake.deleteProfileCalls[0] != "profile-123" {
		t.Errorf("DeleteBillingProfile calls = %v, want [profile-123] (must still run despite the skipped override)", fake.deleteProfileCalls)
	}
}

func TestBillingProfileFinalizer_NoProfileAnnotationIsNoop(t *testing.T) {
	fake := &fakeBillingProfileOpenMeterClient{}
	f := &billingProfileFinalizer{OpenMeterClient: fake}

	account := baseBillingAccount()
	if _, err := f.Finalize(context.Background(), account); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if len(fake.deleteProfileCalls) != 0 {
		t.Errorf("DeleteBillingProfile calls = %v, want none (no BillingProfileIDAnnotation was ever set)", fake.deleteProfileCalls)
	}
}
