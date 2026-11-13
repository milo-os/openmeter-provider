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
	endCalls    [][]openmeter.FeeEnd
	endFn       func([]openmeter.FeeEnd) (openmeter.FeeEndResult, error)
}

func (f *fakeSubscriptionClient) EndFees(_ context.Context, subscriptionID string, fees []openmeter.FeeEnd, _ time.Time) (openmeter.FeeEndResult, error) {
	f.calls = append(f.calls, "EndFees "+subscriptionID)
	f.endCalls = append(f.endCalls, fees)
	if f.endFn != nil {
		return f.endFn(fees)
	}
	return openmeter.FeeEndResult{}, nil
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
	res, err := reconcileEntitlementErr(r, name)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

// reconcileEntitlementErr runs one reconcile and returns its error, for
// failures that are expected to surface as one.
func reconcileEntitlementErr(r *BillingEntitlementReconciler, name string) (reconcile.Result, error) {
	return r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name},
	})
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

	if want := time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC); got.BillingAnchor == nil || !got.BillingAnchor.Equal(want) {
		t.Errorf("BillingAnchor = %v, want the 1st of a month (%s)", got.BillingAnchor, want)
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

func TestBillingEntitlement_Contract(t *testing.T) {
	now := time.Date(2026, time.March, 18, 10, 30, 0, 0, time.UTC)
	contractEntitlement := func(startsAt time.Time) *billingv1alpha1.BillingEntitlement {
		be := testEntitlement(testBEName, "test-offer")
		be.Spec.Contract = &billingv1alpha1.BillingContract{StartsAt: metav1.NewTime(startsAt)}
		return be
	}

	t.Run("started today: subscribes, billing on the contract's day", func(t *testing.T) {
		startsAt := time.Date(2026, time.March, 18, 0, 0, 0, 0, time.UTC)
		r, omc, _ := newEntitlementReconciler(t, contractEntitlement(startsAt), testAccount(), cpuOffer())
		r.Now = func() time.Time { return now }

		reconcileEntitlement(t, r, testBEName)

		if len(omc.ensureCalls) != 1 {
			t.Fatalf("EnsureSubscription calls = %v", omc.calls)
		}
		if got := omc.ensureCalls[0].BillingAnchor; got == nil || !got.Equal(startsAt) {
			t.Errorf("BillingAnchor = %v, want the contract start %s", got, startsAt)
		}
		assertCondition(t, r, metav1.ConditionTrue, conditionReasonSynced)
	})

	t.Run("starts later: waits until the start without subscribing", func(t *testing.T) {
		startsAt := time.Date(2026, time.April, 18, 0, 0, 0, 0, time.UTC)
		r, omc, _ := newEntitlementReconciler(t, contractEntitlement(startsAt), testAccount(), cpuOffer())
		r.Now = func() time.Time { return now }

		res := reconcileEntitlement(t, r, testBEName)

		if len(omc.calls) != 0 {
			t.Errorf("OpenMeter called before the contract starts: %v", omc.calls)
		}
		c := assertCondition(t, r, metav1.ConditionFalse, conditionReasonContractNotStarted)
		if !strings.Contains(c.Message, "2026-04-18T00:00:00Z") {
			t.Errorf("condition message = %q, want the start date", c.Message)
		}
		if want := startsAt.Sub(now); res.RequeueAfter != want {
			t.Errorf("RequeueAfter = %v, want %v (until the start)", res.RequeueAfter, want)
		}

		// On the start date it subscribes.
		r.Now = func() time.Time { return startsAt.Add(time.Second) }
		reconcileEntitlement(t, r, testBEName)
		if len(omc.ensureCalls) != 1 {
			t.Fatalf("EnsureSubscription calls on the start date = %v", omc.calls)
		}
		if got := omc.ensureCalls[0].BillingAnchor; got == nil || !got.Equal(startsAt) {
			t.Errorf("BillingAnchor = %v, want the contract start %s", got, startsAt)
		}
	})
}

// TestBillingEntitlement_ContractMoveAndAmend: an account moved onto a
// contract changes plan and billing day on the start date, and from then on
// a change of Offer waits for the end of the billing period.
func TestBillingEntitlement_ContractMoveAndAmend(t *testing.T) {
	startsAt := time.Date(2026, time.March, 18, 0, 0, 0, 0, time.UTC)
	now := startsAt.Add(time.Minute)
	be := testEntitlement(testBEName, "test-offer")
	be.Spec.Contract = &billingv1alpha1.BillingContract{StartsAt: metav1.NewTime(startsAt)}
	r, omc, _ := newEntitlementReconciler(t, be, testAccount(), cpuOffer())
	r.Now = func() time.Time { return now }

	// The start date: the plan changes now, onto the contract's day.
	reconcileEntitlement(t, r, testBEName)
	d := omc.ensureCalls[0]
	if !d.MoveAnchor || d.ChangeAtPeriodEnd || d.BillingAnchor == nil || !d.BillingAnchor.Equal(startsAt) {
		t.Fatalf("moving onto the contract: MoveAnchor %v, ChangeAtPeriodEnd %v, anchor %v; want a move to %s now",
			d.MoveAnchor, d.ChangeAtPeriodEnd, d.BillingAnchor, startsAt)
	}
	got, _ := getEntitlement(t, r, testBEName)
	if v := got.Annotations[ContractStartedAnnotation]; v != "2026-03-18T00:00:00Z" {
		t.Fatalf("%s = %q, want the contract's start", ContractStartedAnnotation, v)
	}
	assertCondition(t, r, metav1.ConditionTrue, conditionReasonSynced)

	// An amendment: the change waits for the end of the billing period.
	periodEnd := time.Date(2026, time.April, 18, 0, 0, 0, 0, time.UTC)
	now = time.Date(2026, time.March, 25, 12, 0, 0, 0, time.UTC)
	omc.ensureFn = func(d openmeter.DesiredSubscription) (openmeter.SubscriptionState, error) {
		return openmeter.SubscriptionState{
			Current:     om.Subscription{Id: "sub-1", Status: om.SubscriptionStatusActive, Plan: &om.PlanReference{Key: "old-plan", Version: 1}},
			PlanVersion: 1,
			ChangeAt:    &periodEnd,
		}, nil
	}
	res := reconcileEntitlement(t, r, testBEName)
	d = omc.ensureCalls[len(omc.ensureCalls)-1]
	if d.MoveAnchor || !d.ChangeAtPeriodEnd || d.ChangeDue != nil {
		t.Errorf("amending: MoveAnchor %v, ChangeAtPeriodEnd %v, ChangeDue %v; want the change at the period end", d.MoveAnchor, d.ChangeAtPeriodEnd, d.ChangeDue)
	}
	c := assertCondition(t, r, metav1.ConditionTrue, conditionReasonOfferChangeScheduled)
	if !strings.Contains(c.Message, "2026-04-18T00:00:00Z") {
		t.Errorf("condition message = %q, want when it changes", c.Message)
	}
	if want := subscriptionResyncAfter; res.RequeueAfter != want {
		t.Errorf("RequeueAfter = %v, want the resync %v (the change is weeks away)", res.RequeueAfter, want)
	}
	got, _ = getEntitlement(t, r, testBEName)
	if v := got.Annotations[OfferChangeAtAnnotation]; v != "2026-04-18T00:00:00Z" {
		t.Errorf("%s = %q, want the period end", OfferChangeAtAnnotation, v)
	}

	// Shortly before the end it comes back just after the boundary, and
	// says when the change was due.
	now = periodEnd.Add(-time.Minute)
	res = reconcileEntitlement(t, r, testBEName)
	if want := time.Minute + changeAtPeriodEndDelay; res.RequeueAfter != want {
		t.Errorf("RequeueAfter = %v, want %v (just after the boundary)", res.RequeueAfter, want)
	}
	if d = omc.ensureCalls[len(omc.ensureCalls)-1]; d.ChangeDue == nil || !d.ChangeDue.Equal(periodEnd) {
		t.Errorf("ChangeDue = %v, want %s", d.ChangeDue, periodEnd)
	}

	// Once the change is made, the due date goes.
	omc.ensureFn = nil
	now = periodEnd.Add(changeAtPeriodEndDelay)
	reconcileEntitlement(t, r, testBEName)
	got, _ = getEntitlement(t, r, testBEName)
	if _, ok := got.Annotations[OfferChangeAtAnnotation]; ok {
		t.Errorf("%s still set after the change", OfferChangeAtAnnotation)
	}
	if _, ok := got.Annotations[ContractStartedAnnotation]; !ok {
		t.Errorf("%s removed", ContractStartedAnnotation)
	}
	assertCondition(t, r, metav1.ConditionTrue, conditionReasonSynced)
}

func TestSubscriptionBillingAnchor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		terms   *billingv1alpha1.PaymentTerms
		wantDay int
	}{
		{"no terms bill on the 1st", nil, 1},
		{"terms without a day bill on the 1st", &billingv1alpha1.PaymentTerms{}, 1},
		{"the account's invoice day", &billingv1alpha1.PaymentTerms{InvoiceDayOfMonth: 15}, 15},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := testAccount()
			account.Spec.PaymentTerms = tc.terms
			got := subscriptionBillingAnchor(account)
			if want := time.Date(2025, time.January, tc.wantDay, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
				t.Errorf("anchor = %s, want %s", got, want)
			}
		})
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
		wantErr     bool
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
			wantErr:     true,
			wantMessage: syncReasonPermanent + ":",
		},
		{
			name:        "transient OpenMeter error",
			err:         &openmeter.TransientError{Err: errors.New("unavailable"), StatusCode: 503},
			wantReason:  conditionReasonOpenMeterError,
			wantErr:     true,
			wantMessage: syncReasonTransient + ":",
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

			// Waiting on another object requeues without an error; an
			// OpenMeter failure is returned for controller-runtime to count
			// and retry with backoff.
			res, err := reconcileEntitlementErr(r, testBEName)
			if tt.wantErr != (err != nil) {
				t.Fatalf("Reconcile error = %v, want error: %v", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, tt.err) {
				t.Errorf("Reconcile error = %v, want it to wrap %v", err, tt.err)
			}

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
			_, _ = reconcileEntitlementErr(r, testBEName)
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

	if _, err := reconcileEntitlementErr(r, testBEName); !openmeter.IsTransient(err) {
		t.Errorf("Reconcile error = %v, want the transient error returned for retry", err)
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
		name string
		err  error
	}{
		{"transient", &openmeter.TransientError{Err: errors.New("unavailable")}},
		{"permanent", &openmeter.PermanentError{Err: errors.New("refused"), StatusCode: 400}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, omc, rec := newEntitlementReconciler(t, deletingEntitlement(testBEName), testAccount())
			omc.cancelFn = func(openmeter.CustomerKey) error { return tt.err }

			if _, err := reconcileEntitlementErr(r, testBEName); !errors.Is(err, tt.err) {
				t.Errorf("Reconcile error = %v, want it to wrap %v", err, tt.err)
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

// A subscription the provider creates or changes is recorded on the
// entitlement, so the account picks up the invoice OpenMeter issues for it
// within seconds; a pass that changed nothing records nothing.
func TestBillingEntitlement_RecordsSubscriptionChange(t *testing.T) {
	now := time.Date(2026, time.November, 12, 18, 47, 50, 0, time.UTC)
	r, omc, _ := newEntitlementReconciler(t, testEntitlement(testBEName, "test-offer"), testAccount(), cpuOffer())
	r.Now = func() time.Time { return now }
	changed := true
	omc.ensureFn = func(d openmeter.DesiredSubscription) (openmeter.SubscriptionState, error) {
		return openmeter.SubscriptionState{
			Current: om.Subscription{
				Id:     "sub-1",
				Status: om.SubscriptionStatusActive,
				Plan:   &om.PlanReference{Key: d.PlanKey, Version: 1},
			},
			PlanVersion: 1,
			Changed:     changed,
		}, nil
	}

	reconcileEntitlement(t, r, testBEName)
	be, _ := getEntitlement(t, r, testBEName)
	if got, want := be.Annotations[SubscriptionChangedAnnotation], "2026-11-12T18:47:50Z"; got != want {
		t.Fatalf("%s = %q, want %q", SubscriptionChangedAnnotation, got, want)
	}

	changed = false
	r.Now = func() time.Time { return now.Add(time.Hour) }
	reconcileEntitlement(t, r, testBEName)
	be, _ = getEntitlement(t, r, testBEName)
	if got, want := be.Annotations[SubscriptionChangedAnnotation], "2026-11-12T18:47:50Z"; got != want {
		t.Errorf("%s = %q after a pass that changed nothing, want it left at %q", SubscriptionChangedAnnotation, got, want)
	}
}

// A contract fee that runs for a set number of periods is ended once its
// last period is over: the controller asks for it with the fee's item key
// and end, counted from the contract's start, records what happened, and
// comes back when the fee's last period can start.
func TestBillingEntitlement_EndsFeesAfterTheirPeriods(t *testing.T) {
	now := time.Date(2027, time.January, 20, 9, 0, 0, 0, time.UTC)
	start := time.Date(2026, time.November, 12, 0, 0, 0, 0, time.UTC)
	offer := cpuOffer()
	promo := billingv1alpha1.ServicePricingSnapshot{
		Name: "promo-fee", Periods: 3,
		Spec: billingv1alpha1.ServicePricingSpec{
			DisplayName: "Promo", ChargeType: billingv1alpha1.ChargeTypeRecurring,
			Amount: "100.00", Interval: billingv1alpha1.ChargeIntervalMonthly,
		},
	}
	offer.Spec.ServicePricings = append(offer.Spec.ServicePricings, promo)
	be := testEntitlement(testBEName, "test-offer")
	be.Spec.Contract = &billingv1alpha1.BillingContract{StartsAt: metav1.NewTime(start)}
	be.Annotations = map[string]string{ContractStartedAnnotation: start.Format(time.RFC3339)}

	endsAt := time.Date(2027, time.February, 12, 0, 0, 0, 0, time.UTC)

	t.Run("scheduled in its last period", func(t *testing.T) {
		r, omc, rec := newEntitlementReconciler(t, be.DeepCopy(), testAccount(), offer.DeepCopy())
		r.Now = func() time.Time { return now }
		omc.endFn = func(fees []openmeter.FeeEnd) (openmeter.FeeEndResult, error) {
			return openmeter.FeeEndResult{Scheduled: fees}, nil
		}
		reconcileEntitlement(t, r, testBEName)
		if len(omc.endCalls) != 1 || len(omc.endCalls[0]) != 1 {
			t.Fatalf("EndFees calls = %v, want one with the promo fee", omc.endCalls)
		}
		got := omc.endCalls[0][0]
		if got.ItemKey != flatFeeKey(promo) || !got.EndsAt.Equal(endsAt) {
			t.Errorf("fee end = %+v, want %s ending %s", got, flatFeeKey(promo), endsAt)
		}
		if !hasEventPrefix(rec, "Normal "+EventReasonFeeEndScheduled+" Promo's last period ends 2027-02-12T00:00:00Z") {
			t.Error("no FeeEndScheduled event")
		}
	})

	t.Run("comes back when its last period starts", func(t *testing.T) {
		r, omc, _ := newEntitlementReconciler(t, be.DeepCopy(), testAccount(), offer.DeepCopy())
		r.Now = func() time.Time { return now }
		// The last period starts sooner than the periodic resync.
		soon := now.Add(10 * time.Minute)
		omc.endFn = func([]openmeter.FeeEnd) (openmeter.FeeEndResult, error) {
			return openmeter.FeeEndResult{NextCheck: &soon}, nil
		}
		res := reconcileEntitlement(t, r, testBEName)
		if want := soon.Sub(now) + changeAtPeriodEndDelay; res.RequeueAfter != want {
			t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, want)
		}
	})

	t.Run("removed late and reported", func(t *testing.T) {
		r, omc, rec := newEntitlementReconciler(t, be.DeepCopy(), testAccount(), offer.DeepCopy())
		r.Now = func() time.Time { return now }
		omc.endFn = func(fees []openmeter.FeeEnd) (openmeter.FeeEndResult, error) {
			return openmeter.FeeEndResult{RemovedLate: fees}, nil
		}
		reconcileEntitlement(t, r, testBEName)
		if !hasEventPrefix(rec, "Warning "+EventReasonFeeEndedLate+" Promo should have stopped") {
			t.Error("no FeeEndedLate warning")
		}
	})

	t.Run("removed at once when an amended subscription starts after its end", func(t *testing.T) {
		r, omc, rec := newEntitlementReconciler(t, be.DeepCopy(), testAccount(), offer.DeepCopy())
		later := time.Date(2027, time.November, 12, 0, 0, 5, 0, time.UTC)
		r.Now = func() time.Time { return later }
		omc.ensureFn = func(d openmeter.DesiredSubscription) (openmeter.SubscriptionState, error) {
			return openmeter.SubscriptionState{
				Current: om.Subscription{Id: "sub-2", Status: om.SubscriptionStatusActive,
					ActiveFrom: time.Date(2027, time.November, 12, 0, 0, 0, 0, time.UTC),
					Plan:       &om.PlanReference{Key: d.PlanKey, Version: 1}},
				PlanVersion: 1,
			}, nil
		}
		omc.endFn = func(fees []openmeter.FeeEnd) (openmeter.FeeEndResult, error) {
			return openmeter.FeeEndResult{RemovedLate: fees}, nil
		}
		reconcileEntitlement(t, r, testBEName)
		if !hasEventPrefix(rec, "Normal "+EventReasonFeeEndedBeforeAmendment+" Promo's last period ended 2027-02-12T00:00:00Z") {
			t.Error("no FeeEndedBeforeAmendment event")
		}
	})

	// While an amendment waits, the subscription is on the previous
	// Offer's plan, and its fees end by the previous Offer's terms.
	amended := offer.DeepCopy()
	amended.Name, amended.UID = "amended-offer", types.UID("amended-uid")
	for i := range amended.Spec.ServicePricings {
		if amended.Spec.ServicePricings[i].Name == promo.Name {
			amended.Spec.ServicePricings[i].Periods = 6
		}
	}
	amending := be.DeepCopy()
	amending.Spec.OfferRef.Name = amended.Name
	waiting := func(omc *fakeSubscriptionClient, offerName string) {
		omc.ensureFn = func(openmeter.DesiredSubscription) (openmeter.SubscriptionState, error) {
			changeAt := time.Date(2027, time.November, 12, 0, 0, 0, 0, time.UTC)
			return openmeter.SubscriptionState{
				Current: om.Subscription{Id: "sub-1", Status: om.SubscriptionStatusActive,
					ActiveFrom: start,
					Plan:       &om.PlanReference{Key: planKeyForOffer(offer), Version: 1},
					Metadata:   &om.Metadata{subscriptionMetadataOffer: offerName}},
				PlanVersion: 1,
				ChangeAt:    &changeAt,
			}, nil
		}
	}

	t.Run("by the subscribed Offer while an amendment waits", func(t *testing.T) {
		r, omc, rec := newEntitlementReconciler(t, amending.DeepCopy(), testAccount(), offer.DeepCopy(), amended.DeepCopy())
		r.Now = func() time.Time { return now }
		waiting(omc, offer.Name)
		omc.endFn = func(fees []openmeter.FeeEnd) (openmeter.FeeEndResult, error) {
			return openmeter.FeeEndResult{Scheduled: fees}, nil
		}
		reconcileEntitlement(t, r, testBEName)
		if len(omc.endCalls) != 1 || len(omc.endCalls[0]) != 1 || !omc.endCalls[0][0].EndsAt.Equal(endsAt) {
			t.Fatalf("EndFees calls = %v, want the promo fee ending %s, by the previous Offer", omc.endCalls, endsAt)
		}
		if !hasEventPrefix(rec, "Normal "+EventReasonFeeEndScheduled+" Promo's last period ends 2027-02-12T00:00:00Z") {
			t.Error("no FeeEndScheduled event")
		}
	})

	t.Run("by the new Offer when the subscribed one is gone", func(t *testing.T) {
		r, omc, _ := newEntitlementReconciler(t, amending.DeepCopy(), testAccount(), amended.DeepCopy())
		r.Now = func() time.Time { return now }
		waiting(omc, offer.Name)
		reconcileEntitlement(t, r, testBEName)
		want := time.Date(2027, time.May, 12, 0, 0, 0, 0, time.UTC)
		if len(omc.endCalls) != 1 || len(omc.endCalls[0]) != 1 || !omc.endCalls[0][0].EndsAt.Equal(want) {
			t.Fatalf("EndFees calls = %v, want the promo fee ending %s, by the new Offer", omc.endCalls, want)
		}
	})
}

// hasEventPrefix reports whether the recorder holds an event starting with want.
func hasEventPrefix(rec *record.FakeRecorder, want string) bool {
	for {
		select {
		case e := <-rec.Events:
			if strings.HasPrefix(e, want) {
				return true
			}
		default:
			return false
		}
	}
}
