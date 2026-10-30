// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	om "github.com/openmeterio/openmeter/api/client/go"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

const (
	testMDName    = "cpu-seconds"
	testMeterName = "compute.miloapis.com/instance/cpu-seconds"
	testMeterSlug = "compute_miloapis_com_instance_cpu_seconds"
)

// fakeMeterClient is an in-memory OpenMeter meter store implementing the
// slice of openmeter.Client the MeterDefinition reconciler uses. Any other
// method panics via the embedded nil interface.
type fakeMeterClient struct {
	openmeter.Client

	meters map[string]om.Meter

	ensureCalls  []openmeter.DesiredMeter
	archiveCalls []string
	deleteCalls  []string

	ensureErr  error
	getErr     error
	archiveErr error
	deleteErr  error
}

func newFakeMeterClient() *fakeMeterClient {
	return &fakeMeterClient{meters: map[string]om.Meter{}}
}

func (f *fakeMeterClient) EnsureMeter(_ context.Context, d openmeter.DesiredMeter) (om.Meter, bool, error) {
	f.ensureCalls = append(f.ensureCalls, d)
	if f.ensureErr != nil {
		return om.Meter{}, false, f.ensureErr
	}
	want := om.Meter{
		Slug:          d.Slug,
		EventType:     d.EventType,
		Aggregation:   d.Aggregation,
		Name:          &d.Name,
		Description:   &d.Description,
		GroupBy:       &d.GroupBy,
		ValueProperty: &d.ValueProperty,
	}
	existing, ok := f.meters[d.Slug]
	if ok && existing.EventType != d.EventType {
		return om.Meter{}, false, &openmeter.PermanentError{Err: fmt.Errorf("meter %s: eventType %q cannot change", d.Slug, existing.EventType)}
	}
	if ok && *existing.Name == d.Name && *existing.Description == d.Description && maps.Equal(*existing.GroupBy, d.GroupBy) {
		return existing, false, nil
	}
	f.meters[d.Slug] = want
	return want, true, nil
}

func (f *fakeMeterClient) GetMeter(_ context.Context, slug string) (om.Meter, error) {
	if f.getErr != nil {
		return om.Meter{}, f.getErr
	}
	m, ok := f.meters[slug]
	if !ok {
		return om.Meter{}, fmt.Errorf("%w: %s", openmeter.ErrMeterNotFound, slug)
	}
	return m, nil
}

func (f *fakeMeterClient) ArchiveUnreferencedMeterFeatures(_ context.Context, slug string) error {
	f.archiveCalls = append(f.archiveCalls, slug)
	return f.archiveErr
}

func (f *fakeMeterClient) DeleteMeter(_ context.Context, slug string) error {
	f.deleteCalls = append(f.deleteCalls, slug)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.meters, slug)
	return nil
}

func newMeterScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := billingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return scheme
}

func testMeterDefinition(mutate ...func(*billingv1alpha1.MeterDefinition)) *billingv1alpha1.MeterDefinition {
	md := &billingv1alpha1.MeterDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testMDName,
			UID:        types.UID("md-uid"),
			Finalizers: []string{MeterFinalizer},
		},
		Spec: billingv1alpha1.MeterDefinitionSpec{
			MeterName:   testMeterName,
			Phase:       billingv1alpha1.PhasePublished,
			DisplayName: "CPU Seconds",
			Measurement: billingv1alpha1.MeterMeasurement{
				Aggregation: billingv1alpha1.MeterAggregationSum,
				Unit:        "s",
				Dimensions:  []string{"region"},
			},
			MonitoredResourceTypes: []string{"compute.miloapis.com/instance"},
		},
	}
	for _, m := range mutate {
		m(md)
	}
	return md
}

func deleting(md *billingv1alpha1.MeterDefinition) {
	now := metav1.Now()
	md.DeletionTimestamp = &now
}

// meterTestEnv bundles a reconciler wired to a fake k8s client and a fake
// OpenMeter client.
type meterTestEnv struct {
	r       *MeterDefinitionReconciler
	omc     *fakeMeterClient
	rec     *record.FakeRecorder
	updates int
	patches []client.Patch
}

