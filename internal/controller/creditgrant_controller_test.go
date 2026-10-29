// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	om "github.com/openmeterio/openmeter/api/client/go"
	omv3 "github.com/openmeterio/openmeter/api/v3"
	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
	"k8s.io/utils/ptr"
)

type fakeCreditGrantOpenMeterClient struct {
	openmeter.Client

	customer       om.Customer
	customerErr    error
	ensureGrant    omv3.BillingCreditGrant
	ensureGrantErr error
	voidErr        error

	ensureCalls []openmeter.DesiredCreditGrant
	voidCalls   []string
}

func (f *fakeCreditGrantOpenMeterClient) GetCustomer(_ context.Context, key openmeter.CustomerKey) (om.Customer, error) {
	if f.customerErr != nil {
		return om.Customer{}, f.customerErr
	}
	return f.customer, nil
}

func (f *fakeCreditGrantOpenMeterClient) EnsureCreditGrant(_ context.Context, _ openmeter.CustomerID, desired openmeter.DesiredCreditGrant) (omv3.BillingCreditGrant, error) {
	f.ensureCalls = append(f.ensureCalls, desired)
	if f.ensureGrantErr != nil {
		return omv3.BillingCreditGrant{}, f.ensureGrantErr
	}
	return f.ensureGrant, nil
}

func (f *fakeCreditGrantOpenMeterClient) VoidCreditGrant(_ context.Context, _ openmeter.CustomerID, grantID string) error {
	f.voidCalls = append(f.voidCalls, grantID)
	return f.voidErr
}

func (f *fakeCreditGrantOpenMeterClient) ListCreditGrants(_ context.Context, _ openmeter.CustomerID) ([]omv3.BillingCreditGrant, error) {
	if f.ensureGrant.Id != "" {
		return []omv3.BillingCreditGrant{f.ensureGrant}, nil
	}
	return nil, nil
}

func setupCreditGrantScheme(t *testing.T) *runtime.Scheme {
	s := runtime.NewScheme()
	if err := billingv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

func TestCreditGrantReconciler_BillingAccountNotFound(t *testing.T) {
	scheme := setupCreditGrantScheme(t)
	grant := &billingv1alpha1.CreditGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-grant",
			Namespace:  "org-1",
			Finalizers: []string{CreditGrantFinalizer},
		},
		Spec: billingv1alpha1.CreditGrantSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: "missing-account"},
			Amount:            "100.00",
			Currency:          "USD",
			Reason:            billingv1alpha1.CreditGrantReasonPromotional,
			Description:       "welcome",
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(grant).WithStatusSubresource(grant).Build()
	omc := &fakeCreditGrantOpenMeterClient{}
	r := &CreditGrantReconciler{
		Client:          cl,
		OpenMeterClient: omc,
		Recorder:        record.NewFakeRecorder(10),
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "org-1", Name: "test-grant"},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	updated := &billingv1alpha1.CreditGrant{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "org-1", Name: "test-grant"}, updated); err != nil {
		t.Fatalf("Get updated: %v", err)
	}
	if updated.Status.Phase != billingv1alpha1.CreditGrantPhaseFailed {
		t.Errorf("status.phase = %s, want Failed", updated.Status.Phase)
	}
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, billingv1alpha1.CreditGrantConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != creditGrantReasonBillingAccountNotFound {
		t.Errorf("Ready condition = %+v, want False/BillingAccountNotFound", ready)
	}
}

