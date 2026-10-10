// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	om "github.com/openmeterio/openmeter/api/client/go"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

const (
	testNamespace   = "org-ns"
	testAccountName = "acct"
	testAccountUID  = "acct-uid"
	testBEName      = "entitlement"
)

// fakeSubscriptionClient records the subscription calls the controller makes.
type fakeSubscriptionClient struct {
	openmeter.Client // unimplemented methods panic if used

	calls       []string
	ensureCalls []openmeter.DesiredSubscription
	ensureFn    func(openmeter.DesiredSubscription) (openmeter.SubscriptionState, error)
	cancelFn    func(openmeter.CustomerKey) error
}

func (f *fakeSubscriptionClient) EnsureSubscription(_ context.Context, d openmeter.DesiredSubscription) (openmeter.SubscriptionState, error) {
	f.calls = append(f.calls, "EnsureSubscription "+string(d.CustomerKey)+" "+d.PlanKey)
	f.ensureCalls = append(f.ensureCalls, d)
	if f.ensureFn != nil {
		return f.ensureFn(d)
	}
	return openmeter.SubscriptionState{
		Current: om.Subscription{
			Id:     "sub-1",
			Status: om.SubscriptionStatusActive,
			Plan:   &om.PlanReference{Key: d.PlanKey, Version: 1},
		},
		PlanVersion: 1,
	}, nil
}

func (f *fakeSubscriptionClient) CancelSubscriptions(_ context.Context, key openmeter.CustomerKey) error {
	f.calls = append(f.calls, "CancelSubscriptions "+string(key))
	if f.cancelFn != nil {
		return f.cancelFn(key)
	}
	return nil
}

func testAccount() *billingv1alpha1.BillingAccount {
	return &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testAccountName, UID: types.UID(testAccountUID)},
	}
}

func deletingAccount() *billingv1alpha1.BillingAccount {
	a := testAccount()
	now := metav1.Now()
	a.DeletionTimestamp = &now
	a.Finalizers = []string{CustomerLinkFinalizer}
	return a
}

func testEntitlement(name, offer string) *billingv1alpha1.BillingEntitlement {
	return &billingv1alpha1.BillingEntitlement{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  testNamespace,
			Name:       name,
			UID:        types.UID(name + "-uid"),
			Generation: 1,
			Finalizers: []string{SubscriptionFinalizer},
		},
		Spec: billingv1alpha1.BillingEntitlementSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: testAccountName},
			OfferRef:          billingv1alpha1.OfferReference{Name: offer},
		},
	}
}

func deletingEntitlement(name string) *billingv1alpha1.BillingEntitlement {
	be := testEntitlement(name, "test-offer")
	now := metav1.Now()
	be.DeletionTimestamp = &now
	return be
}

func newEntitlementReconciler(t *testing.T, objs ...client.Object) (*BillingEntitlementReconciler, *fakeSubscriptionClient, *record.FakeRecorder) {
	t.Helper()
	k8s := fake.NewClientBuilder().
		WithScheme(newOfferScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&billingv1alpha1.BillingEntitlement{}).
		WithIndex(&billingv1alpha1.BillingEntitlement{}, EntitlementBillingAccountRefField, entitlementBillingAccountRef).
		WithIndex(&billingv1alpha1.BillingEntitlement{}, EntitlementOfferRefField, entitlementOfferRef).
		Build()
	omc := &fakeSubscriptionClient{}
	rec := record.NewFakeRecorder(50)
	return &BillingEntitlementReconciler{Client: k8s, OpenMeterClient: omc, Recorder: rec}, omc, rec
}

func reconcileEntitlement(t *testing.T, r *BillingEntitlementReconciler, name string) reconcile.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func getEntitlement(t *testing.T, r *BillingEntitlementReconciler, name string) (*billingv1alpha1.BillingEntitlement, bool) {
	t.Helper()
	var be billingv1alpha1.BillingEntitlement
	err := r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, &be)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	return &be, true
}

