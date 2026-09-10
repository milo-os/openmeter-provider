// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"slices"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

// partialFailureOpenMeterClient lets the customer cleanup succeed while the
// billing-profile cleanup keeps failing — the exact split observed in
// production when the profile delete was wedged.
type partialFailureOpenMeterClient struct {
	openmeter.Client // panics on any unimplemented method if called

	deleteCustomerCalls int
}

func (f *partialFailureOpenMeterClient) DeleteCustomer(_ context.Context, _ string) error {
	f.deleteCustomerCalls++
	return nil
}

func (f *partialFailureOpenMeterClient) DeleteBillingProfileCustomerOverride(_ context.Context, _ string) error {
	return nil
}

func (f *partialFailureOpenMeterClient) DeleteBillingProfile(_ context.Context, _ string) error {
	return errors.New("profile is referenced by customer overrides")
}

// TestReconcile_PersistsSucceededFinalizerWhenSiblingFails is the regression
// test for a bug review found and the production logs had been showing all
// along: "OpenMeter customer deleted" repeating for the same account, pass
// after pass.
//
// Finalizers.Finalize runs EVERY registered finalizer and can return both
// "A succeeded and was removed from the object" and "B failed". Checking the
// error before persisting threw away A's removal, so A's remote cleanup was
// re-run on every retry until B finally succeeded — re-issuing remote calls
// and re-emitting Deleted events indefinitely.
func TestReconcile_PersistsSucceededFinalizerWhenSiblingFails(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := billingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	now := metav1.Now()
	account := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "acme-account",
			Namespace:         "org-a",
			UID:               types.UID("uid-1"),
			DeletionTimestamp: &now,
			Finalizers:        []string{CustomerLinkFinalizer, BillingProfileFinalizer},
			Annotations: map[string]string{
				BillingProfileIDAnnotation:    "profile-123",
				OpenMeterCustomerIDAnnotation: "01M23AAAXB1MDSQTY0WWY9P0G8",
			},
		},
		Spec: billingv1alpha1.BillingAccountSpec{CurrencyCode: "USD"},
	}

	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(account).Build()
	omc := &partialFailureOpenMeterClient{}

	r := &BillingAccountReconciler{Client: k8s, OpenMeterClient: omc}
	r.Finalizers = finalizer.NewFinalizers()
	if err := r.Finalizers.Register(CustomerLinkFinalizer, &customerLinkFinalizer{OpenMeterClient: omc}); err != nil {
		t.Fatalf("register customer finalizer: %v", err)
	}
	if err := r.Finalizers.Register(BillingProfileFinalizer, &billingProfileFinalizer{OpenMeterClient: omc}); err != nil {
		t.Fatalf("register billing profile finalizer: %v", err)
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "acme-account", Namespace: "org-a"}}

	// First pass: the customer cleanup succeeds, the profile cleanup fails.
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("Reconcile: expected the wedged billing-profile finalizer to surface an error")
	}

	var afterFirst billingv1alpha1.BillingAccount
	if err := k8s.Get(context.Background(), req.NamespacedName, &afterFirst); err != nil {
		t.Fatalf("get after first reconcile: %v", err)
	}
	if slices.Contains(afterFirst.Finalizers, CustomerLinkFinalizer) {
		t.Errorf("finalizers = %v, want %q dropped — its cleanup succeeded, so the removal must be persisted even though a sibling failed",
			afterFirst.Finalizers, CustomerLinkFinalizer)
	}
	if !slices.Contains(afterFirst.Finalizers, BillingProfileFinalizer) {
		t.Errorf("finalizers = %v, want %q retained so its cleanup is retried", afterFirst.Finalizers, BillingProfileFinalizer)
	}

	// Second pass: the already-completed customer cleanup must NOT run again.
	callsAfterFirst := omc.deleteCustomerCalls
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("Reconcile: expected the billing-profile finalizer to still be failing")
	}
	if omc.deleteCustomerCalls != callsAfterFirst {
		t.Errorf("DeleteCustomer called %d times by the second pass (was %d) — a finalizer that already succeeded is being re-run every retry",
			omc.deleteCustomerCalls, callsAfterFirst)
	}
}

// TestReconcile_RemovesAllFinalizersWhenCleanupSucceeds is the happy path:
// with nothing failing, both finalizers come off and the object is released.
func TestReconcile_RemovesAllFinalizersWhenCleanupSucceeds(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := billingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	now := metav1.Now()
	account := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "acme-account",
			Namespace:         "org-a",
			UID:               types.UID("uid-1"),
			DeletionTimestamp: &now,
			Finalizers:        []string{CustomerLinkFinalizer, BillingProfileFinalizer},
			Annotations: map[string]string{
				BillingProfileIDAnnotation:    "profile-123",
				OpenMeterCustomerIDAnnotation: "01M23AAAXB1MDSQTY0WWY9P0G8",
			},
		},
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(account).Build()
	omc := &allSucceedOpenMeterClient{}

	r := &BillingAccountReconciler{Client: k8s, OpenMeterClient: omc}
	r.Finalizers = finalizer.NewFinalizers()
	_ = r.Finalizers.Register(CustomerLinkFinalizer, &customerLinkFinalizer{OpenMeterClient: omc})
	_ = r.Finalizers.Register(BillingProfileFinalizer, &billingProfileFinalizer{OpenMeterClient: omc})

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "acme-account", Namespace: "org-a"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Once the last finalizer is dropped the API server completes the
	// deletion, so NotFound is the expected terminal state. If the object
	// does still exist, it must at least carry no finalizers.
	var after billingv1alpha1.BillingAccount
	switch err := k8s.Get(context.Background(), req.NamespacedName, &after); {
	case apierrors.IsNotFound(err):
		// Fully deleted — nothing left to assert.
	case err != nil:
		t.Fatalf("get after reconcile: %v", err)
	case len(after.Finalizers) > 0:
		t.Errorf("finalizers = %v, want all removed", after.Finalizers)
	}
}

type allSucceedOpenMeterClient struct {
	openmeter.Client
}

func (f *allSucceedOpenMeterClient) DeleteCustomer(_ context.Context, _ string) error { return nil }
func (f *allSucceedOpenMeterClient) DeleteBillingProfileCustomerOverride(_ context.Context, _ string) error {
	return nil
}
func (f *allSucceedOpenMeterClient) DeleteBillingProfile(_ context.Context, _ string) error {
	return nil
}