func TestCreditGrantReconciler_WaitingForCustomer(t *testing.T) {
	scheme := setupCreditGrantScheme(t)
	account := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "main-account",
			Namespace: "org-1",
			UID:       "account-uid-123",
		},
	}
	grant := &billingv1alpha1.CreditGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-grant",
			Namespace:  "org-1",
			Finalizers: []string{CreditGrantFinalizer},
		},
		Spec: billingv1alpha1.CreditGrantSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: "main-account"},
			Amount:            "100.00",
			Currency:          "USD",
			Reason:            billingv1alpha1.CreditGrantReasonPromotional,
			Description:       "welcome",
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(account, grant).WithStatusSubresource(grant).Build()
	omc := &fakeCreditGrantOpenMeterClient{
		customerErr: openmeter.ErrCustomerNotFound,
	}
	r := &CreditGrantReconciler{
		Client:          cl,
		OpenMeterClient: omc,
		Recorder:        record.NewFakeRecorder(10),
	}

	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "org-1", Name: "test-grant"},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected non-zero RequeueAfter for pending customer")
	}

	updated := &billingv1alpha1.CreditGrant{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "org-1", Name: "test-grant"}, updated); err != nil {
		t.Fatalf("Get updated: %v", err)
	}
	if updated.Status.Phase != billingv1alpha1.CreditGrantPhasePending {
		t.Errorf("status.phase = %s, want Pending", updated.Status.Phase)
	}
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, billingv1alpha1.CreditGrantConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != creditGrantReasonWaitingForCustomer {
		t.Errorf("Ready condition = %+v, want False/WaitingForCustomer", ready)
	}
}

func TestCreditGrantReconciler_Success(t *testing.T) {
	scheme := setupCreditGrantScheme(t)
	account := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "main-account",
			Namespace: "org-1",
			UID:       "account-uid-123",
		},
	}
	grant := &billingv1alpha1.CreditGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-grant",
			Namespace: "org-1",
			UID:       "grant-uid-456",
		},
		Spec: billingv1alpha1.CreditGrantSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: "main-account"},
			Amount:            "100.00",
			Currency:          "USD",
			Reason:            billingv1alpha1.CreditGrantReasonPromotional,
			Description:       "welcome",
			ExpiresAfter:      &metav1.Duration{Duration: 720 * time.Hour},
		},
	}

	now := time.Now().Truncate(time.Second)
	expiresAt := now.Add(720 * time.Hour)
	key := string(grant.UID)
	omGrant := omv3.BillingCreditGrant{
		Id:          "om-grant-789",
		Name:        "test-grant",
		Amount:      "100.00",
		Currency:    "USD",
		Status:      omv3.BillingCreditGrantStatusActive,
		Key:         &key,
		EffectiveAt: &now,
		ExpiresAt:   &expiresAt,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(account, grant).WithStatusSubresource(grant).Build()
	omc := &fakeCreditGrantOpenMeterClient{
		customer:    om.Customer{Id: "om-cust-123", Key: ptr.To("account-uid-123")},
		ensureGrant: omGrant,
	}
	r := &CreditGrantReconciler{
		Client:          cl,
		OpenMeterClient: omc,
		Recorder:        record.NewFakeRecorder(10),
	}

	// First pass adds finalizer
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "org-1", Name: "test-grant"},
	})
	if err != nil {
		t.Fatalf("First Reconcile failed: %v", err)
	}

	// Second pass ensures grant
	_, err = r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "org-1", Name: "test-grant"},
	})
	if err != nil {
		t.Fatalf("Second Reconcile failed: %v", err)
	}

	updated := &billingv1alpha1.CreditGrant{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "org-1", Name: "test-grant"}, updated); err != nil {
		t.Fatalf("Get updated: %v", err)
	}
	if !controllerutil.ContainsFinalizer(updated, CreditGrantFinalizer) {
		t.Errorf("missing finalizer on grant")
	}
	if updated.Annotations[CreditGrantIDAnnotation] != "om-grant-789" {
		t.Errorf("grantId annotation = %s, want om-grant-789", updated.Annotations[CreditGrantIDAnnotation])
	}
	if updated.Status.Phase != billingv1alpha1.CreditGrantPhaseActive {
		t.Errorf("status.phase = %s, want Active", updated.Status.Phase)
	}
	if updated.Status.EffectiveAt == nil || !updated.Status.EffectiveAt.Time.Equal(now) {
		t.Errorf("status.effectiveAt = %v, want %v", updated.Status.EffectiveAt, now)
	}
	if updated.Status.ExpiresAt == nil || !updated.Status.ExpiresAt.Time.Equal(expiresAt) {
		t.Errorf("status.expiresAt = %v, want %v", updated.Status.ExpiresAt, expiresAt)
	}
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, billingv1alpha1.CreditGrantConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != "Active" {
		t.Errorf("Ready condition = %+v, want True/Active", ready)
	}
	if len(omc.ensureCalls) != 1 {
		t.Errorf("ensureCalls = %d, want 1", len(omc.ensureCalls))
	}
}