func subscriptionCondition(t *testing.T, r *BillingEntitlementReconciler) *metav1.Condition {
	t.Helper()
	be, ok := getEntitlement(t, r, testBEName)
	if !ok {
		t.Fatal("entitlement not found")
	}
	return apimeta.FindStatusCondition(be.Status.Conditions, ConditionTypeOpenMeterSubscriptionSynced)
}

func assertCondition(t *testing.T, r *BillingEntitlementReconciler, status metav1.ConditionStatus, reason string) *metav1.Condition {
	t.Helper()
	c := subscriptionCondition(t, r)
	if c == nil {
		t.Fatalf("condition %s not set", ConditionTypeOpenMeterSubscriptionSynced)
	}
	if c.Status != status || c.Reason != reason {
		t.Fatalf("condition = %s/%s (%s), want %s/%s", c.Status, c.Reason, c.Message, status, reason)
	}
	return c
}

// ---------------------------------------------------------------------------
// Sync
// ---------------------------------------------------------------------------

func TestBillingEntitlement_AddsFinalizerFirst(t *testing.T) {
	be := testEntitlement(testBEName, "test-offer")
	be.Finalizers = nil
	r, omc, _ := newEntitlementReconciler(t, be, testAccount(), cpuOffer())

	reconcileEntitlement(t, r, testBEName)

	stored, _ := getEntitlement(t, r, testBEName)
	if !slices.Contains(stored.Finalizers, SubscriptionFinalizer) {
		t.Fatalf("finalizer not added: %v", stored.Finalizers)
	}
	if len(omc.calls) != 0 {
		t.Errorf("OpenMeter called before the finalizer was persisted: %v", omc.calls)
	}
}

func TestBillingEntitlement_SubscribesAccountToOfferPlan(t *testing.T) {
	r, omc, rec := newEntitlementReconciler(t, testEntitlement(testBEName, "test-offer"), testAccount(), cpuOffer())

	res := reconcileEntitlement(t, r, testBEName)

	if len(omc.ensureCalls) != 1 {
		t.Fatalf("EnsureSubscription calls = %v", omc.calls)
	}
	got := omc.ensureCalls[0]
	if got.CustomerKey != testAccountUID {
		t.Errorf("CustomerKey = %q, want the BillingAccount UID", got.CustomerKey)
	}
	if got.PlanKey != planKeyForOffer(cpuOffer()) {
		t.Errorf("PlanKey = %q, want %q", got.PlanKey, planKeyForOffer(cpuOffer()))
	}
	wantMeta := map[string]string{
		"miloapis.com/billing-entitlement": testNamespace + "/" + testBEName,
		"miloapis.com/billing-account":     testAccountName,
		"miloapis.com/offer-name":          "test-offer",
	}
	for k, v := range wantMeta {
		if got.Metadata[k] != v {
			t.Errorf("metadata[%s] = %q, want %q", k, got.Metadata[k], v)
		}
	}

	c := assertCondition(t, r, metav1.ConditionTrue, conditionReasonSynced)
	if !strings.Contains(c.Message, "sub-1") || c.ObservedGeneration != 1 {
		t.Errorf("condition = %+v", c)
	}
	if res.RequeueAfter != subscriptionResyncAfter {
		t.Errorf("RequeueAfter = %v, want the resync interval", res.RequeueAfter)
	}
	if events := drainRecorder(rec); !containsEvent(events, EventReasonSynced) {
		t.Errorf("events = %v, want a Synced event", events)
	}

	// Steady state: no repeated event.
	reconcileEntitlement(t, r, testBEName)
	if events := drainRecorder(rec); len(events) != 0 {
		t.Errorf("steady state emitted events: %v", events)
	}
}

