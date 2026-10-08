// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
// Test fixtures
// ---------------------------------------------------------------------------

// baseOffer returns a GA offer with a stable UID and no servicePricings. Tests
// that need a synced plan attach a snapshot via addSnapshot.
func baseOffer() *billingv1alpha1.Offer {
	return &billingv1alpha1.Offer{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-offer",
			UID:  types.UID("test-uid"),
		},
		Spec: billingv1alpha1.OfferSpec{
			LaunchStage: billingv1alpha1.OfferLaunchStageGA,
		},
	}
}

// cpuMeter returns a MeterDefinition whose metadata.name deliberately differs
// from its canonical reverse-DNS meterName — the exact shape that used to
// break feature creation (the offer controller once derived the OpenMeter slug
// from metadata.name instead of spec.meterName).
func cpuMeter() *billingv1alpha1.MeterDefinition {
	return &billingv1alpha1.MeterDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cpu-meter",
		},
		Spec: billingv1alpha1.MeterDefinitionSpec{
			MeterName: "compute.miloapis.com/cpu",
		},
	}
}

// cpuOffer returns a fully-populated GA offer covering every pricing shape:
// usage with two region-matched rates and a default catch-all, a recurring
// flat fee, and a one-time flat fee.
func cpuOffer() *billingv1alpha1.Offer {
	o := baseOffer()
	o.Spec.ServicePricings = []billingv1alpha1.ServicePricingSnapshot{
		{
			Name: "usage-item-multi-region",
			Spec: billingv1alpha1.ServicePricingSpec{
				DisplayName: "Compute Usage",
				ChargeType:  billingv1alpha1.ChargeTypeUsage,
				Metric:      "compute.miloapis.com/cpu",
				PricingUnit: "vcpu",
				ServiceRef:  "compute.miloapis.com",
				Rates: []billingv1alpha1.PricingRate{
					{
						Match: &billingv1alpha1.DimensionMatch{
							Dimension: "region",
							Value:     "us-east",
						},
						Flat: "0.10",
					},
					{
						Match: &billingv1alpha1.DimensionMatch{
							Dimension: "region",
							Value:     "eu-west",
						},
						Tiered: []billingv1alpha1.PricingTierBand{
							{Rate: "0.12", UpTo: "100"},
							{Rate: "0.08"},
						},
					},
					{
						Flat: "0.05",
					},
				},
			},
		},
		{
			Name: "recurring-base-fee",
			Spec: billingv1alpha1.ServicePricingSpec{
				DisplayName: "Monthly Platform Fee",
				ChargeType:  billingv1alpha1.ChargeTypeRecurring,
				Amount:      "50.00",
				Interval:    billingv1alpha1.ChargeIntervalMonthly,
				ServiceRef:  "platform.miloapis.com",
			},
		},
		{
			Name: "one-time-setup",
			Spec: billingv1alpha1.ServicePricingSpec{
				DisplayName: "Setup Fee",
				ChargeType:  billingv1alpha1.ChargeTypeOneTime,
				Amount:      "100.00",
				Trigger:     billingv1alpha1.ChargeTriggerBillingAccountActivation,
				ServiceRef:  "platform.miloapis.com",
			},
		},
	}
	return o
}

// newOfferScheme builds the scheme with the billing API types plus the core
// client-go scheme (some reconciler paths List core types indirectly).
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
// seeded with the given MeterDefinitions (and, when provided, offers) and a
// tracking fake OpenMeter client.
func newOfferReconciler(t *testing.T, objs ...client.Object) (*OfferReconciler, *fakeOpenMeterClient) {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newOfferScheme(t)).WithObjects(objs...).Build()
	omc := &fakeOpenMeterClient{}
	return &OfferReconciler{Client: k8s, OpenMeterClient: omc}, omc
}

func mustDesiredPlan(t *testing.T, r *OfferReconciler, offer *billingv1alpha1.Offer) openmeter.DesiredPlan {
	t.Helper()
	dp, err := r.desiredPlan(context.Background(), offer)
	if err != nil {
		t.Fatalf("desiredPlan: %v", err)
	}
	if len(dp.Phases) != 1 {
		t.Fatalf("desiredPlan: expected 1 phase, got %d", len(dp.Phases))
	}
	return dp
}

// mustUsageRateCard extracts the i-th rate card as a usage-based card.
func mustUsageRateCard(t *testing.T, phase om.PlanPhase, i int) om.RateCardUsageBased {
	t.Helper()
	rc, err := phase.RateCards[i].AsRateCardUsageBased()
	if err != nil {
		t.Fatalf("rate card %d: expected usage_based: %v", i, err)
	}
	return rc
}

// mustFlatRateCard extracts the i-th rate card as a flat-fee card.
func mustFlatRateCard(t *testing.T, phase om.PlanPhase, i int) om.RateCardFlatFee {
	t.Helper()
	rc, err := phase.RateCards[i].AsRateCardFlatFee()
	if err != nil {
		t.Fatalf("rate card %d: expected flat_fee: %v", i, err)
	}
	return rc
}