func newMeterTestEnv(t *testing.T, objs ...client.Object) *meterTestEnv {
	t.Helper()
	env := &meterTestEnv{omc: newFakeMeterClient(), rec: record.NewFakeRecorder(50)}
	k8s := fake.NewClientBuilder().
		WithScheme(newMeterScheme(t)).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				env.updates++
				return c.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				env.patches = append(env.patches, patch)
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	env.r = &MeterDefinitionReconciler{Client: k8s, OpenMeterClient: env.omc, Recorder: env.rec}
	env.r.Finalizers = finalizer.NewFinalizers()
	if err := env.r.Finalizers.Register(MeterFinalizer, &meterLinkFinalizer{OpenMeterClient: env.omc, Recorder: env.rec}); err != nil {
		t.Fatalf("register finalizer: %v", err)
	}
	return env
}

func (e *meterTestEnv) reconcile(t *testing.T) (reconcile.Result, error) {
	t.Helper()
	return e.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: testMDName}})
}

func (e *meterTestEnv) get(t *testing.T) (*billingv1alpha1.MeterDefinition, bool) {
	t.Helper()
	var md billingv1alpha1.MeterDefinition
	err := e.r.Get(context.Background(), types.NamespacedName{Name: testMDName}, &md)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("get MeterDefinition: %v", err)
	}
	return &md, true
}