func TestBillingEntitlement_PendingMigration(t *testing.T) {
	r, omc, _ := newEntitlementReconciler(t, testEntitlement(testBEName, "test-offer"), testAccount(), cpuOffer())
	omc.ensureFn = func(d openmeter.DesiredSubscription) (openmeter.SubscriptionState, error) {
		end := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
		return openmeter.SubscriptionState{
			Current: om.Subscription{Id: "sub-1", Status: om.SubscriptionStatusCanceled,
				Plan: &om.PlanReference{Key: d.PlanKey, Version: 1}, ActiveTo: &end},
			Pending: &om.Subscription{Id: "sub-2", Status: om.SubscriptionStatusScheduled,
				Plan: &om.PlanReference{Key: d.PlanKey, Version: 2}, ActiveFrom: end},
			PlanVersion: 2,
		}, nil
	}

	reconcileEntitlement(t, r, testBEName)

	c := assertCondition(t, r, metav1.ConditionTrue, conditionReasonMigrationScheduled)
	if !strings.Contains(c.Message, "migrating to version 2 at 2026-11-09T00:00:00Z") {
		t.Errorf("message = %q", c.Message)
	}
}

func TestBillingEntitlement_EnsureFailures(t *testing.T) {
	failedOffer := cpuOffer()
	failedOffer.Status.Conditions = []metav1.Condition{{
		Type: ConditionTypeOpenMeterPlanSynced, Status: metav1.ConditionFalse,
		Reason: conditionReasonInvalidPricing, Message: "rates overlap",
	}}

	tests := []struct {
		name        string
		offer       *billingv1alpha1.Offer
		err         error
		wantReason  string
		wantRequeue time.Duration
		wantMessage string
	}{
		{
			name:        "customer not created yet",
			err:         openmeter.ErrCustomerNotFound,
			wantReason:  conditionReasonCustomerNotSynced,
			wantRequeue: transientRequeueAfter,
		},
		{
			name:        "plan not published yet",
			err:         fmt.Errorf("plan: %w", openmeter.ErrPlanNotPublished),
			wantReason:  conditionReasonPlanNotPublished,
			wantRequeue: transientRequeueAfter,
			wantMessage: "no published OpenMeter plan yet",
		},
		{
			name:        "plan failed to sync quotes the Offer's reason",
			offer:       failedOffer,
			err:         openmeter.ErrPlanNotPublished,
			wantReason:  conditionReasonPlanNotPublished,
			wantRequeue: transientRequeueAfter,
			wantMessage: "InvalidPricing: rates overlap",
		},
		{
			name:        "customer cannot be invoiced",
			err:         &openmeter.PermanentError{Err: fmt.Errorf("%w: no stripe data", openmeter.ErrCustomerBillingNotReady), StatusCode: 409},
			wantReason:  conditionReasonCustomerBillingNotReady,
			wantRequeue: customerBillingNotReadyRequeueAfter,
			wantMessage: "default payment method",
		},
		{
			name:        "permanent OpenMeter error",
			err:         &openmeter.PermanentError{Err: errors.New("bad request"), StatusCode: 400},
			wantReason:  conditionReasonOpenMeterError,
			wantRequeue: permanentRequeueAfter,
		},
		{
			name:        "transient OpenMeter error",
			err:         &openmeter.TransientError{Err: errors.New("unavailable"), StatusCode: 503},
			wantReason:  conditionReasonOpenMeterError,
			wantRequeue: transientRequeueAfter,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			offer := tt.offer
			if offer == nil {
				offer = cpuOffer()
			}
			r, omc, rec := newEntitlementReconciler(t, testEntitlement(testBEName, "test-offer"), testAccount(), offer)
			omc.ensureFn = func(openmeter.DesiredSubscription) (openmeter.SubscriptionState, error) {
				return openmeter.SubscriptionState{}, tt.err
			}

			res := reconcileEntitlement(t, r, testBEName)

			c := assertCondition(t, r, metav1.ConditionFalse, tt.wantReason)
			if !strings.Contains(c.Message, tt.wantMessage) {
				t.Errorf("message = %q, want it to contain %q", c.Message, tt.wantMessage)
			}
			if res.RequeueAfter != tt.wantRequeue {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, tt.wantRequeue)
			}
			if events := drainRecorder(rec); !containsEvent(events, EventReasonSyncFailed) {
				t.Errorf("events = %v, want SyncFailed", events)
			}
			reconcileEntitlement(t, r, testBEName)
			if events := drainRecorder(rec); len(events) != 0 {
				t.Errorf("unchanged failure re-emitted events: %v", events)
			}
		})
	}
}