// ---------------------------------------------------------------------------
// TestSanitizeKeyPart exercises the regex-safe key encoding (regression for
// feature/rate-card keys that used to be rejected by OpenMeter's key regex).
// ---------------------------------------------------------------------------

func TestSanitizeKeyPart(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain lowercase", "region", "region"},
		{"hyphen to underscore", "us-east", "us_east"},
		{"dots collapsed", "v1.0", "v1_0"},
		{"double hyphen collapses", "us--east", "us_east"},
		{"mixed case lowercases", "US-East", "us_east"},
		{"spaces collapse", "new york", "new_york"},
		{"slash collapses", "a/b", "a_b"},
		{"leading/trailing junk trimmed", " us ", "us"},
		{"leading junk only", "_us", "us"},
		{"digits kept", "gpt-4o", "gpt_4o"},
		{"all junk becomes empty", "---", ""},
		{"underscore preserved", "a_b", "a_b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeKeyPart(tt.in); got != tt.want {
				t.Errorf("sanitizeKeyPart(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// desiredPlan mapping
// ---------------------------------------------------------------------------

// TestDesiredPlan_NamePrecedence verifies the display-name annotation is used
// when present, and metadata.name otherwise; Key is UID-derived and sanitized.
func TestDesiredPlan_NamePrecedence(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantName    string
	}{
		{name: "annotation present", annotations: map[string]string{billingv1alpha1.DisplayNameAnnotation: "Custom Display Name"}, wantName: "Custom Display Name"},
		{name: "no annotation falls back to metadata name", annotations: nil, wantName: "test-offer"},
		{name: "empty annotation falls back to metadata name", annotations: map[string]string{billingv1alpha1.DisplayNameAnnotation: ""}, wantName: "test-offer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			offer := cpuOffer()
			offer.Annotations = tt.annotations

			r, _ := newOfferReconciler(t, cpuMeter())
			dp := mustDesiredPlan(t, r, offer)

			if dp.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", dp.Name, tt.wantName)
			}
			if dp.Key != "test_uid" {
				t.Errorf("Key = %q, want %q", dp.Key, "test_uid")
			}
			if dp.Description != "test-offer" {
				t.Errorf("Description = %q, want %q", dp.Description, "test-offer")
			}
			if dp.Currency != "USD" {
				t.Errorf("Currency = %q, want USD", dp.Currency)
			}
		})
	}
}

