// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
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

func newOfferScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := billingv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add billing scheme: %v", err)
	}
	return s
}

// newOfferReconciler builds an OfferReconciler backed by a fake k8s client
// (with the Offer status subresource enabled, as on the real CRD), a
// recording fake OpenMeter client, and a fake event recorder.
func newOfferReconciler(t *testing.T, objs ...client.Object) (*OfferReconciler, *fakeOpenMeterClient, *record.FakeRecorder) {
	t.Helper()
	k8s := fake.NewClientBuilder().
		WithScheme(newOfferScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&billingv1alpha1.Offer{}).
		Build()
	omc := &fakeOpenMeterClient{}
	rec := record.NewFakeRecorder(50)
	return &OfferReconciler{Client: k8s, OpenMeterClient: omc, Recorder: rec}, omc, rec
}

func withFinalizer(o *billingv1alpha1.Offer) *billingv1alpha1.Offer {
	cp := o.DeepCopy()
	cp.Finalizers = append(cp.Finalizers, ProductPlanFinalizer)
	return cp
}

func deletedOffer() *billingv1alpha1.Offer {
	now := metav1.Now()
	o := withFinalizer(cpuOffer())
	o.DeletionTimestamp = &now
	return o
}

func requestFor(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
}

func reconcileOnce(t *testing.T, r *OfferReconciler) reconcile.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), requestFor("test-offer"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func getOffer(t *testing.T, r *OfferReconciler) *billingv1alpha1.Offer {
	t.Helper()
	var stored billingv1alpha1.Offer
	if err := r.Get(context.Background(), requestFor("test-offer").NamespacedName, &stored); err != nil {
		t.Fatalf("get offer: %v", err)
	}
	return &stored
}

func assertOfferGone(t *testing.T, r *OfferReconciler) {
	t.Helper()
	var stored billingv1alpha1.Offer
	err := r.Get(context.Background(), requestFor("test-offer").NamespacedName, &stored)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("offer still present (err=%v): finalizer was not released", err)
	}
}

func assertFinalizerKept(t *testing.T, r *OfferReconciler) {
	t.Helper()
	if !slices.Contains(getOffer(t, r).Finalizers, ProductPlanFinalizer) {
		t.Fatalf("finalizer released; a live plan could be orphaned")
	}
}

func syncedCondition(t *testing.T, r *OfferReconciler) *metav1.Condition {
	t.Helper()
	return apimeta.FindStatusCondition(getOffer(t, r).Status.Conditions, ConditionTypeOpenMeterPlanSynced)
}