// events drains the fake recorder.
func (e *meterTestEnv) events() []string {
	var out []string
	for {
		select {
		case ev := <-e.rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func hasEvent(events []string, reason string) bool {
	return slices.ContainsFunc(events, func(e string) bool { return strings.Contains(e, " "+reason+" ") })
}

// ---------------------------------------------------------------------------
// desiredMeter
// ---------------------------------------------------------------------------

func TestDesiredMeter_Aggregations(t *testing.T) {
	tests := []struct {
		aggregation       billingv1alpha1.MeterAggregation
		wantAggregation   om.MeterAggregation
		wantValueProperty string
	}{
		{billingv1alpha1.MeterAggregationSum, om.MeterAggregationSum, "$.value"},
		{billingv1alpha1.MeterAggregationCount, om.MeterAggregationCount, "$.value"},
		{billingv1alpha1.MeterAggregationUniqueCount, om.MeterAggregationUniqueCount, "$.resource.uid"},
		{billingv1alpha1.MeterAggregationMax, om.MeterAggregationMax, "$.value"},
		{billingv1alpha1.MeterAggregationMin, om.MeterAggregationMin, "$.value"},
		{billingv1alpha1.MeterAggregationLatest, om.MeterAggregationLatest, "$.value"},
		{billingv1alpha1.MeterAggregationAverage, om.MeterAggregationAvg, "$.value"},
	}
	for _, tt := range tests {
		t.Run(string(tt.aggregation), func(t *testing.T) {
			md := testMeterDefinition(func(md *billingv1alpha1.MeterDefinition) {
				md.Spec.Measurement.Aggregation = tt.aggregation
			})
			got, err := desiredMeter(md)
			if err != nil {
				t.Fatalf("desiredMeter: %v", err)
			}
			if got.Aggregation != tt.wantAggregation {
				t.Errorf("Aggregation = %q, want %q", got.Aggregation, tt.wantAggregation)
			}
			// For Count the client drops the valueProperty on the wire.
			if got.ValueProperty != tt.wantValueProperty {
				t.Errorf("ValueProperty = %q, want %q", got.ValueProperty, tt.wantValueProperty)
			}
			if got.Slug != testMeterSlug || got.EventType != testMeterName {
				t.Errorf("Slug/EventType = %q/%q, want %q/%q", got.Slug, got.EventType, testMeterSlug, testMeterName)
			}
		})
	}
}

func TestDesiredMeter_NameAndDescription(t *testing.T) {
	got, err := desiredMeter(testMeterDefinition())
	if err != nil {
		t.Fatalf("desiredMeter: %v", err)
	}
	if got.Name != "CPU Seconds" || got.Description != "CPU Seconds" {
		t.Errorf("Name/Description = %q/%q, want displayName for both", got.Name, got.Description)
	}

	got, err = desiredMeter(testMeterDefinition(func(md *billingv1alpha1.MeterDefinition) {
		md.Spec.Description = "Seconds of vCPU time"
	}))
	if err != nil {
		t.Fatalf("desiredMeter: %v", err)
	}
	if got.Name != "CPU Seconds" || got.Description != "Seconds of vCPU time" {
		t.Errorf("Name/Description = %q/%q, want displayName/description", got.Name, got.Description)
	}
}

func TestDesiredMeter_Dimensions(t *testing.T) {
	tests := []struct {
		name             string
		dimensions       []string
		systemDimensions []string
		want             map[string]string
	}{
		{
			name:       "declared dimensions plus project_name fallback",
			dimensions: []string{"region", "tier"},
			want: map[string]string{
				"region":       "$.dimensions.region",
				"tier":         "$.dimensions.tier",
				"project_name": "$.dimensions.project_name",
			},
		},
		{
			name:       "sanitized keys with quoted paths",
			dimensions: []string{"resource.type", "k8s.io/zone"},
			want: map[string]string{
				"resource_type": `$.dimensions."resource.type"`,
				"k8s_io_zone":   `$.dimensions."k8s.io/zone"`,
				"project_name":  "$.dimensions.project_name",
			},
		},
		{
			name:             "system dimensions from status",
			dimensions:       []string{"region"},
			systemDimensions: []string{"project_name", "organization_name"},
			want: map[string]string{
				"region":            "$.dimensions.region",
				"project_name":      "$.dimensions.project_name",
				"organization_name": "$.dimensions.organization_name",
			},
		},
		{
			name:       "project_name declared explicitly is folded",
			dimensions: []string{"project_name"},
			want:       map[string]string{"project_name": "$.dimensions.project_name"},
		},
		{
			name:       "reserved query group-by is prefixed",
			dimensions: []string{"subject"},
			want: map[string]string{
				"dim_subject":  "$.dimensions.subject",
				"project_name": "$.dimensions.project_name",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			md := testMeterDefinition(func(md *billingv1alpha1.MeterDefinition) {
				md.Spec.Measurement.Dimensions = tt.dimensions
				md.Status.SystemDimensions = tt.systemDimensions
			})
			got, err := desiredMeter(md)
			if err != nil {
				t.Fatalf("desiredMeter: %v", err)
			}
			if !maps.Equal(got.GroupBy, tt.want) {
				t.Errorf("GroupBy = %v, want %v", got.GroupBy, tt.want)
			}
		})
	}
}

func TestDesiredMeter_DimensionCollisionIsPermanent(t *testing.T) {
	md := testMeterDefinition(func(md *billingv1alpha1.MeterDefinition) {
		md.Spec.Measurement.Dimensions = []string{"resource.type", "resource_type"}
	})
	if _, err := desiredMeter(md); !openmeter.IsPermanent(err) {
		t.Fatalf("desiredMeter error = %v, want a PermanentError", err)
	}
}

// ---------------------------------------------------------------------------
// Reconcile
// ---------------------------------------------------------------------------

func TestReconcileMeter_AddsFinalizerFirst(t *testing.T) {
	env := newMeterTestEnv(t, testMeterDefinition(func(md *billingv1alpha1.MeterDefinition) {
		md.Finalizers = nil
	}))

	if _, err := env.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	md, _ := env.get(t)
	if !slices.Contains(md.Finalizers, MeterFinalizer) {
		t.Errorf("finalizers = %v, want %q", md.Finalizers, MeterFinalizer)
	}
	if len(env.omc.ensureCalls) != 0 {
		t.Error("EnsureMeter called in the same pass that added the finalizer")
	}
	// L6: the finalizer is persisted as a merge patch, never a full Update
	// that would strip spec fields this binary does not know about.
	if env.updates != 0 {
		t.Errorf("full Updates = %d, want 0", env.updates)
	}
	if len(env.patches) != 1 || env.patches[0].Type() != types.MergePatchType {
		t.Errorf("patches = %v, want one merge patch", env.patches)
	}
}

// TestReconcileMeter_SyncsEveryPhase: the meter must exist for every phase —
// late events of Deprecated/Retired meters are still forwarded and billed.
func TestReconcileMeter_SyncsEveryPhase(t *testing.T) {
	for _, phase := range []billingv1alpha1.Phase{
		billingv1alpha1.PhaseDraft,
		billingv1alpha1.PhasePublished,
		billingv1alpha1.PhaseDeprecated,
		billingv1alpha1.PhaseRetired,
	} {
		t.Run(string(phase), func(t *testing.T) {
			env := newMeterTestEnv(t, testMeterDefinition(func(md *billingv1alpha1.MeterDefinition) {
				md.Spec.Phase = phase
			}))
			res, err := env.reconcile(t)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if _, ok := env.omc.meters[testMeterSlug]; !ok {
				t.Errorf("meter not created for phase %s", phase)
			}
			if res.RequeueAfter != meterResyncInterval {
				t.Errorf("RequeueAfter = %v, want the periodic resync %v", res.RequeueAfter, meterResyncInterval)
			}
		})
	}
}

// TestReconcileMeter_SyncedEventOnlyOnChange: billing rewrites status on its
// own schedule; an unchanged meter must not emit a Synced event per write.
func TestReconcileMeter_SyncedEventOnlyOnChange(t *testing.T) {
	env := newMeterTestEnv(t, testMeterDefinition())

	if _, err := env.reconcile(t); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if evs := env.events(); !hasEvent(evs, EventReasonSynced) {
		t.Errorf("events after create = %v, want %s", evs, EventReasonSynced)
	}

	res, err := env.reconcile(t)
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if evs := env.events(); len(evs) != 0 {
		t.Errorf("events after a no-op reconcile = %v, want none", evs)
	}
	if res.RequeueAfter != meterResyncInterval {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, meterResyncInterval)
	}
}

// TestReconcileMeter_RecreatesDeletedMeter: the periodic resync re-runs
// EnsureMeter, which repairs a meter deleted directly in OpenMeter.
func TestReconcileMeter_RecreatesDeletedMeter(t *testing.T) {
	env := newMeterTestEnv(t, testMeterDefinition())
	if _, err := env.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	delete(env.omc.meters, testMeterSlug)
	_ = env.events()

	if _, err := env.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := env.omc.meters[testMeterSlug]; !ok {
		t.Error("meter not recreated")
	}
	if evs := env.events(); !hasEvent(evs, EventReasonSynced) {
		t.Errorf("events = %v, want %s for the recreate", evs, EventReasonSynced)
	}
}

func TestReconcileMeter_OpenMeterErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"transient", &openmeter.TransientError{Err: errors.New("503")}, syncReasonTransient},
		{"permanent", &openmeter.PermanentError{Err: errors.New("400")}, syncReasonPermanent},
		{"unclassified", errors.New("boom"), syncReasonInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newMeterTestEnv(t, testMeterDefinition())
			env.omc.ensureErr = tt.err

			// Every failure is returned, so controller-runtime counts it and
			// retries with backoff, permanent ones included.
			res, err := env.reconcile(t)
			if !errors.Is(err, tt.err) {
				t.Fatalf("Reconcile error = %v, want it to wrap %v", err, tt.err)
			}
			if !res.IsZero() {
				t.Errorf("Result = %+v, want zero alongside an error", res)
			}
			evs := env.events()
			if !hasEvent(evs, EventReasonSyncFailed) || !strings.Contains(strings.Join(evs, "\n"), tt.want+":") {
				t.Errorf("events = %v, want %s with reason prefix %s", evs, EventReasonSyncFailed, tt.want)
			}
		})
	}
}