// TestDesiredPlan_ExhaustiveMapping is the core mapping test: it drives the
// full cpuOffer through desiredPlan and asserts every OpenMeter artifact —
// feature keys (derived from spec.meterName, not metadata.name), rate card
// types, prices, billing cadences, and metadata.
func TestDesiredPlan_ExhaustiveMapping(t *testing.T) {
	r, omc := newOfferReconciler(t, cpuMeter())
	dp := mustDesiredPlan(t, r, cpuOffer())

	phase := dp.Phases[0]
	if phase.Key != "default_phase" {
		t.Errorf("phase key = %q, want default_phase", phase.Key)
	}
	if len(phase.RateCards) != 5 {
		t.Fatalf("expected 5 rate cards, got %d", len(phase.RateCards))
	}

	// --- Features ---
	// us-east, eu-west, and the default catch-all each get a feature. The
	// slug MUST come from spec.meterName ("compute.miloapis.com/cpu" ->
	// "compute_miloapis_com_cpu"); the old code derived it from the
	// metadata.name "cpu-meter" and produced features pointing at a meter
	// that never existed.
	if len(omc.ensureFeatureCalls) != 3 {
		t.Fatalf("expected 3 EnsureFeature calls, got %d", len(omc.ensureFeatureCalls))
	}
	wantFeatures := []struct {
		key             string
		meterSlug       string
		dimension, val  string
		hasFilter       bool
	}{
		{key: "compute_miloapis_com_cpu_region_us_east", meterSlug: "compute_miloapis_com_cpu", dimension: "region", val: "us-east", hasFilter: true},
		{key: "compute_miloapis_com_cpu_region_eu_west", meterSlug: "compute_miloapis_com_cpu", dimension: "region", val: "eu-west", hasFilter: true},
		{key: "compute_miloapis_com_cpu_default", meterSlug: "compute_miloapis_com_cpu", hasFilter: false},
	}
	for i, want := range wantFeatures {
		call := omc.ensureFeatureCalls[i]
		if call.Key != want.key {
			t.Errorf("feature %d key = %q, want %q", i, call.Key, want.key)
		}
		if call.MeterSlug != want.meterSlug {
			t.Errorf("feature %d meterSlug = %q, want %q", i, call.MeterSlug, want.meterSlug)
		}
		if call.Name != want.key {
			t.Errorf("feature %d name = %q, want key %q", i, call.Name, want.key)
		}
		if want.hasFilter {
			filter, ok := call.AdvancedMeterGroupByFilters[want.dimension]
			if !ok {
				t.Errorf("feature %d expected filter on dimension %q", i, want.dimension)
				continue
			}
			// The filter value must be the RAW (unsanitized) value.
			if filter.Eq == nil || *filter.Eq != want.val {
				t.Errorf("feature %d %s filter = %v, want %q", i, want.dimension, filter, want.val)
			}
		} else if len(call.AdvancedMeterGroupByFilters) != 0 {
			t.Errorf("feature %d expected no filters, got %v", i, call.AdvancedMeterGroupByFilters)
		}
	}

	// --- Rate cards (usage) ---
	rcUs := mustUsageRateCard(t, phase, 0)
	if rcUs.Key != "compute_miloapis_com_cpu_region_us_east" {
		t.Errorf("us-east key = %q", rcUs.Key)
	}
	if rcUs.FeatureKey == nil || *rcUs.FeatureKey != rcUs.Key {
		t.Errorf("us-east featureKey not linked to key: %v", rcUs.FeatureKey)
	}
	if rcUs.BillingCadence != "P1M" {
		t.Errorf("us-east cadence = %q, want P1M", rcUs.BillingCadence)
	}
	unit, err := rcUs.Price.AsUnitPriceWithCommitments()
	if err != nil || unit.Amount != om.Numeric("0.10") {
		t.Errorf("us-east price = %v (err %v), want unit 0.10", rcUs.Price, err)
	}

	rcEu := mustUsageRateCard(t, phase, 1)
	if rcEu.Key != "compute_miloapis_com_cpu_region_eu_west" {
		t.Errorf("eu-west key = %q", rcEu.Key)
	}
	tiered, err := rcEu.Price.AsTieredPriceWithCommitments()
	if err != nil {
		t.Fatalf("eu-west price not tiered: %v", err)
	}
	if tiered.Mode != om.TieredPriceModeGraduated {
		t.Errorf("eu-west mode = %q, want graduated", tiered.Mode)
	}
	if len(tiered.Tiers) != 2 {
		t.Fatalf("eu-west tiers = %d, want 2", len(tiered.Tiers))
	}
	if tiered.Tiers[0].UpToAmount == nil || *tiered.Tiers[0].UpToAmount != om.Numeric("100") {
		t.Errorf("eu-west tier0 upTo = %v, want 100", tiered.Tiers[0].UpToAmount)
	}
	if tiered.Tiers[0].UnitPrice == nil || tiered.Tiers[0].UnitPrice.Amount != om.Numeric("0.12") {
		t.Errorf("eu-west tier0 rate = %v", tiered.Tiers[0].UnitPrice)
	}
	if tiered.Tiers[1].UpToAmount != nil {
		t.Errorf("eu-west final tier upTo = %v, want nil (open-ended)", tiered.Tiers[1].UpToAmount)
	}
	if tiered.Tiers[1].UnitPrice == nil || tiered.Tiers[1].UnitPrice.Amount != om.Numeric("0.08") {
		t.Errorf("eu-west tier1 rate = %v", tiered.Tiers[1].UnitPrice)
	}

	rcDef := mustUsageRateCard(t, phase, 2)
	if rcDef.Key != "compute_miloapis_com_cpu_default" {
		t.Errorf("default key = %q", rcDef.Key)
	}

	// --- Rate cards (flat fees) ---
	rcRec := mustFlatRateCard(t, phase, 3)
	if rcRec.Key != "recurring_base_fee" {
		t.Errorf("recurring key = %q, want recurring_base_fee", rcRec.Key)
	}
	if rcRec.BillingCadence == nil || *rcRec.BillingCadence != "P1M" {
		t.Errorf("recurring cadence = %v, want P1M", rcRec.BillingCadence)
	}
	if rcRec.Price == nil || rcRec.Price.Amount != om.Numeric("50.00") {
		t.Errorf("recurring amount = %v, want 50.00", rcRec.Price)
	}
	if rcRec.Metadata == nil || (*rcRec.Metadata)["miloapis.com/service-ref"] != "platform.miloapis.com" {
		t.Errorf("recurring metadata = %v", rcRec.Metadata)
	}

	rcOne := mustFlatRateCard(t, phase, 4)
	if rcOne.Key != "one_time_setup" {
		t.Errorf("one-time key = %q, want one_time_setup", rcOne.Key)
	}
	if rcOne.BillingCadence != nil {
		t.Errorf("one-time cadence = %v, want nil (one-time)", rcOne.BillingCadence)
	}
	if rcOne.Metadata == nil || (*rcOne.Metadata)["miloapis.com/trigger"] != "BillingAccountActivation" {
		t.Errorf("one-time trigger metadata = %v", rcOne.Metadata)
	}
}