func drainRecorder(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case ev := <-rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func containsEvent(events []string, substr string) bool {
	for _, e := range events {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Sync
// ---------------------------------------------------------------------------

func TestReconcile_AddsFinalizer(t *testing.T) {
	r, omc, _ := newOfferReconciler(t, cpuMeter(), cpuOffer())

	if res := reconcileOnce(t, r); res.RequeueAfter != 0 {
		t.Errorf("unexpected requeue: %+v", res)
	}
	if len(omc.calls) != 0 {
		t.Errorf("OpenMeter called before finalizer added: %v", omc.calls)
	}
	if !slices.Contains(getOffer(t, r).Finalizers, ProductPlanFinalizer) {
		t.Errorf("finalizer not added")
	}
}

func TestReconcile_DraftNoSync(t *testing.T) {
	offer := withFinalizer(cpuOffer())
	offer.Spec.LaunchStage = billingv1alpha1.OfferLaunchStageDraft
	r, omc, _ := newOfferReconciler(t, cpuMeter(), offer)

	if res := reconcileOnce(t, r); res.RequeueAfter != 0 {
		t.Errorf("draft should not requeue: %+v", res)
	}
	if len(omc.calls) != 0 {
		t.Errorf("OpenMeter must not be called for a draft offer: %v", omc.calls)
	}
	if syncedCondition(t, r) != nil {
		t.Errorf("draft offer must not get a sync condition")
	}
}

func TestReconcile_GASnapshotEmptyRequeues(t *testing.T) {
	r, omc, rec := newOfferReconciler(t, cpuMeter(), withFinalizer(baseOffer()))

	if res := reconcileOnce(t, r); res.RequeueAfter != transientRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, transientRequeueAfter)
	}
	if len(omc.calls) != 0 {
		t.Errorf("OpenMeter called for an empty snapshot: %v", omc.calls)
	}
	if !containsEvent(drainRecorder(rec), EventReasonSyncSkipped) {
		t.Errorf("expected a %s event", EventReasonSyncSkipped)
	}
}

// TestReconcile_GAConverges is the happy path: meters are checked, features
// are ensured BEFORE the plan that references them, and the Offer records a
// True condition naming the published plan.
func TestReconcile_GAConverges(t *testing.T) {
	r, omc, rec := newOfferReconciler(t, cpuMeter(), withFinalizer(cpuOffer()))

	if res := reconcileOnce(t, r); res.RequeueAfter != 0 {
		t.Errorf("unexpected requeue on success: %+v", res)
	}

	want := []string{
		"GetMeter " + cpuSlug,
		"EnsureFeature", "EnsureFeature", "EnsureFeature",
		"EnsurePlan test_uid",
	}
	if got := omc.callNames(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	plan := omc.ensurePlanCalls[0]
	if len(plan.Phases) != 1 || len(plan.Phases[0].RateCards) != 5 {
		t.Errorf("plan phases wrong: %+v", plan.Phases)
	}

	cond := syncedCondition(t, r)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != conditionReasonSynced {
		t.Fatalf("condition = %+v, want True/%s", cond, conditionReasonSynced)
	}
	if !strings.Contains(cond.Message, "plan-1") || !strings.Contains(cond.Message, "active") {
		t.Errorf("condition message %q should name the plan and its status", cond.Message)
	}
	if !containsEvent(drainRecorder(rec), EventReasonSynced) {
		t.Errorf("expected a %s event", EventReasonSynced)
	}
}

// TestReconcile_SteadyStateIsQuiet: once synced, further reconciles neither
// rewrite the status nor emit events.
func TestReconcile_SteadyStateIsQuiet(t *testing.T) {
	r, _, rec := newOfferReconciler(t, cpuMeter(), withFinalizer(cpuOffer()))
	reconcileOnce(t, r)
	rv := getOffer(t, r).ResourceVersion
	drainRecorder(rec)

	reconcileOnce(t, r)
	if got := getOffer(t, r).ResourceVersion; got != rv {
		t.Errorf("status rewritten on a no-op reconcile (resourceVersion %s -> %s)", rv, got)
	}
	if evs := drainRecorder(rec); len(evs) != 0 {
		t.Errorf("events on a no-op reconcile: %v", evs)
	}
}

func TestReconcile_MeterDefinitionMissing(t *testing.T) {
	r, omc, rec := newOfferReconciler(t, withFinalizer(cpuOffer())) // no MeterDefinition

	if res := reconcileOnce(t, r); res.RequeueAfter != transientRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, transientRequeueAfter)
	}
	if len(omc.calls) != 0 {
		t.Errorf("OpenMeter called without a meter: %v", omc.calls)
	}
	if cond := syncedCondition(t, r); cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditionReasonMeterNotFound {
		t.Errorf("condition = %+v, want False/%s", cond, conditionReasonMeterNotFound)
	}
	if !containsEvent(drainRecorder(rec), EventReasonSyncFailed) {
		t.Errorf("expected a SyncFailed event")
	}
}

// TestReconcile_OpenMeterMeterNotYetSynced: the MeterDefinition exists but
// its OpenMeter meter does not yet; creating features now would 4xx and be
// backed off as permanent, so the reconciler waits on the short schedule.
func TestReconcile_OpenMeterMeterNotYetSynced(t *testing.T) {
	r, omc, _ := newOfferReconciler(t, cpuMeter(), withFinalizer(cpuOffer()))
	omc.getMeterFn = func(context.Context, string) (om.Meter, error) {
		return om.Meter{}, openmeter.ErrMeterNotFound
	}

	if res := reconcileOnce(t, r); res.RequeueAfter != transientRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, transientRequeueAfter)
	}
	if len(omc.ensureFeatureCalls) != 0 || len(omc.ensurePlanCalls) != 0 {
		t.Errorf("features/plan ensured before the meter exists: %v", omc.calls)
	}
	if cond := syncedCondition(t, r); cond == nil || cond.Reason != conditionReasonMeterNotSynced {
		t.Errorf("condition = %+v, want %s", cond, conditionReasonMeterNotSynced)
	}
}