func TestBillingEntitlement_WaitsWithoutTouchingSubscription(t *testing.T) {
	draft := cpuOffer()
	draft.Spec.LaunchStage = billingv1alpha1.OfferLaunchStageDraft

	tests := []struct {
		name       string
		objs       []client.Object
		wantReason string
	}{
		{"billing account missing", []client.Object{cpuOffer()}, conditionReasonBillingAccountNotFound},
		{"offer missing", []client.Object{testAccount()}, conditionReasonOfferNotFound},
		{"offer not GA", []client.Object{testAccount(), draft}, conditionReasonOfferNotAssignable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := append([]client.Object{testEntitlement(testBEName, "test-offer")}, tt.objs...)
			r, omc, _ := newEntitlementReconciler(t, objs...)

			res := reconcileEntitlement(t, r, testBEName)

			assertCondition(t, r, metav1.ConditionFalse, tt.wantReason)
			if len(omc.calls) != 0 {
				t.Errorf("OpenMeter calls = %v, want none (an existing subscription must be left alone)", omc.calls)
			}
			if res.RequeueAfter != 0 {
				t.Errorf("RequeueAfter = %v, want 0 (a watch brings it back)", res.RequeueAfter)
			}
		})
	}
}

func TestBillingEntitlement_DeletingAccountCancelsSubscription(t *testing.T) {
	r, omc, _ := newEntitlementReconciler(t, testEntitlement(testBEName, "test-offer"), deletingAccount(), cpuOffer())

	reconcileEntitlement(t, r, testBEName)

	if want := []string{"CancelSubscriptions " + testAccountUID}; !slices.Equal(omc.calls, want) {
		t.Errorf("calls = %v, want %v", omc.calls, want)
	}
	assertCondition(t, r, metav1.ConditionFalse, conditionReasonBillingAccountDeleting)
}

func TestBillingEntitlement_DeletingAccountCancelFailureRetries(t *testing.T) {
	r, omc, _ := newEntitlementReconciler(t, testEntitlement(testBEName, "test-offer"), deletingAccount(), cpuOffer())
	omc.cancelFn = func(openmeter.CustomerKey) error {
		return &openmeter.TransientError{Err: errors.New("unavailable")}
	}

	if res := reconcileEntitlement(t, r, testBEName); res.RequeueAfter != transientRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, transientRequeueAfter)
	}
	assertCondition(t, r, metav1.ConditionFalse, conditionReasonOpenMeterError)
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

func TestBillingEntitlement_DeleteCancelsAndReleases(t *testing.T) {
	r, omc, rec := newEntitlementReconciler(t, deletingEntitlement(testBEName), testAccount(), cpuOffer())

	reconcileEntitlement(t, r, testBEName)

	if want := []string{"CancelSubscriptions " + testAccountUID}; !slices.Equal(omc.calls, want) {
		t.Errorf("calls = %v, want %v", omc.calls, want)
	}
	if _, ok := getEntitlement(t, r, testBEName); ok {
		t.Error("finalizer not released")
	}
	if events := drainRecorder(rec); !containsEvent(events, EventReasonDeleted) {
		t.Errorf("events = %v, want Deleted", events)
	}
}