func TestCreditGrantReconciler_Void(t *testing.T) {
	scheme := setupCreditGrantScheme(t)
	account := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "main-account",
			Namespace: "org-1",
			UID:       "account-uid-123",
		},
	}
	grant := &billingv1alpha1.CreditGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-grant",
			Namespace:  "org-1",
			UID:        "grant-uid-456",
			Finalizers: []string{CreditGrantFinalizer},
			Annotations: map[string]string{
				CreditGrantIDAnnotation: "om-grant-789",
			},
		},
		Spec: billingv1alpha1.CreditGrantSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: "main-account"},
			Amount:            "100.00",
			Currency:          "USD",
			Reason:            billingv1alpha1.CreditGrantReasonPromotional,
			Description:       "welcome",
			Voided:            true,
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(account, grant).WithStatusSubresource(grant).Build()
	omc := &fakeCreditGrantOpenMeterClient{
		customer: om.Customer{Id: "om-cust-123", Key: ptr.To("account-uid-123")},
	}
	r := &CreditGrantReconciler{
		Client:          cl,
		OpenMeterClient: omc,
		Recorder:        record.NewFakeRecorder(10),
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "org-1", Name: "test-grant"},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	updated := &billingv1alpha1.CreditGrant{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "org-1", Name: "test-grant"}, updated); err != nil {
		t.Fatalf("Get updated: %v", err)
	}
	if updated.Status.Phase != billingv1alpha1.CreditGrantPhaseVoided {
		t.Errorf("status.phase = %s, want Voided", updated.Status.Phase)
	}
	if len(omc.voidCalls) != 1 || omc.voidCalls[0] != "om-grant-789" {
		t.Errorf("voidCalls = %v, want [om-grant-789]", omc.voidCalls)
	}
}

func TestCreditGrantReconciler_Deletion(t *testing.T) {
	scheme := setupCreditGrantScheme(t)
	account := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "main-account",
			Namespace: "org-1",
			UID:       "account-uid-123",
		},
	}
	now := metav1.Now()
	grant := &billingv1alpha1.CreditGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-grant",
			Namespace:         "org-1",
			UID:               "grant-uid-456",
			Finalizers:        []string{CreditGrantFinalizer},
			DeletionTimestamp: &now,
			Annotations: map[string]string{
				CreditGrantIDAnnotation: "om-grant-789",
			},
		},
		Spec: billingv1alpha1.CreditGrantSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: "main-account"},
			Amount:            "100.00",
			Currency:          "USD",
			Reason:            billingv1alpha1.CreditGrantReasonPromotional,
			Description:       "welcome",
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(account, grant).WithStatusSubresource(grant).Build()
	omc := &fakeCreditGrantOpenMeterClient{
		customer: om.Customer{Id: "om-cust-123", Key: ptr.To("account-uid-123")},
	}
	r := &CreditGrantReconciler{
		Client:          cl,
		OpenMeterClient: omc,
		Recorder:        record.NewFakeRecorder(10),
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "org-1", Name: "test-grant"},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	if len(omc.voidCalls) != 1 || omc.voidCalls[0] != "om-grant-789" {
		t.Errorf("voidCalls = %v, want [om-grant-789]", omc.voidCalls)
	}

	updated := &billingv1alpha1.CreditGrant{}
	_ = cl.Get(context.Background(), types.NamespacedName{Namespace: "org-1", Name: "test-grant"}, updated)
	if controllerutil.ContainsFinalizer(updated, CreditGrantFinalizer) {
		t.Errorf("finalizer should have been removed")
	}
}