// TestDesiredPlan_MeterSlugFromMeterName is the focused regression test for
// the meter slug bug: feature keys and rate-card featureKeys must be derived
// from spec.meterName. Covers the name != meterName split that used to pass
// silently in every test because the e2e fixture used identical values.
func TestDesiredPlan_MeterSlugFromMeterName(t *testing.T) {
	r, omc := newOfferReconciler(t, cpuMeter())
	dp := mustDesiredPlan(t, r, cpuOffer())

	// Every usage feature key must carry the meterName-derived slug.
	for _, call := range omc.ensureFeatureCalls {
		if !strings.HasPrefix(call.Key, "compute_miloapis_com_cpu_") {
			t.Errorf("feature key %q must start with meterName-derived slug", call.Key)
		}
		if call.MeterSlug != "compute_miloapis_com_cpu" {
			t.Errorf("MeterSlug = %q, want compute_miloapis_com_cpu", call.MeterSlug)
		}
	}

	// The plan must reference features under the same keys (feature.Key ==
	// rate card key), proving the whole chain is consistent.
	for i := 0; i < 3; i++ {
		rc := mustUsageRateCard(t, dp.Phases[0], i)
		if !strings.HasPrefix(rc.Key, "compute_miloapis_com_cpu_") {
			t.Errorf("rate card %d key %q: meterName slug not used", i, rc.Key)
		}
	}
}

// TestDesiredPlan_FeatureKeySanitization pins the regex-safe encoding of
// dimension values that previously produced invalid feature keys (dots,
// spaces, double hyphens) and got a permanent 400 from OpenMeter.
func TestDesiredPlan_FeatureKeySanitization(t *testing.T) {
	md := cpuMeter()
	offer := baseOffer()
	dims := []string{"region", "model", "tier", "region"}
	vals := []string{"us-east-1", "gpt 4o", "v1.0", "PROD--A"}
	expect := []string{
		"compute_miloapis_com_cpu_region_us_east_1",
		"compute_miloapis_com_cpu_model_gpt_4o",
		"compute_miloapis_com_cpu_tier_v1_0",
		"compute_miloapis_com_cpu_region_prod_a",
	}

	var rates []billingv1alpha1.PricingRate
	for i := range dims {
		rates = append(rates, billingv1alpha1.PricingRate{
			Match: &billingv1alpha1.DimensionMatch{Dimension: dims[i], Value: vals[i]},
			Flat:  "1.00",
		})
	}
	offer.Spec.ServicePricings = []billingv1alpha1.ServicePricingSnapshot{
		{
			Name: "usage-sanitized",
			Spec: billingv1alpha1.ServicePricingSpec{
				ChargeType: billingv1alpha1.ChargeTypeUsage,
				Metric:     "compute.miloapis.com/cpu",
				Rates:      rates,
			},
		},
	}

	r, omc := newOfferReconciler(t, md)
	dp := mustDesiredPlan(t, r, offer)

	if len(omc.ensureFeatureCalls) != len(expect) {
		t.Fatalf("expected %d feature calls, got %d", len(expect), len(omc.ensureFeatureCalls))
	}
	for i, want := range expect {
		if omc.ensureFeatureCalls[i].Key != want {
			t.Errorf("feature %d key = %q, want %q", i, omc.ensureFeatureCalls[i].Key, want)
		}
	}
	_ = dp
	// The rate card count must match (no silent drops).
	if n := len(dp.Phases[0].RateCards); n != len(expect) {
		t.Errorf("rate cards = %d, want %d", n, len(expect))
	}
}

// TestDesiredPlan_MeterNotFound covers the transient blocker when the usage
// pricing references a metric with no MeterDefinition yet.
func TestDesiredPlan_MeterNotFound(t *testing.T) {
	r, _ := newOfferReconciler(t) // no meters

	offer := cpuOffer()
	_, err := r.desiredPlan(context.Background(), offer)
	if err == nil {
		t.Fatal("expected error for missing meter")
	}
	if !strings.Contains(err.Error(), "compute.miloapis.com/cpu") {
		t.Errorf("error should name the missing metric: %v", err)
	}
}

// TestDesiredPlan_UnsupportedChargeType verifies that an unrecognized
// chargeType is surfaced as an error rather than silently producing zero rate
// cards (which OpenMeter rejects as a phase-less plan).
func TestDesiredPlan_UnsupportedChargeType(t *testing.T) {
	r, omc := newOfferReconciler(t, cpuMeter())

	offer := baseOffer()
	offer.Spec.ServicePricings = []billingv1alpha1.ServicePricingSnapshot{
		{
			Name: "bogus-pricing",
			Spec: billingv1alpha1.ServicePricingSpec{
				ChargeType: billingv1alpha1.ChargeType("Bogus"),
			},
		},
	}

	if _, err := r.desiredPlan(context.Background(), offer); err == nil {
		t.Fatal("expected error for unsupported charge type")
	}
	if len(omc.ensureFeatureCalls) != 0 {
		t.Errorf("no features should be created, got %d", len(omc.ensureFeatureCalls))
	}
}

// TestDesiredPlan_NoPricings guards against a plan with zero rate cards: a GA
// offer with no servicePricings must not reach OpenMeter as a phase-less
// plan.
func TestDesiredPlan_NoPricings(t *testing.T) {
	r, omc := newOfferReconciler(t, cpuMeter())

	offer := baseOffer() // empty snapshot
	if _, err := r.desiredPlan(context.Background(), offer); err == nil {
		t.Fatal("expected error for empty servicePricings")
	}
	if len(omc.ensureFeatureCalls) != 0 {
		t.Errorf("no features should be created, got %d", len(omc.ensureFeatureCalls))
	}
}