func TestReconcileMeter_UnrepresentableDimensionsArePermanent(t *testing.T) {
	env := newMeterTestEnv(t, testMeterDefinition(func(md *billingv1alpha1.MeterDefinition) {
		md.Spec.Measurement.Dimensions = []string{"a-b", "a.b"}
	}))

	_, err := env.reconcile(t)
	if !openmeter.IsPermanent(err) {
		t.Fatalf("Reconcile error = %v, want a permanent error", err)
	}
	if len(env.omc.ensureCalls) != 0 {
		t.Error("EnsureMeter called for an unrepresentable dimension set")
	}
	if evs := env.events(); !hasEvent(evs, EventReasonSyncFailed) {
		t.Errorf("events = %v, want %s", evs, EventReasonSyncFailed)
	}
}

// ---------------------------------------------------------------------------
// Finalizer
// ---------------------------------------------------------------------------

func TestMeterFinalizer_DeletesOwnMeter(t *testing.T) {
	env := newMeterTestEnv(t, testMeterDefinition(deleting))
	env.omc.meters[testMeterSlug] = om.Meter{Slug: testMeterSlug, EventType: testMeterName}

	if _, err := env.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !slices.Equal(env.omc.archiveCalls, []string{testMeterSlug}) || !slices.Equal(env.omc.deleteCalls, []string{testMeterSlug}) {
		t.Errorf("archive/delete calls = %v/%v, want both for %s", env.omc.archiveCalls, env.omc.deleteCalls, testMeterSlug)
	}
	if _, exists := env.get(t); exists {
		t.Error("MeterDefinition still present; the finalizer should have been released")
	}
	if env.updates != 0 {
		t.Errorf("full Updates = %d, want 0 (merge patch only)", env.updates)
	}
	if evs := env.events(); !hasEvent(evs, EventReasonDeleted) {
		t.Errorf("events = %v, want %s", evs, EventReasonDeleted)
	}
}