// TestReconcile_InvalidPricing: an unrepresentable snapshot touches nothing
// in OpenMeter and backs off on the permanent schedule.
func TestReconcile_InvalidPricing(t *testing.T) {
	offer := withFinalizer(baseOffer())
	offer.Spec.ServicePricings = []billingv1alpha1.ServicePricingSnapshot{
		usagePricing("u", matchRate("region", "us-east", "1"), matchRate("tier", "premium", "2")),
	}
	r, omc, _ := newOfferReconciler(t, cpuMeter(), offer)

	if res := reconcileOnce(t, r); res.RequeueAfter != permanentRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, permanentRequeueAfter)
	}
	if len(omc.calls) != 0 {
		t.Errorf("OpenMeter called for an invalid offer: %v", omc.calls)
	}
	if cond := syncedCondition(t, r); cond == nil || cond.Reason != conditionReasonInvalidPricing {
		t.Errorf("condition = %+v, want %s", cond, conditionReasonInvalidPricing)
	}
}

func TestReconcile_OpenMeterFailures(t *testing.T) {
	transient := &openmeter.TransientError{Err: errors.New("down")}
	permanent := &openmeter.PermanentError{Err: errors.New("rejected")}
	tests := []struct {
		name        string
		setup       func(*fakeOpenMeterClient)
		wantRequeue time.Duration
		wantNoPlan  bool
	}{
		{
			name: "feature permanent stops before the plan",
			setup: func(f *fakeOpenMeterClient) {
				f.ensureFeatureFn = func(context.Context, openmeter.DesiredFeature) (om.Feature, error) { return om.Feature{}, permanent }
			},
			wantRequeue: permanentRequeueAfter,
			wantNoPlan:  true,
		},
		{
			name: "plan transient",
			setup: func(f *fakeOpenMeterClient) {
				f.ensurePlanFn = func(context.Context, openmeter.DesiredPlan) (om.Plan, error) { return om.Plan{}, transient }
			},
			wantRequeue: transientRequeueAfter,
		},
		{
			name: "plan permanent",
			setup: func(f *fakeOpenMeterClient) {
				f.ensurePlanFn = func(context.Context, openmeter.DesiredPlan) (om.Plan, error) { return om.Plan{}, permanent }
			},
			wantRequeue: permanentRequeueAfter,
		},
		{
			name: "plan unclassified is treated as transient",
			setup: func(f *fakeOpenMeterClient) {
				f.ensurePlanFn = func(context.Context, openmeter.DesiredPlan) (om.Plan, error) { return om.Plan{}, errors.New("??") }
			},
			wantRequeue: transientRequeueAfter,
		},
		{
			name: "meter lookup transient",
			setup: func(f *fakeOpenMeterClient) {
				f.getMeterFn = func(context.Context, string) (om.Meter, error) { return om.Meter{}, transient }
			},
			wantRequeue: transientRequeueAfter,
			wantNoPlan:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, omc, rec := newOfferReconciler(t, cpuMeter(), withFinalizer(cpuOffer()))
			tt.setup(omc)

			if res := reconcileOnce(t, r); res.RequeueAfter != tt.wantRequeue {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, tt.wantRequeue)
			}
			if tt.wantNoPlan && len(omc.ensurePlanCalls) != 0 {
				t.Errorf("EnsurePlan called after an earlier failure")
			}
			cond := syncedCondition(t, r)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditionReasonOpenMeterError {
				t.Errorf("condition = %+v, want False/%s", cond, conditionReasonOpenMeterError)
			}
			if !containsEvent(drainRecorder(rec), EventReasonSyncFailed) {
				t.Errorf("expected a SyncFailed event")
			}
		})
	}
}