// TestDesiredPlan_DuplicateRateCardKey verifies a local dup-key detection:
// two rates with the same dimension+value collapse to the same feature key,
// which OpenMeter would reject as rate_card_duplicated_key. The mismatch must
// surface as a mapping error, not a permanent 400 deep in the plan create.
func TestDesiredPlan_DuplicateRateCardKey(t *testing.T) {
	r, _ := newOfferReconciler(t, cpuMeter())

	offer := baseOffer()
	offer.Spec.ServicePricings = []billingv1alpha1.ServicePricingSnapshot{
		{
			Name: "usage-dup",
			Spec: billingv1alpha1.ServicePricingSpec{
				ChargeType: billingv1alpha1.ChargeTypeUsage,
				Metric:     "compute.miloapis.com/cpu",
				Rates: []billingv1alpha1.PricingRate{
					{Match: &billingv1alpha1.DimensionMatch{Dimension: "region", Value: "us-east"}, Flat: "0.10"},
					{Match: &billingv1alpha1.DimensionMatch{Dimension: "region", Value: "us-east"}, Flat: "0.20"},
				},
			},
		},
	}

	if _, err := r.desiredPlan(context.Background(), offer); err == nil {
		t.Fatal("expected duplicate-key error")
	}
}

// TestDesiredPlan_FlatFeeKeyCollidesWithFeature covers the cross-shape
// collision: a flat fee whose name sanitizes to a usage feature key.
func TestDesiredPlan_FlatFeeKeyCollidesWithFeature(t *testing.T) {
	r, _ := newOfferReconciler(t, cpuMeter())

	offer := baseOffer()
	offer.Spec.ServicePricings = []billingv1alpha1.ServicePricingSnapshot{
		{
			Name: "usage-default",
			Spec: billingv1alpha1.ServicePricingSpec{
				ChargeType: billingv1alpha1.ChargeTypeUsage,
				Metric:     "compute.miloapis.com/cpu",
				Rates:      []billingv1alpha1.PricingRate{{Flat: "0.05"}},
			},
		},
		{
			// sanitizeKeyPart("compute.miloapis.com/cpu-default") ==
			// "compute_miloapis_com_cpu_default", the same key the usage
			// default rate produces — forcing a cross-shape collision.
			Name: "compute.miloapis.com/cpu-default",
			Spec: billingv1alpha1.ServicePricingSpec{
				ChargeType: billingv1alpha1.ChargeTypeOneTime,
				Amount:     "10.00",
			},
		},
	}

	if _, err := r.desiredPlan(context.Background(), offer); err == nil {
		t.Fatal("expected duplicate-key error across usage and flat-fee cards")
	}
}

// ---------------------------------------------------------------------------
// Reconcile
// ---------------------------------------------------------------------------

// withFinalizer returns a copy of the offer with the product-plan finalizer
// already attached, simulating a prior reconcile pass. This lets a test
// exercise the sync/delete logic directly instead of duplicating the
// finalizer-add pass.
func withFinalizer(o *billingv1alpha1.Offer) *billingv1alpha1.Offer {
	cp := o.DeepCopy()
	cp.Finalizers = append(cp.Finalizers, ProductPlanFinalizer)
	return cp
}

func requestFor(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
}

// drainRecorder reads all buffered events synchronously. FakeRecorder's
// channel is buffered and Eventf is called synchronously inside Reconcile, so
// everything emitted is already present when Reconcile returns.
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