func TestBillingEntitlement_DeleteHandsOverToSuccessor(t *testing.T) {
	successor := testEntitlement("successor", "other-offer")
	r, omc, _ := newEntitlementReconciler(t, deletingEntitlement(testBEName), successor, testAccount(), cpuOffer())

	reconcileEntitlement(t, r, testBEName)

	if len(omc.calls) != 0 {
		t.Errorf("calls = %v, want none: the successor changes the subscription instead", omc.calls)
	}
	if _, ok := getEntitlement(t, r, testBEName); ok {
		t.Error("finalizer not released")
	}
}

func TestBillingEntitlement_DeleteIgnoresDeletingSibling(t *testing.T) {
	r, omc, _ := newEntitlementReconciler(t, deletingEntitlement(testBEName), deletingEntitlement("also-going"), testAccount())

	reconcileEntitlement(t, r, testBEName)

	if want := []string{"CancelSubscriptions " + testAccountUID}; !slices.Equal(omc.calls, want) {
		t.Errorf("calls = %v, want %v", omc.calls, want)
	}
}

func TestBillingEntitlement_DeleteWithoutAccountReleases(t *testing.T) {
	r, omc, _ := newEntitlementReconciler(t, deletingEntitlement(testBEName))

	reconcileEntitlement(t, r, testBEName)

	if len(omc.calls) != 0 {
		t.Errorf("calls = %v, want none: a deleted account has no customer", omc.calls)
	}
	if _, ok := getEntitlement(t, r, testBEName); ok {
		t.Error("finalizer not released")
	}
}

func TestBillingEntitlement_DeleteCancelFailureKeepsFinalizer(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantRequeue time.Duration
	}{
		{"transient", &openmeter.TransientError{Err: errors.New("unavailable")}, transientRequeueAfter},
		{"permanent", &openmeter.PermanentError{Err: errors.New("refused"), StatusCode: 400}, permanentRequeueAfter},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, omc, rec := newEntitlementReconciler(t, deletingEntitlement(testBEName), testAccount())
			omc.cancelFn = func(openmeter.CustomerKey) error { return tt.err }

			res := reconcileEntitlement(t, r, testBEName)

			if res.RequeueAfter != tt.wantRequeue {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, tt.wantRequeue)
			}
			be, ok := getEntitlement(t, r, testBEName)
			if !ok || !slices.Contains(be.Finalizers, SubscriptionFinalizer) {
				t.Fatal("finalizer released while the subscription may still bill")
			}
			if events := drainRecorder(rec); !containsEvent(events, EventReasonDeleteFailed) {
				t.Errorf("events = %v, want DeleteFailed", events)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Watches
// ---------------------------------------------------------------------------

func TestBillingEntitlement_WatchMappers(t *testing.T) {
	foreign := testEntitlement("foreign", "test-offer")
	foreign.Namespace = "other-ns"
	sibling := testEntitlement("sibling", "other-offer")
	r, _, _ := newEntitlementReconciler(t, testEntitlement(testBEName, "test-offer"), sibling, foreign)
	ctx := context.Background()

	names := func(reqs []reconcile.Request) []string {
		out := make([]string, 0, len(reqs))
		for _, req := range reqs {
			out = append(out, req.Namespace+"/"+req.Name)
		}
		slices.Sort(out)
		return out
	}

	if got, want := names(r.entitlementsForOffer(ctx, baseOffer())), []string{
		testNamespace + "/" + testBEName, "other-ns/foreign",
	}; !slices.Equal(got, want) {
		t.Errorf("entitlementsForOffer = %v, want %v", got, want)
	}
	if got, want := names(r.entitlementsForAccount(ctx, testAccount())), []string{
		testNamespace + "/" + testBEName, testNamespace + "/sibling",
	}; !slices.Equal(got, want) {
		t.Errorf("entitlementsForAccount = %v, want %v (namespace-scoped)", got, want)
	}
	if got, want := names(r.siblingEntitlements(ctx, testEntitlement(testBEName, "test-offer"))), []string{
		testNamespace + "/sibling",
	}; !slices.Equal(got, want) {
		t.Errorf("siblingEntitlements = %v, want %v", got, want)
	}
}