// TestReconcile_RecoversAfterFailure: the condition flips back to True once
// OpenMeter recovers.
func TestReconcile_RecoversAfterFailure(t *testing.T) {
	r, omc, _ := newOfferReconciler(t, cpuMeter(), withFinalizer(cpuOffer()))
	omc.ensurePlanFn = func(context.Context, openmeter.DesiredPlan) (om.Plan, error) {
		return om.Plan{}, &openmeter.TransientError{Err: errors.New("down")}
	}
	reconcileOnce(t, r)
	omc.ensurePlanFn = nil
	reconcileOnce(t, r)
	if cond := syncedCondition(t, r); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("condition = %+v, want True after recovery", cond)
	}
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

func planVersion(id string, version int, status om.PlanStatus, featureKeys ...string) om.Plan {
	var cards []om.RateCard
	for _, key := range featureKeys {
		k := key
		var card om.RateCard
		_ = card.FromRateCardUsageBased(om.RateCardUsageBased{
			Type: om.RateCardUsageBasedTypeUsageBased, Key: k, Name: k, FeatureKey: &k,
		})
		cards = append(cards, card)
	}
	return om.Plan{
		Id: id, Key: "test_uid", Version: version, Status: status,
		Phases: []om.PlanPhase{{Key: planPhaseKey, Name: planPhaseName, RateCards: cards}},
	}
}

func TestReconcileDelete_NoPlan(t *testing.T) {
	r, omc, _ := newOfferReconciler(t, cpuMeter(), deletedOffer())

	if res := reconcileOnce(t, r); res.RequeueAfter != 0 {
		t.Errorf("unexpected requeue: %+v", res)
	}
	if len(omc.deletePlanCalls) != 0 {
		t.Errorf("DeletePlan called with no plan")
	}
	assertOfferGone(t, r)
}