// TestReconcile_AddsFinalizer verifies the first pass attaches the finalizer
// and returns without touching OpenMeter.
func TestReconcile_AddsFinalizer(t *testing.T) {
	r, omc := newOfferReconciler(t, cpuMeter(), cpuOffer())
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec

	res, err := r.Reconcile(context.Background(), requestFor("test-offer"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("unexpected requeue: %+v", res)
	}
	if len(omc.ensurePlanCalls) != 0 {
		t.Errorf("EnsurePlan called before finalizer added")
	}

	var stored billingv1alpha1.Offer
	if err := r.Get(context.Background(), requestFor("test-offer").NamespacedName, &stored); err != nil {
		t.Fatalf("get offer: %v", err)
	}
	if !slices.Contains(stored.Finalizers, ProductPlanFinalizer) {
		t.Errorf("finalizers = %v, want %q", stored.Finalizers, ProductPlanFinalizer)
	}
}

// TestReconcile_DraftNoSync verifies a Draft offer keeps its finalizer (so a
// later publish can clean up) but never provisions a plan.
func TestReconcile_DraftNoSync(t *testing.T) {
	offer := withFinalizer(cpuOffer())
	offer.Spec.LaunchStage = billingv1alpha1.OfferLaunchStageDraft
	// Draft offers carry no snapshot in practice; assert both that the
	// sync is skipped and that no error occurs.
	offer.Spec.ServicePricings = nil

	r, omc := newOfferReconciler(t, cpuMeter(), offer)
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec

	res, err := r.Reconcile(context.Background(), requestFor("test-offer"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("draft should not requeue: %+v", res)
	}
	if len(omc.ensurePlanCalls) != 0 {
		t.Errorf("EnsurePlan must not be called for a draft offer")
	}

	var stored billingv1alpha1.Offer
	if err := r.Get(context.Background(), requestFor("test-offer").NamespacedName, &stored); err != nil {
		t.Fatalf("get offer: %v", err)
	}
	if !slices.Contains(stored.Finalizers, ProductPlanFinalizer) {
		t.Errorf("draft offer lost its finalizer")
	}
}

// TestReconcile_GASnapshotEmptyRequeues verifies a GA offer with an empty
// servicePricings snapshot requeues transiently (the snapshot fill is
// asynchronous in the billing controller) and records a SyncSkipped event.
func TestReconcile_GASnapshotEmptyRequeues(t *testing.T) {
	// GA, finalizer present, but snapshot empty.
	offer := withFinalizer(baseOffer())

	r, omc := newOfferReconciler(t, cpuMeter(), offer)
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec

	res, err := r.Reconcile(context.Background(), requestFor("test-offer"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != transientRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, transientRequeueAfter)
	}
	if len(omc.ensurePlanCalls) != 0 {
		t.Errorf("EnsurePlan must not be called for an empty snapshot")
	}
	if !containsEvent(drainRecorder(rec), EventReasonSyncSkipped) {
		t.Errorf("expected a %s event", EventReasonSyncSkipped)
	}
}

// TestReconcile_GAConverges is the happy path: a GA offer with a populated
// snapshot provisions the plan and emits a Synced event.
func TestReconcile_GAConverges(t *testing.T) {
	r, omc := newOfferReconciler(t, cpuMeter(), withFinalizer(cpuOffer()))
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec

	res, err := r.Reconcile(context.Background(), requestFor("test-offer"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("unexpected requeue on success: %+v", res)
	}
	if len(omc.ensurePlanCalls) != 1 {
		t.Fatalf("EnsurePlan calls = %d, want 1", len(omc.ensurePlanCalls))
	}
	p := omc.ensurePlanCalls[0]
	if p.Key != "test_uid" {
		t.Errorf("plan key = %q, want test_uid", p.Key)
	}
	if len(p.Phases) != 1 || len(p.Phases[0].RateCards) != 5 {
		t.Errorf("plan phases wrong: %+v", p.Phases)
	}
	if !containsEvent(drainRecorder(rec), EventReasonSynced) {
		t.Errorf("expected a %s event", EventReasonSynced)
	}
}

// TestReconcile_EnsurePlanTransient verifies transient EnsurePlan failures
// requeue on the transient schedule and emit a SyncFailed event.
func TestReconcile_EnsurePlanTransient(t *testing.T) {
	r, omc := newOfferReconciler(t, cpuMeter(), withFinalizer(cpuOffer()))
	omc.ensurePlanFn = func(context.Context, openmeter.DesiredPlan) (om.Plan, error) {
		return om.Plan{}, &openmeter.TransientError{Err: errors.New("boom")}
	}
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec

	res, err := r.Reconcile(context.Background(), requestFor("test-offer"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != transientRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, transientRequeueAfter)
	}
	if !containsEvent(drainRecorder(rec), EventReasonSyncFailed) {
		t.Errorf("expected a SyncFailed event")
	}
}

// TestReconcile_EnsurePlanPermanent verifies permanent EnsurePlan failures
// requeue on the long permanent schedule (never tighter) and still surface
// the failure as an event.
func TestReconcile_EnsurePlanPermanent(t *testing.T) {
	r, omc := newOfferReconciler(t, cpuMeter(), withFinalizer(cpuOffer()))
	omc.ensurePlanFn = func(context.Context, openmeter.DesiredPlan) (om.Plan, error) {
		return om.Plan{}, &openmeter.PermanentError{Err: errors.New("rejected")}
	}
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec

	res, err := r.Reconcile(context.Background(), requestFor("test-offer"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != permanentRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, permanentRequeueAfter)
	}
	if !containsEvent(drainRecorder(rec), syncReasonPermanent) {
		t.Errorf("expected a %s SyncFailed event", syncReasonPermanent)
	}
}

// TestReconcile_DesiredPlanError verifies a mapping error (here an
// unsupported charge type) stops before EnsurePlan.
func TestReconcile_DesiredPlanError(t *testing.T) {
	offer := withFinalizer(baseOffer())
	offer.Spec.ServicePricings = []billingv1alpha1.ServicePricingSnapshot{
		{
			Name: "bogus",
			Spec: billingv1alpha1.ServicePricingSpec{
				ChargeType: billingv1alpha1.ChargeType("Bogus"),
			},
		},
	}

	r, omc := newOfferReconciler(t, cpuMeter(), offer)
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec

	res, err := r.Reconcile(context.Background(), requestFor("test-offer"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != transientRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, transientRequeueAfter)
	}
	if len(omc.ensurePlanCalls) != 0 {
		t.Errorf("EnsurePlan must not run when desiredPlan fails")
	}
	if !containsEvent(drainRecorder(rec), EventReasonSyncFailed) {
		t.Errorf("expected a SyncFailed event")
	}
}

// ---------------------------------------------------------------------------
// Reconcile delete / finalizer
// ---------------------------------------------------------------------------

func deletedOffer() *billingv1alpha1.Offer {
	now := metav1.Now()
	o := withFinalizer(cpuOffer())
	o.DeletionTimestamp = &now
	return o
}

// TestReconcileDelete_PlanNotFound verifies the finalizer is released when
// OpenMeter has no plan for the key (e.g. it was never synced).
func TestReconcileDelete_PlanNotFound(t *testing.T) {
	r, omc := newOfferReconciler(t, cpuMeter(), deletedOffer())
	omc.getPlanByKeyFn = func(context.Context, string) (om.Plan, error) {
		return om.Plan{}, openmeter.ErrPlanNotFound
	}

	res, err := r.Reconcile(context.Background(), requestFor("test-offer"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("unexpected requeue on clean delete: %+v", res)
	}
	if len(omc.deletePlanCalls) != 0 {
		t.Errorf("DeletePlan should not be called when no plan exists")
	}

	// Removing the last finalizer from a deleting object makes the fake
	// client complete the deletion: the Offer must no longer exist.
	var stored billingv1alpha1.Offer
	err = r.Get(context.Background(), requestFor("test-offer").NamespacedName, &stored)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get offer after clean delete: err = %v, want NotFound (finalizer removal must release the object)", err)
	}
}

// TestReconcileDelete_PlanDeleted is the happy deletion path: plan exists,
// DeletePlan succeeds, finalizer is removed.
func TestReconcileDelete_PlanDeleted(t *testing.T) {
	r, omc := newOfferReconciler(t, cpuMeter(), deletedOffer())
	omc.getPlanByKeyFn = func(_ context.Context, key string) (om.Plan, error) {
		return om.Plan{Id: "plan-123", Key: key}, nil
	}
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec

	res, err := r.Reconcile(context.Background(), requestFor("test-offer"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("unexpected requeue on successful delete: %+v", res)
	}
	if len(omc.deletePlanCalls) != 1 || omc.deletePlanCalls[0] != "plan-123" {
		t.Errorf("DeletePlan calls = %v, want [plan-123]", omc.deletePlanCalls)
	}

	// Finalizer removed -> deletion completes -> object gone.
	var stored billingv1alpha1.Offer
	err = r.Get(context.Background(), requestFor("test-offer").NamespacedName, &stored)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get offer after plan deleted: err = %v, want NotFound (finalizer removal must release the object)", err)
	}
	if !containsEvent(drainRecorder(rec), EventReasonDeleted) {
		t.Errorf("expected a Deleted event")
	}
}

// TestReconcileDelete_TransientFailure verifies a wedged OpenMeter blocks
// deletion (keeps the finalizer) and requeues transiently — the safety
// property that prevents orphaning a live plan.
func TestReconcileDelete_TransientFailure(t *testing.T) {
	r, omc := newOfferReconciler(t, cpuMeter(), deletedOffer())
	omc.getPlanByKeyFn = func(context.Context, string) (om.Plan, error) {
		return om.Plan{}, &openmeter.TransientError{Err: errors.New("down")}
	}
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec

	res, err := r.Reconcile(context.Background(), requestFor("test-offer"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != transientRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, transientRequeueAfter)
	}

	var stored billingv1alpha1.Offer
	if err := r.Get(context.Background(), requestFor("test-offer").NamespacedName, &stored); err != nil {
		t.Fatalf("get offer: %v", err)
	}
	if !slices.Contains(stored.Finalizers, ProductPlanFinalizer) {
		t.Errorf("finalizer was released while OpenMeter was unreachable — plan could be orphaned")
	}
}

// TestReconcileDelete_DropFinalizerOnPermanent verifies that a permanent
// OpenMeter failure does not block Kubernetes deletion forever: the finalizer
// is released and the plan is left in place (surfaced via an event), which is
// the documented tradeoff.
func TestReconcileDelete_DropFinalizerOnPermanent(t *testing.T) {
	r, omc := newOfferReconciler(t, cpuMeter(), deletedOffer())
	omc.getPlanByKeyFn = func(_ context.Context, key string) (om.Plan, error) {
		return om.Plan{Id: "plan-123", Key: key}, nil
	}
	omc.deletePlanFn = func(context.Context, string) error {
		return &openmeter.PermanentError{Err: errors.New("cannot delete")}
	}
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec

	res, err := r.Reconcile(context.Background(), requestFor("test-offer"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("unexpected requeue on permanent delete failure: %+v", res)
	}

	// Finalizer released on permanent failure -> deletion completes -> gone.
	var stored billingv1alpha1.Offer
	err = r.Get(context.Background(), requestFor("test-offer").NamespacedName, &stored)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get offer after permanent delete failure: err = %v, want NotFound", err)
	}
	if !containsEvent(drainRecorder(rec), EventReasonDeleteFailed) {
		t.Errorf("expected a DeleteFailed event")
	}
}

// TestSanitizeKeyPart_BuildsValidRegex is a meta-test: every sanitized output
// must match OpenMeter's own key regex, proving the encoding can never feed
// the API an invalid key.
func TestSanitizeKeyPart_BuildsValidRegex(t *testing.T) {
	inputs := []string{
		"us-east-1", "gpt 4o", "v1.0", "PROD--A", "a__b", " a",
		"us-east", "region", "123", "foo.bar/baz-qux", "Mixed Case",
	}
	for _, in := range inputs {
		got := sanitizeKeyPart(in)
		if got == "" {
			continue // all-junk inputs legitimately collapse to empty
		}
		re := regexp.MustCompile(`^[a-z0-9]+(?:_[a-z0-9]+)*$`)
		if !re.MatchString(got) {
			t.Errorf("sanitizeKeyPart(%q) = %q, does not match %s", in, got, re)
		}
	}
}

// ---------------------------------------------------------------------------
// fake OpenMeter client
// ---------------------------------------------------------------------------

// fakeOpenMeterClient records every call and delegates to overridable fns.
// Methods not overridden return a reasonable echo so Reconcile can run.
type fakeOpenMeterClient struct {
	openmeter.Client // embed the interface; unimplemented methods panic if used

	ensureFeatureCalls []openmeter.DesiredFeature
	ensureFeatureFn    func(context.Context, openmeter.DesiredFeature) (om.Feature, error)

	ensurePlanCalls []openmeter.DesiredPlan
	ensurePlanFn    func(context.Context, openmeter.DesiredPlan) (om.Plan, error)

	getPlanByKeyFn func(context.Context, string) (om.Plan, error)

	deletePlanCalls []string
	deletePlanFn    func(context.Context, string) error

	archiveFeaturesCalls []string
	archiveFeaturesFn    func(context.Context, []string) error

	listFeaturesCalls []string
	listFeaturesFn    func(context.Context, *string) ([]om.Feature, error)

	archiveMeterFeaturesCalls []string
	archiveMeterFeaturesFn    func(context.Context, string) error
}

func (f *fakeOpenMeterClient) EnsureFeature(ctx context.Context, d openmeter.DesiredFeature) (om.Feature, error) {
	f.ensureFeatureCalls = append(f.ensureFeatureCalls, d)
	if f.ensureFeatureFn != nil {
		return f.ensureFeatureFn(ctx, d)
	}
	return om.Feature{Key: d.Key}, nil
}

func (f *fakeOpenMeterClient) EnsurePlan(ctx context.Context, d openmeter.DesiredPlan) (om.Plan, error) {
	f.ensurePlanCalls = append(f.ensurePlanCalls, d)
	if f.ensurePlanFn != nil {
		return f.ensurePlanFn(ctx, d)
	}
	return om.Plan{Key: d.Key, Name: d.Name}, nil
}

func (f *fakeOpenMeterClient) GetPlanByKey(ctx context.Context, key string) (om.Plan, error) {
	if f.getPlanByKeyFn != nil {
		return f.getPlanByKeyFn(ctx, key)
	}
	return om.Plan{Key: key}, nil
}

func (f *fakeOpenMeterClient) DeletePlan(ctx context.Context, id string) error {
	f.deletePlanCalls = append(f.deletePlanCalls, id)
	if f.deletePlanFn != nil {
		return f.deletePlanFn(ctx, id)
	}
	return nil
}

func (f *fakeOpenMeterClient) ListFeatures(ctx context.Context, meterSlug *string) ([]om.Feature, error) {
	if meterSlug != nil {
		f.listFeaturesCalls = append(f.listFeaturesCalls, *meterSlug)
	}
	if f.listFeaturesFn != nil {
		return f.listFeaturesFn(ctx, meterSlug)
	}
	return nil, nil
}

func (f *fakeOpenMeterClient) DeleteFeature(ctx context.Context, idOrKey string) error {
	return nil
}

func (f *fakeOpenMeterClient) ArchiveFeaturesIfUnreferenced(ctx context.Context, keys []string) error {
	f.archiveFeaturesCalls = append(f.archiveFeaturesCalls, keys...)
	if f.archiveFeaturesFn != nil {
		return f.archiveFeaturesFn(ctx, keys)
	}
	return nil
}

func (f *fakeOpenMeterClient) ArchiveUnreferencedMeterFeatures(ctx context.Context, meterSlug string) error {
	f.archiveMeterFeaturesCalls = append(f.archiveMeterFeaturesCalls, meterSlug)
	if f.archiveMeterFeaturesFn != nil {
		return f.archiveMeterFeaturesFn(ctx, meterSlug)
	}
	return nil
}