// TestMeterFinalizer_SlugCollisionGuard is the regression test for deleting
// the losing MeterDefinition of a slug collision taking the winner's meter
// (and its features) down with it.
func TestMeterFinalizer_SlugCollisionGuard(t *testing.T) {
	env := newMeterTestEnv(t, testMeterDefinition(deleting))
	// Another MeterDefinition ("compute.miloapis.com/instance/cpu_seconds")
	// owns the meter under the same slug.
	env.omc.meters[testMeterSlug] = om.Meter{Slug: testMeterSlug, EventType: "compute.miloapis.com/instance/cpu_seconds"}

	if _, err := env.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(env.omc.archiveCalls) != 0 || len(env.omc.deleteCalls) != 0 {
		t.Errorf("archive/delete calls = %v/%v, want none for another meterName's meter",
			env.omc.archiveCalls, env.omc.deleteCalls)
	}
	if _, ok := env.omc.meters[testMeterSlug]; !ok {
		t.Error("the other MeterDefinition's meter was deleted")
	}
	if _, exists := env.get(t); exists {
		t.Error("MeterDefinition still present; the finalizer should have been released")
	}
	if evs := env.events(); !hasEvent(evs, EventReasonDeleteSkipped) {
		t.Errorf("events = %v, want %s", evs, EventReasonDeleteSkipped)
	}
}

func TestMeterFinalizer_AlreadyAbsent(t *testing.T) {
	env := newMeterTestEnv(t, testMeterDefinition(deleting))

	if _, err := env.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(env.omc.deleteCalls) != 0 {
		t.Errorf("DeleteMeter calls = %v, want none for an absent meter", env.omc.deleteCalls)
	}
	if _, exists := env.get(t); exists {
		t.Error("MeterDefinition still present; the finalizer should have been released")
	}
}

func TestMeterFinalizer_FailuresKeepFinalizer(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeMeterClient)
	}{
		{"GetMeter transient", func(f *fakeMeterClient) { f.getErr = &openmeter.TransientError{Err: errors.New("503")} }},
		{"GetMeter unrecognized 404", func(f *fakeMeterClient) {
			f.getErr = &openmeter.PermanentError{Err: errors.New("404 not from OpenMeter"), StatusCode: 404}
		}},
		{"archive features", func(f *fakeMeterClient) { f.archiveErr = errors.New("list features failed") }},
		{"DeleteMeter conflict", func(f *fakeMeterClient) {
			f.deleteErr = &openmeter.PermanentError{Err: errors.New("meter has active features"), StatusCode: 409}
		}},
		{"DeleteMeter transient", func(f *fakeMeterClient) { f.deleteErr = &openmeter.TransientError{Err: errors.New("503")} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newMeterTestEnv(t, testMeterDefinition(deleting))
			env.omc.meters[testMeterSlug] = om.Meter{Slug: testMeterSlug, EventType: testMeterName}
			tt.setup(env.omc)

			if _, err := env.reconcile(t); err == nil {
				t.Fatal("Reconcile: expected the failing cleanup to surface an error for backoff")
			}
			md, exists := env.get(t)
			if !exists || !slices.Contains(md.Finalizers, MeterFinalizer) {
				t.Error("finalizer released despite a failed cleanup")
			}
			if evs := env.events(); !hasEvent(evs, EventReasonDeleteFailed) {
				t.Errorf("events = %v, want %s", evs, EventReasonDeleteFailed)
			}
		})
	}
}