// TestReconcileDelete_DeletesAllVersionsThenCollectsFeatures pins the
// ordering fix: every plan version is deleted BEFORE feature GC runs, and
// GC receives the union of features from every version (including already
// deleted ones, so a retry after a partial failure still collects them).
func TestReconcileDelete_DeletesAllVersionsThenCollectsFeatures(t *testing.T) {
	r, omc, rec := newOfferReconciler(t, cpuMeter(), deletedOffer())
	deletedAt := time.Now()
	gone := planVersion("p0", 1, om.PlanStatusArchived, "f_old")
	gone.DeletedAt = &deletedAt
	omc.listPlanVersionsFn = func(_ context.Context, key string, includeDeleted bool) ([]om.Plan, error) {
		if key != "test_uid" || !includeDeleted {
			t.Errorf("ListPlanVersions(%q, %v), want (test_uid, true)", key, includeDeleted)
		}
		return []om.Plan{
			gone,
			planVersion("p1", 2, om.PlanStatusArchived, "f_a", "f_b"),
			planVersion("p2", 3, om.PlanStatusActive, "f_b", "f_c"),
		}, nil
	}

	if res := reconcileOnce(t, r); res.RequeueAfter != 0 {
		t.Errorf("unexpected requeue: %+v", res)
	}
	want := []string{"ListPlanVersions test_uid", "DeletePlan p1", "DeletePlan p2", "ArchiveFeaturesIfUnreferenced"}
	if got := omc.callNames(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if !slices.Equal(omc.archiveFeaturesCalls, []string{"f_old", "f_a", "f_b", "f_c"}) {
		t.Errorf("archive candidates = %v, want [f_old f_a f_b f_c]", omc.archiveFeaturesCalls)
	}
	assertOfferGone(t, r)
	if !containsEvent(drainRecorder(rec), EventReasonDeleted) {
		t.Errorf("expected a Deleted event")
	}
}

func TestReconcileDelete_Failures(t *testing.T) {
	transient := &openmeter.TransientError{Err: errors.New("down")}
	permanent := &openmeter.PermanentError{Err: errors.New("rejected")}
	oneVersion := func(context.Context, string, bool) ([]om.Plan, error) {
		return []om.Plan{planVersion("p1", 1, om.PlanStatusActive, "f_a")}, nil
	}
	tests := []struct {
		name            string
		setup           func(*fakeOpenMeterClient)
		wantRequeue     time.Duration
		wantReleased    bool
		wantArchiveCall bool
	}{
		{
			name: "list transient keeps finalizer",
			setup: func(f *fakeOpenMeterClient) {
				f.listPlanVersionsFn = func(context.Context, string, bool) ([]om.Plan, error) { return nil, transient }
			},
			wantRequeue: transientRequeueAfter,
		},
		{
			name: "list permanent keeps finalizer (published plan state unknown)",
			setup: func(f *fakeOpenMeterClient) {
				f.listPlanVersionsFn = func(context.Context, string, bool) ([]om.Plan, error) { return nil, permanent }
			},
			wantRequeue: permanentRequeueAfter,
		},
		{
			name: "delete transient keeps finalizer and skips feature GC",
			setup: func(f *fakeOpenMeterClient) {
				f.listPlanVersionsFn = oneVersion
				f.deletePlanFn = func(context.Context, om.Plan) error { return transient }
			},
			wantRequeue: transientRequeueAfter,
		},
		{
			name: "delete permanent releases finalizer",
			setup: func(f *fakeOpenMeterClient) {
				f.listPlanVersionsFn = oneVersion
				f.deletePlanFn = func(context.Context, om.Plan) error { return permanent }
			},
			wantReleased:    true,
			wantArchiveCall: true,
		},
		{
			name: "feature GC transient keeps finalizer",
			setup: func(f *fakeOpenMeterClient) {
				f.listPlanVersionsFn = oneVersion
				f.archiveFeaturesFn = func(context.Context, []string) error { return transient }
			},
			wantRequeue:     transientRequeueAfter,
			wantArchiveCall: true,
		},
		{
			name: "feature GC permanent releases finalizer",
			setup: func(f *fakeOpenMeterClient) {
				f.listPlanVersionsFn = oneVersion
				f.archiveFeaturesFn = func(context.Context, []string) error { return permanent }
			},
			wantReleased:    true,
			wantArchiveCall: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, omc, rec := newOfferReconciler(t, cpuMeter(), deletedOffer())
			tt.setup(omc)

			if res := reconcileOnce(t, r); res.RequeueAfter != tt.wantRequeue {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, tt.wantRequeue)
			}
			if tt.wantReleased {
				assertOfferGone(t, r)
			} else {
				assertFinalizerKept(t, r)
			}
			if got := slices.Contains(omc.callNames(), "ArchiveFeaturesIfUnreferenced"); got != tt.wantArchiveCall {
				t.Errorf("feature GC called = %v, want %v (calls %v)", got, tt.wantArchiveCall, omc.calls)
			}
			if !containsEvent(drainRecorder(rec), EventReasonDeleteFailed) {
				t.Errorf("expected a DeleteFailed event")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Watches
// ---------------------------------------------------------------------------

func TestOffersForMeter(t *testing.T) {
	ga := cpuOffer()
	draft := cpuOffer()
	draft.Name, draft.UID = "draft-offer", "draft-uid"
	draft.Spec.LaunchStage = billingv1alpha1.OfferLaunchStageDraft
	other := baseOffer()
	other.Name, other.UID = "other-offer", "other-uid"
	other.Spec.ServicePricings = []billingv1alpha1.ServicePricingSnapshot{{
		Name: "x", Spec: billingv1alpha1.ServicePricingSpec{ChargeType: billingv1alpha1.ChargeTypeUsage, Metric: "other/metric"},
	}}
	r, _, _ := newOfferReconciler(t, ga, draft, other)

	reqs := r.offersForMeter(context.Background(), cpuMeter())
	if len(reqs) != 1 || reqs[0].Name != "test-offer" {
		t.Fatalf("offersForMeter = %v, want only test-offer", reqs)
	}
}

// ---------------------------------------------------------------------------
// fake OpenMeter client
// ---------------------------------------------------------------------------

// fakeOpenMeterClient records every call (in order) and delegates to
// overridable fns. Defaults model a healthy OpenMeter.
type fakeOpenMeterClient struct {
	openmeter.Client // unimplemented methods panic if used

	calls []string

	getMeterFn func(context.Context, string) (om.Meter, error)

	ensureFeatureCalls []openmeter.DesiredFeature
	ensureFeatureFn    func(context.Context, openmeter.DesiredFeature) (om.Feature, error)

	ensurePlanCalls []openmeter.DesiredPlan
	ensurePlanFn    func(context.Context, openmeter.DesiredPlan) (om.Plan, error)

	listPlanVersionsFn func(context.Context, string, bool) ([]om.Plan, error)

	deletePlanCalls []string
	deletePlanFn    func(context.Context, om.Plan) error

	archiveFeaturesCalls []string
	archiveFeaturesFn    func(context.Context, []string) error

	archiveMeterFeaturesCalls []string
	archiveMeterFeaturesFn    func(context.Context, string) error
}

// callNames returns the recorded calls with EnsureFeature arguments elided.
func (f *fakeOpenMeterClient) callNames() []string {
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		if strings.HasPrefix(c, "EnsureFeature") {
			c = "EnsureFeature"
		}
		out = append(out, c)
	}
	return out
}

func (f *fakeOpenMeterClient) GetMeter(ctx context.Context, slug string) (om.Meter, error) {
	f.calls = append(f.calls, "GetMeter "+slug)
	if f.getMeterFn != nil {
		return f.getMeterFn(ctx, slug)
	}
	return om.Meter{Slug: slug}, nil
}

func (f *fakeOpenMeterClient) EnsureFeature(ctx context.Context, d openmeter.DesiredFeature) (om.Feature, error) {
	f.calls = append(f.calls, "EnsureFeature "+d.Key)
	f.ensureFeatureCalls = append(f.ensureFeatureCalls, d)
	if f.ensureFeatureFn != nil {
		return f.ensureFeatureFn(ctx, d)
	}
	return om.Feature{Key: d.Key}, nil
}

func (f *fakeOpenMeterClient) EnsurePlan(ctx context.Context, d openmeter.DesiredPlan) (om.Plan, error) {
	f.calls = append(f.calls, "EnsurePlan "+d.Key)
	f.ensurePlanCalls = append(f.ensurePlanCalls, d)
	if f.ensurePlanFn != nil {
		return f.ensurePlanFn(ctx, d)
	}
	return om.Plan{Id: "plan-1", Key: d.Key, Name: d.Name, Version: 1, Status: om.PlanStatusActive}, nil
}

func (f *fakeOpenMeterClient) ListPlanVersions(ctx context.Context, key string, includeDeleted bool) ([]om.Plan, error) {
	f.calls = append(f.calls, "ListPlanVersions "+key)
	if f.listPlanVersionsFn != nil {
		return f.listPlanVersionsFn(ctx, key, includeDeleted)
	}
	return nil, nil
}

func (f *fakeOpenMeterClient) DeletePlan(ctx context.Context, plan om.Plan) error {
	f.calls = append(f.calls, "DeletePlan "+plan.Id)
	f.deletePlanCalls = append(f.deletePlanCalls, plan.Id)
	if f.deletePlanFn != nil {
		return f.deletePlanFn(ctx, plan)
	}
	return nil
}

func (f *fakeOpenMeterClient) ArchiveFeaturesIfUnreferenced(ctx context.Context, keys []string) error {
	f.calls = append(f.calls, "ArchiveFeaturesIfUnreferenced")
	f.archiveFeaturesCalls = append(f.archiveFeaturesCalls, keys...)
	if f.archiveFeaturesFn != nil {
		return f.archiveFeaturesFn(ctx, keys)
	}
	return nil
}

func (f *fakeOpenMeterClient) ArchiveUnreferencedMeterFeatures(ctx context.Context, meterSlug string) error {
	f.calls = append(f.calls, "ArchiveUnreferencedMeterFeatures "+meterSlug)
	f.archiveMeterFeaturesCalls = append(f.archiveMeterFeaturesCalls, meterSlug)
	if f.archiveMeterFeaturesFn != nil {
		return f.archiveMeterFeaturesFn(ctx, meterSlug)
	}
	return nil
}
