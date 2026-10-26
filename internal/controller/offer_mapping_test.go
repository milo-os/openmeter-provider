// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	omv3 "github.com/openmeterio/openmeter/api/v3"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

// openMeterKeyRE is OpenMeter's key pattern for features and rate cards.
var openMeterKeyRE = regexp.MustCompile(`^[a-z0-9]+(?:_[a-z0-9]+)*$`)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// baseOffer returns a GA offer with a stable UID and no servicePricings.
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
// break feature creation (the slug was once derived from metadata.name).
func cpuMeter() *billingv1alpha1.MeterDefinition {
	return &billingv1alpha1.MeterDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "cpu-meter"},
		Spec: billingv1alpha1.MeterDefinitionSpec{
			MeterName: "compute.miloapis.com/cpu",
			Measurement: billingv1alpha1.MeterMeasurement{
				Dimensions: []string{"region", "tier"},
			},
		},
	}
}

const cpuSlug = "compute_miloapis_com_cpu"

// cpuMeters is the meter index for cpuMeter.
func cpuMeters() map[string]meterInfo {
	return map[string]meterInfo{
		"compute.miloapis.com/cpu": {
			Name:       "compute.miloapis.com/cpu",
			Slug:       cpuSlug,
			Dimensions: []string{"region", "tier"},
		},
	}
}

func usagePricing(name string, rates ...billingv1alpha1.PricingRate) billingv1alpha1.ServicePricingSnapshot {
	return billingv1alpha1.ServicePricingSnapshot{
		Name: name,
		Spec: billingv1alpha1.ServicePricingSpec{
			DisplayName: "Compute Usage",
			ChargeType:  billingv1alpha1.ChargeTypeUsage,
			Metric:      "compute.miloapis.com/cpu",
			PricingUnit: "vcpu",
			ServiceRef:  "compute.miloapis.com",
			Rates:       rates,
		},
	}
}

func matchRate(dim, value, flat string) billingv1alpha1.PricingRate {
	return billingv1alpha1.PricingRate{
		Match: &billingv1alpha1.DimensionMatch{Dimension: dim, Value: value},
		Flat:  flat,
	}
}

// cpuOffer returns a fully-populated GA offer covering every pricing shape:
// usage with two region-matched rates and a catch-all, a recurring flat fee,
// and a one-time flat fee.
func cpuOffer() *billingv1alpha1.Offer {
	o := baseOffer()
	o.Spec.ServicePricings = []billingv1alpha1.ServicePricingSnapshot{
		usagePricing("usage-item-multi-region",
			matchRate("region", "us-east", "0.10"),
			billingv1alpha1.PricingRate{
				Match: &billingv1alpha1.DimensionMatch{Dimension: "region", Value: "eu-west"},
				Tiered: []billingv1alpha1.PricingTierBand{
					{Rate: "0.12", UpTo: "100"},
					{Rate: "0.08"},
				},
			},
			billingv1alpha1.PricingRate{Flat: "0.05"},
		),
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

func mustMapping(t *testing.T, offer *billingv1alpha1.Offer) offerMapping {
	t.Helper()
	m, err := buildOfferMapping(offer, cpuMeters())
	if err != nil {
		t.Fatalf("buildOfferMapping: %v", err)
	}
	if len(m.Plan.Phases) != 1 {
		t.Fatalf("expected 1 phase, got %d", len(m.Plan.Phases))
	}
	return m
}

// mustUsageRateCard returns rate card i, which must reference a feature.
func mustUsageRateCard(t *testing.T, phase omv3.BillingPlanPhase, i int) omv3.BillingRateCard {
	t.Helper()
	rc := phase.RateCards[i]
	if d, _ := rc.Price.Discriminator(); d == string(omv3.BillingPriceFlatTypeFlat) {
		t.Fatalf("rate card %d: flat price, want a usage price", i)
	}
	return rc
}

// mustFlatRateCard returns rate card i, which must have a flat price.
func mustFlatRateCard(t *testing.T, phase omv3.BillingPlanPhase, i int) (omv3.BillingRateCard, omv3.BillingPriceFlat) {
	t.Helper()
	rc := phase.RateCards[i]
	if d, _ := rc.Price.Discriminator(); d != string(omv3.BillingPriceFlatTypeFlat) {
		t.Fatalf("rate card %d: price type %q, want flat", i, d)
	}
	flat, err := rc.Price.AsBillingPriceFlat()
	if err != nil {
		t.Fatalf("rate card %d: %v", i, err)
	}
	return rc, flat
}

func featureByKey(t *testing.T, m offerMapping, key string) openmeter.DesiredFeature {
	t.Helper()
	for _, f := range m.Features {
		if f.Key == key {
			return f
		}
	}
	t.Fatalf("no feature with key %q in mapping", key)
	return openmeter.DesiredFeature{}
}

// ---------------------------------------------------------------------------
// Key encoding
// ---------------------------------------------------------------------------

func TestSanitizeKeyPart(t *testing.T) {
	tests := []struct{ in, want string }{
		{"region", "region"},
		{"us-east", "us_east"},
		{"v1.0", "v1_0"},
		{"us--east", "us_east"},
		{"US-East", "us_east"},
		{"new york", "new_york"},
		{"a/b", "a_b"},
		{" us ", "us"},
		{"_us", "us"},
		{"gpt-4o", "gpt_4o"},
		{"---", ""},
		{"a_b", "a_b"},
	}
	for _, tt := range tests {
		got := sanitizeKeyPart(tt.in)
		if got != tt.want {
			t.Errorf("sanitizeKeyPart(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if got != "" && !openMeterKeyRE.MatchString(got) {
			t.Errorf("sanitizeKeyPart(%q) = %q is not a valid key", tt.in, got)
		}
	}
}

func TestBoundedKey(t *testing.T) {
	long := strings.Repeat("abc_", 30)
	tests := []struct {
		name, readable, fallback, identity string
	}{
		{"short", "recurring_fee", "flat_fee", "recurring-fee"},
		{"long is truncated", long, "flat_fee", long},
		{"truncation lands on underscore", strings.Repeat("a", 53) + "_b", "x", "id"},
		{"empty uses fallback", "", "flat_fee", "---"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := boundedKey(tt.readable, tt.fallback, tt.identity)
			if len(got) > openMeterKeyMaxLen {
				t.Errorf("len(%q) = %d > %d", got, len(got), openMeterKeyMaxLen)
			}
			if !openMeterKeyRE.MatchString(got) {
				t.Errorf("%q is not a valid OpenMeter key", got)
			}
			if again := boundedKey(tt.readable, tt.fallback, tt.identity); again != got {
				t.Errorf("not deterministic: %q vs %q", got, again)
			}
		})
	}
	if boundedKey("x", "f", "a") == boundedKey("x", "f", "b") {
		t.Error("different identities produced the same key")
	}
}

// ---------------------------------------------------------------------------
// Mapping
// ---------------------------------------------------------------------------

func TestBuildOfferMapping_Plan(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantName    string
	}{
		{"annotation present", map[string]string{billingv1alpha1.DisplayNameAnnotation: "Custom Display Name"}, "Custom Display Name"},
		{"no annotation", nil, "test-offer"},
		{"empty annotation", map[string]string{billingv1alpha1.DisplayNameAnnotation: ""}, "test-offer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			offer := cpuOffer()
			offer.Annotations = tt.annotations
			plan := mustMapping(t, offer).Plan
			if plan.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", plan.Name, tt.wantName)
			}
			if plan.Key != "test_uid" {
				t.Errorf("Key = %q, want test_uid", plan.Key)
			}
			if plan.Currency != "USD" {
				t.Errorf("Currency = %q", plan.Currency)
			}
			if plan.Phases[0].Key != planPhaseKey {
				t.Errorf("phase key = %q", plan.Phases[0].Key)
			}
		})
	}
}

// TestBuildOfferMapping_Exhaustive drives the full cpuOffer and asserts every
// OpenMeter artifact: features, filters, rate card types, prices, cadences,
// and descriptions.
func TestBuildOfferMapping_Exhaustive(t *testing.T) {
	m := mustMapping(t, cpuOffer())
	phase := m.Plan.Phases[0]
	if len(phase.RateCards) != 5 {
		t.Fatalf("rate cards = %d, want 5", len(phase.RateCards))
	}
	if len(m.Features) != 3 {
		t.Fatalf("features = %d, want 3", len(m.Features))
	}
	if !slices.Equal(m.MeterSlugs, []string{cpuSlug}) {
		t.Errorf("MeterSlugs = %v, want [%s]", m.MeterSlugs, cpuSlug)
	}

	// Every usage rate card is keyed by, and points at, its feature; every
	// feature is bound to the slug derived from spec.meterName.
	for i := 0; i < 3; i++ {
		rc := mustUsageRateCard(t, phase, i)
		if m.CardFeatures[rc.Key] != rc.Key {
			t.Errorf("rate card %d feature %q != key %q", i, m.CardFeatures[rc.Key], rc.Key)
		}
		f := featureByKey(t, m, rc.Key)
		if f.MeterSlug != cpuSlug {
			t.Errorf("feature %q meter = %q, want %q", f.Key, f.MeterSlug, cpuSlug)
		}
		if !strings.HasPrefix(f.Key, cpuSlug+"_") || !openMeterKeyRE.MatchString(f.Key) || len(f.Key) > 64 {
			t.Errorf("feature key %q invalid", f.Key)
		}
		if rc.BillingCadence == nil || *rc.BillingCadence != "P1M" {
			t.Errorf("rate card %d cadence = %v", i, rc.BillingCadence)
		}
		want := "ServicePricing usage-item-multi-region (compute.miloapis.com), priced per vcpu"
		if rc.Description == nil || *rc.Description != want {
			t.Errorf("rate card %d description = %v, want %q", i, rc.Description, want)
		}
		if rc.UnitConfig != nil {
			t.Errorf("rate card %d converts usage without a unitConversion", i)
		}
	}

	// us-east: $eq filter on the RAW value, flat unit price.
	us := mustUsageRateCard(t, phase, 0)
	usF := featureByKey(t, m, us.Key)
	if eq := usF.AdvancedMeterGroupByFilters["region"].Eq; eq == nil || *eq != "us-east" {
		t.Errorf("us-east filter = %+v", usF.AdvancedMeterGroupByFilters)
	}
	if us.Name != "Compute Usage (region=us-east)" {
		t.Errorf("us-east name = %q", us.Name)
	}
	if d, _ := us.Price.Discriminator(); d != string(omv3.BillingPriceUnitTypeUnit) {
		t.Errorf("us-east price type = %q, want unit", d)
	}
	if unit, err := us.Price.AsBillingPriceUnit(); err != nil || unit.Amount != "0.10" {
		t.Errorf("us-east price = %+v (%v)", unit, err)
	}

	// eu-west: graduated tiers, last band open-ended.
	eu := mustUsageRateCard(t, phase, 1)
	if d, _ := eu.Price.Discriminator(); d != string(omv3.BillingPriceGraduatedTypeGraduated) {
		t.Fatalf("eu-west price type = %q, want graduated", d)
	}
	tiered, err := eu.Price.AsBillingPriceGraduated()
	if err != nil || len(tiered.Tiers) != 2 {
		t.Fatalf("eu-west price = %+v (%v)", tiered, err)
	}
	if tiered.Tiers[0].UpToAmount == nil || *tiered.Tiers[0].UpToAmount != "100" ||
		tiered.Tiers[0].UnitPrice.Amount != "0.12" {
		t.Errorf("eu-west tier0 = %+v", tiered.Tiers[0])
	}
	if tiered.Tiers[1].UpToAmount != nil || tiered.Tiers[1].UnitPrice.Amount != "0.08" {
		t.Errorf("eu-west tier1 = %+v", tiered.Tiers[1])
	}

	// Catch-all: excludes every matched value so matched usage is not
	// billed twice.
	def := mustUsageRateCard(t, phase, 2)
	defF := featureByKey(t, m, def.Key)
	nin := defF.AdvancedMeterGroupByFilters["region"].Nin
	if nin == nil || !slices.Equal(*nin, []string{"eu-west", "us-east"}) {
		t.Errorf("catch-all filter = %+v, want region $nin [eu-west us-east]", defF.AdvancedMeterGroupByFilters)
	}
	if def.Name != "Compute Usage (region not in (eu-west, us-east))" {
		t.Errorf("catch-all name = %q", def.Name)
	}

	// Recurring flat fee: monthly cadence, charged in advance as v1 did.
	rec, recPrice := mustFlatRateCard(t, phase, 3)
	if !strings.HasPrefix(rec.Key, "recurring_base_fee_") || !openMeterKeyRE.MatchString(rec.Key) {
		t.Errorf("recurring key = %q", rec.Key)
	}
	if rec.BillingCadence == nil || *rec.BillingCadence != "P1M" {
		t.Errorf("recurring cadence = %v", rec.BillingCadence)
	}
	if recPrice.Amount != "50.00" {
		t.Errorf("recurring price = %+v", recPrice)
	}
	if rec.PaymentTerm == nil || *rec.PaymentTerm != omv3.BillingPricePaymentTermInAdvance {
		t.Errorf("recurring payment term = %v, want in_advance", rec.PaymentTerm)
	}
	if rec.Description == nil || *rec.Description != "ServicePricing recurring-base-fee (platform.miloapis.com)" {
		t.Errorf("recurring description = %v", rec.Description)
	}
	if _, ok := m.CardFeatures[rec.Key]; ok || rec.Feature != nil {
		t.Errorf("flat fee must not reference a feature")
	}

	// One-time flat fee: no cadence (charged once), trigger recorded.
	one, _ := mustFlatRateCard(t, phase, 4)
	if !strings.HasPrefix(one.Key, "one_time_setup_") {
		t.Errorf("one-time key = %q", one.Key)
	}
	if one.BillingCadence != nil {
		t.Errorf("one-time cadence = %v, want nil", one.BillingCadence)
	}
	if one.Description == nil || !strings.HasSuffix(*one.Description, ", charged on BillingAccountActivation") {
		t.Errorf("one-time description = %v", one.Description)
	}
}

// TestBuildOfferMapping_UnitConversion: a pricing whose meter measures in a
// smaller unit than it is priced in converts the usage, so the rates and tier
// bounds stay as the Offer states them, in the pricing unit.
func TestBuildOfferMapping_UnitConversion(t *testing.T) {
	offer := baseOffer()
	sp := usagePricing("memory", billingv1alpha1.PricingRate{Tiered: []billingv1alpha1.PricingTierBand{
		{Rate: "0", UpTo: "200"}, {Rate: "0.05"},
	}})
	sp.Spec.PricingUnit = "GiB"
	sp.Spec.UnitConversion = &billingv1alpha1.UnitConversion{
		MeterUnitsPerPricingUnit: "1073741824",
		Description:              "Metered in bytes, priced per GiB",
	}
	offer.Spec.ServicePricings = []billingv1alpha1.ServicePricingSnapshot{sp}

	rc := mustUsageRateCard(t, mustMapping(t, offer).Plan.Phases[0], 0)
	uc := rc.UnitConfig
	if uc == nil || uc.Operation != omv3.BillingUnitConfigOperationDivide || uc.ConversionFactor != "1073741824" {
		t.Fatalf("unit config = %+v, want divide by 1073741824", uc)
	}
	if uc.DisplayUnit == nil || *uc.DisplayUnit != "GiB" {
		t.Errorf("display unit = %v, want the pricing unit", uc.DisplayUnit)
	}
	tiered, err := rc.Price.AsBillingPriceGraduated()
	if err != nil || *tiered.Tiers[0].UpToAmount != "200" || tiered.Tiers[1].UnitPrice.Amount != "0.05" {
		t.Errorf("price = %+v (%v), want the Offer's tiers unchanged", tiered, err)
	}
}

func TestBuildOfferMapping_SingleUnmatchedRateBillsAllUsage(t *testing.T) {
	offer := baseOffer()
	offer.Spec.ServicePricings = []billingv1alpha1.ServicePricingSnapshot{
		usagePricing("usage", billingv1alpha1.PricingRate{Flat: "0.05"}),
	}
	m := mustMapping(t, offer)
	if len(m.Features) != 1 {
		t.Fatalf("features = %d, want 1", len(m.Features))
	}
	f := m.Features[0]
	if len(f.AdvancedMeterGroupByFilters) != 0 {
		t.Errorf("unmatched-only rate must not filter, got %+v", f.AdvancedMeterGroupByFilters)
	}
	if f.Name != "compute.miloapis.com/cpu" {
		t.Errorf("feature name = %q", f.Name)
	}
}

// TestBuildOfferMapping_MatchedOnlyHasNoCatchAll: without an unmatched rate,
// unmatched usage is simply not billed.
func TestBuildOfferMapping_MatchedOnlyHasNoCatchAll(t *testing.T) {
	offer := baseOffer()
	offer.Spec.ServicePricings = []billingv1alpha1.ServicePricingSnapshot{
		usagePricing("usage", matchRate("tier", "premium", "1"), matchRate("tier", "standard", "0.5")),
	}
	m := mustMapping(t, offer)
	for _, f := range m.Features {
		if f.AdvancedMeterGroupByFilters["tier"].Eq == nil {
			t.Errorf("feature %q has no $eq filter: %+v", f.Key, f.AdvancedMeterGroupByFilters)
		}
	}
}

// TestBuildOfferMapping_FeatureKeysEncodeDefinition: features are shared
// across Offers and immutable, so equal definitions must share a key and
// different definitions must never collide.
func TestBuildOfferMapping_FeatureKeysEncodeDefinition(t *testing.T) {
	keysFor := func(rates ...billingv1alpha1.PricingRate) []string {
		offer := baseOffer()
		offer.Spec.ServicePricings = []billingv1alpha1.ServicePricingSnapshot{usagePricing("usage", rates...)}
		m := mustMapping(t, offer)
		var keys []string
		for _, f := range m.Features {
			keys = append(keys, f.Key)
		}
		return keys
	}
	catchAll := billingv1alpha1.PricingRate{Flat: "0.01"}

	a := keysFor(matchRate("region", "us-east", "1"))
	b := keysFor(matchRate("region", "us.east", "1"))
	if a[0] == b[0] {
		t.Errorf("us-east and us.east share feature key %q; one Offer would bill the other's filter", a[0])
	}

	same := keysFor(matchRate("region", "us-east", "2"))
	if a[0] != same[0] {
		t.Errorf("same definition produced different keys %q / %q", a[0], same[0])
	}

	// Two Offers whose catch-alls exclude different value sets need
	// different features.
	c1 := keysFor(matchRate("region", "us-east", "1"), catchAll)
	c2 := keysFor(matchRate("region", "us-east", "1"), matchRate("region", "eu-west", "1"), catchAll)
	if c1[1] == c2[2] {
		t.Errorf("catch-alls with different exclusions share key %q", c1[1])
	}
	// Rate order does not change the catch-all definition.
	c3 := keysFor(matchRate("region", "eu-west", "1"), matchRate("region", "us-east", "1"), catchAll)
	if c2[2] != c3[2] {
		t.Errorf("catch-all key depends on rate order: %q vs %q", c2[2], c3[2])
	}
}

func TestBuildOfferMapping_LongNamesStayValid(t *testing.T) {
	meters := map[string]meterInfo{
		"very.long.service.miloapis.com/some/deeply/nested/metric-name-with-many-words": {
			Name:       "very.long.service.miloapis.com/some/deeply/nested/metric-name-with-many-words",
			Slug:       openmeter.MeterSlug("very.long.service.miloapis.com/some/deeply/nested/metric-name-with-many-words"),
			Dimensions: []string{"a-rather-long-dimension-name"},
		},
	}
	offer := baseOffer()
	sp := usagePricing(strings.Repeat("long-pricing-name-", 10),
		matchRate("a-rather-long-dimension-name", strings.Repeat("value-", 20), "1"),
		billingv1alpha1.PricingRate{Flat: "0.5"})
	sp.Spec.Metric = "very.long.service.miloapis.com/some/deeply/nested/metric-name-with-many-words"
	offer.Spec.ServicePricings = []billingv1alpha1.ServicePricingSnapshot{
		sp,
		{Name: strings.Repeat("fee-", 40), Spec: billingv1alpha1.ServicePricingSpec{
			ChargeType: billingv1alpha1.ChargeTypeOneTime, Amount: "1",
		}},
	}
	m, err := buildOfferMapping(offer, meters)
	if err != nil {
		t.Fatalf("buildOfferMapping: %v", err)
	}
	for _, f := range m.Features {
		if len(f.Key) > openMeterKeyMaxLen || !openMeterKeyRE.MatchString(f.Key) {
			t.Errorf("feature key %q (len %d) invalid", f.Key, len(f.Key))
		}
	}
	flat, _ := mustFlatRateCard(t, m.Plan.Phases[0], 2)
	if len(flat.Key) > openMeterKeyMaxLen || !openMeterKeyRE.MatchString(flat.Key) {
		t.Errorf("flat fee key %q (len %d) invalid", flat.Key, len(flat.Key))
	}
}

// TestBuildOfferMapping_FlatFeeNamesDoNotCollide: pricing names that
// sanitize identically still get distinct rate card keys.
func TestBuildOfferMapping_FlatFeeNamesDoNotCollide(t *testing.T) {
	offer := baseOffer()
	for _, name := range []string{"setup-fee", "setup.fee"} {
		offer.Spec.ServicePricings = append(offer.Spec.ServicePricings, billingv1alpha1.ServicePricingSnapshot{
			Name: name,
			Spec: billingv1alpha1.ServicePricingSpec{ChargeType: billingv1alpha1.ChargeTypeOneTime, Amount: "1"},
		})
	}
	m := mustMapping(t, offer)
	a, _ := mustFlatRateCard(t, m.Plan.Phases[0], 0)
	b, _ := mustFlatRateCard(t, m.Plan.Phases[0], 1)
	if a.Key == b.Key {
		t.Fatalf("both flat fees keyed %q", a.Key)
	}
}

func TestBuildOfferMapping_ProjectNameDimensionAllowed(t *testing.T) {
	offer := baseOffer()
	offer.Spec.ServicePricings = []billingv1alpha1.ServicePricingSnapshot{
		usagePricing("usage", matchRate(billingv1alpha1.SystemDimensionProjectName, "p1", "1")),
	}
	mustMapping(t, offer)
}

// TestBuildOfferMapping_FilterKeysUseMeterGroupByKey: feature filters must
// be keyed by the same sanitized group-by key the MeterDefinition
// controller gives the meter (openmeter.GroupByKey), or OpenMeter rejects
// the feature for filtering on an unknown group-by.
func TestBuildOfferMapping_FilterKeysUseMeterGroupByKey(t *testing.T) {
	meters := map[string]meterInfo{
		"compute.miloapis.com/cpu": {
			Name:       "compute.miloapis.com/cpu",
			Slug:       cpuSlug,
			Dimensions: []string{"resource.type"},
		},
	}
	offer := baseOffer()
	offer.Spec.ServicePricings = []billingv1alpha1.ServicePricingSnapshot{
		usagePricing("usage",
			matchRate("resource.type", "vm", "1"),
			billingv1alpha1.PricingRate{Flat: "2"},
		),
	}
	m, err := buildOfferMapping(offer, meters)
	if err != nil {
		t.Fatalf("buildOfferMapping: %v", err)
	}
	if len(m.Features) != 2 {
		t.Fatalf("features = %d, want 2", len(m.Features))
	}
	for _, f := range m.Features {
		if _, ok := f.AdvancedMeterGroupByFilters["resource_type"]; !ok {
			t.Errorf("feature %q filters = %v, want keyed by %q", f.Key, f.AdvancedMeterGroupByFilters, "resource_type")
		}
		if _, raw := f.AdvancedMeterGroupByFilters["resource.type"]; raw {
			t.Errorf("feature %q filters keyed by the raw dimension name", f.Key)
		}
	}
}

func TestBuildOfferMapping_Deterministic(t *testing.T) {
	h1, err := mustMapping(t, cpuOffer()).Plan.SpecHash()
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := mustMapping(t, cpuOffer()).Plan.SpecHash()
	if h1 != h2 {
		t.Fatalf("same Offer produced different plan hashes; every reconcile would publish a new version")
	}
}

func TestBuildOfferMapping_MeterNotFound(t *testing.T) {
	_, err := buildOfferMapping(cpuOffer(), map[string]meterInfo{})
	if !errors.Is(err, errMeterNotFound) {
		t.Fatalf("err = %v, want errMeterNotFound", err)
	}
	if !strings.Contains(err.Error(), "compute.miloapis.com/cpu") {
		t.Errorf("error should name the metric: %v", err)
	}
}

// TestBuildOfferMapping_RejectsUnrepresentable covers every snapshot that
// would either double-bill or be rejected by OpenMeter.
func TestBuildOfferMapping_RejectsUnrepresentable(t *testing.T) {
	oneTime := func(name, amount string) billingv1alpha1.ServicePricingSnapshot {
		return billingv1alpha1.ServicePricingSnapshot{Name: name, Spec: billingv1alpha1.ServicePricingSpec{
			ChargeType: billingv1alpha1.ChargeTypeOneTime, Amount: amount,
		}}
	}
	tests := []struct {
		name     string
		pricings []billingv1alpha1.ServicePricingSnapshot
		wantMsg  string
	}{
		{"empty snapshot", nil, "no servicePricings"},
		{"mixed match dimensions",
			[]billingv1alpha1.ServicePricingSnapshot{usagePricing("u",
				matchRate("region", "us-east", "1"), matchRate("tier", "premium", "2"))},
			"share one dimension"},
		{"duplicate match value",
			[]billingv1alpha1.ServicePricingSnapshot{usagePricing("u",
				matchRate("region", "us-east", "1"), matchRate("region", "us-east", "2"))},
			"more than one rate"},
		{"two catch-alls",
			[]billingv1alpha1.ServicePricingSnapshot{usagePricing("u",
				billingv1alpha1.PricingRate{Flat: "1"}, billingv1alpha1.PricingRate{Flat: "2"})},
			"only one catch-all"},
		{"undeclared dimension",
			[]billingv1alpha1.ServicePricingSnapshot{usagePricing("u", matchRate("zone", "a", "1"))},
			"not declared on meter"},
		{"metric priced twice",
			[]billingv1alpha1.ServicePricingSnapshot{
				usagePricing("u1", billingv1alpha1.PricingRate{Flat: "1"}),
				usagePricing("u2", billingv1alpha1.PricingRate{Flat: "2"}),
			},
			"billed twice"},
		{"no rates", []billingv1alpha1.ServicePricingSnapshot{usagePricing("u")}, "no rates"},
		{"rate without price",
			[]billingv1alpha1.ServicePricingSnapshot{usagePricing("u", billingv1alpha1.PricingRate{})},
			"flat or tiered"},
		{"open tier before last",
			[]billingv1alpha1.ServicePricingSnapshot{usagePricing("u", billingv1alpha1.PricingRate{
				Tiered: []billingv1alpha1.PricingTierBand{{Rate: "1"}, {Rate: "2", UpTo: "10"}},
			})},
			"last tier"},
		{"flat fee without amount", []billingv1alpha1.ServicePricingSnapshot{oneTime("fee", "")}, "no amount"},
		{"non-positive unit conversion",
			[]billingv1alpha1.ServicePricingSnapshot{func() billingv1alpha1.ServicePricingSnapshot {
				sp := usagePricing("u", billingv1alpha1.PricingRate{Flat: "1"})
				sp.Spec.UnitConversion = &billingv1alpha1.UnitConversion{MeterUnitsPerPricingUnit: "0", Description: "x"}
				return sp
			}()},
			"not a positive decimal"},
		{"unsupported charge type",
			[]billingv1alpha1.ServicePricingSnapshot{{Name: "x", Spec: billingv1alpha1.ServicePricingSpec{
				ChargeType: billingv1alpha1.ChargeType("Bogus"),
			}}},
			"unsupported charge type"},
		{"service pricing discount negative",
			[]billingv1alpha1.ServicePricingSnapshot{func() billingv1alpha1.ServicePricingSnapshot {
				sp := usagePricing("u", billingv1alpha1.PricingRate{Flat: "1"})
				sp.Discount = &billingv1alpha1.Discount{Percentage: -5}
				return sp
			}()},
			"must be between 0 and 100"},
		{"service pricing discount over 100",
			[]billingv1alpha1.ServicePricingSnapshot{func() billingv1alpha1.ServicePricingSnapshot {
				sp := usagePricing("u", billingv1alpha1.PricingRate{Flat: "1"})
				sp.Discount = &billingv1alpha1.Discount{Percentage: 105}
				return sp
			}()},
			"must be between 0 and 100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			offer := baseOffer()
			offer.Spec.ServicePricings = tt.pricings
			_, err := buildOfferMapping(offer, cpuMeters())
			var invalid *invalidOfferError
			if !errors.As(err, &invalid) {
				t.Fatalf("err = %v, want *invalidOfferError", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("err = %q, want it to mention %q", err, tt.wantMsg)
			}
		})
	}
}

func TestBuildOfferMapping_Discounts(t *testing.T) {
	t.Run("snapshot discount 15 percent", func(t *testing.T) {
		offer := cpuOffer()
		for i := range offer.Spec.ServicePricings {
			offer.Spec.ServicePricings[i].Discount = &billingv1alpha1.Discount{Percentage: 15}
		}
		m := mustMapping(t, offer)
		if len(m.Plan.Phases[0].RateCards) == 0 {
			t.Fatal("mapping has no rate cards to check")
		}
		for i, rc := range m.Plan.Phases[0].RateCards {
			if rc.Discounts == nil || rc.Discounts.Percentage == nil {
				t.Fatalf("rate card %d (%s) missing discount", i, rc.Key)
			}
			if *rc.Discounts.Percentage != 15 {
				t.Errorf("rate card %d (%s) discount = %v, want 15", i, rc.Key, *rc.Discounts.Percentage)
			}
		}
	})

	t.Run("snapshot discount 100 percent", func(t *testing.T) {
		offer := cpuOffer()
		for i := range offer.Spec.ServicePricings {
			offer.Spec.ServicePricings[i].Discount = &billingv1alpha1.Discount{Percentage: 100}
		}
		m := mustMapping(t, offer)
		if len(m.Plan.Phases[0].RateCards) == 0 {
			t.Fatal("mapping has no rate cards to check")
		}
		for i, rc := range m.Plan.Phases[0].RateCards {
			if rc.Discounts == nil || rc.Discounts.Percentage == nil {
				t.Fatalf("rate card %d (%s) missing discount", i, rc.Key)
			}
			if *rc.Discounts.Percentage != 100 {
				t.Errorf("rate card %d (%s) discount = %v, want 100", i, rc.Key, *rc.Discounts.Percentage)
			}
		}
	})

	t.Run("snapshot discount 0 percent means no discount", func(t *testing.T) {
		offer := cpuOffer()
		for i := range offer.Spec.ServicePricings {
			offer.Spec.ServicePricings[i].Discount = &billingv1alpha1.Discount{Percentage: 0}
		}
		m := mustMapping(t, offer)
		if len(m.Plan.Phases[0].RateCards) == 0 {
			t.Fatal("mapping has no rate cards to check")
		}
		for i, rc := range m.Plan.Phases[0].RateCards {
			if rc.Discounts != nil {
				t.Errorf("rate card %d (%s) has unexpected discount: %+v", i, rc.Key, rc.Discounts)
			}
		}
	})
}

func TestBuildOfferMapping_DiscountOverrides(t *testing.T) {
	meters := map[string]meterInfo{
		"compute.miloapis.com/cpu": {
			Name:       "compute.miloapis.com/cpu",
			Slug:       cpuSlug,
			Dimensions: []string{"region", "tier"},
		},
		"storage.miloapis.com/bytes": {
			Name: "storage.miloapis.com/bytes",
			Slug: "storage_miloapis_com_bytes",
		},
	}

	offer := baseOffer()

	// 1. compute usage: snapshot discount 15%
	spCompute := usagePricing("compute-usage", billingv1alpha1.PricingRate{Flat: "0.10"})
	spCompute.Discount = &billingv1alpha1.Discount{Percentage: 15}

	// 2. storage usage: snapshot discount 100%
	spStorage := billingv1alpha1.ServicePricingSnapshot{
		Name: "storage-usage",
		Spec: billingv1alpha1.ServicePricingSpec{
			DisplayName: "Storage Usage",
			ChargeType:  billingv1alpha1.ChargeTypeUsage,
			Metric:      "storage.miloapis.com/bytes",
			PricingUnit: "gib",
			Rates:       []billingv1alpha1.PricingRate{{Flat: "0.05"}},
		},
		Discount: &billingv1alpha1.Discount{Percentage: 100},
	}

	// 3. recurring fee: snapshot discount 0% (no discount)
	spRecurring := billingv1alpha1.ServicePricingSnapshot{
		Name: "base-fee",
		Spec: billingv1alpha1.ServicePricingSpec{
			DisplayName: "Base Fee",
			ChargeType:  billingv1alpha1.ChargeTypeRecurring,
			Amount:      "10.00",
			Interval:    billingv1alpha1.ChargeIntervalMonthly,
		},
		Discount: &billingv1alpha1.Discount{Percentage: 0},
	}

	// 4. one-time fee: nil discount (no discount)
	spOneTime := billingv1alpha1.ServicePricingSnapshot{
		Name: "setup-fee",
		Spec: billingv1alpha1.ServicePricingSpec{
			DisplayName: "Setup Fee",
			ChargeType:  billingv1alpha1.ChargeTypeOneTime,
			Amount:      "50.00",
			Trigger:     billingv1alpha1.ChargeTriggerBillingAccountActivation,
		},
	}

	offer.Spec.ServicePricings = []billingv1alpha1.ServicePricingSnapshot{spCompute, spStorage, spRecurring, spOneTime}

	m, err := buildOfferMapping(offer, meters)
	if err != nil {
		t.Fatalf("buildOfferMapping: %v", err)
	}

	// Check each rate card by Name
	byName := make(map[string]omv3.BillingRateCard)
	for _, rc := range m.Plan.Phases[0].RateCards {
		byName[rc.Name] = rc
	}

	computeCard, ok := byName["Compute Usage"]
	if !ok || computeCard.Discounts == nil || computeCard.Discounts.Percentage == nil || *computeCard.Discounts.Percentage != 15 {
		t.Errorf("compute card discount = %+v (ok=%v), want 15%%", computeCard.Discounts, ok)
	}

	storageCard, ok := byName["Storage Usage"]
	if !ok || storageCard.Discounts == nil || storageCard.Discounts.Percentage == nil || *storageCard.Discounts.Percentage != 100 {
		t.Errorf("storage card discount = %+v (ok=%v), want 100%%", storageCard.Discounts, ok)
	}

	recurringCard, ok := byName["Base Fee"]
	if !ok || recurringCard.Discounts != nil {
		t.Errorf("base fee card discount = %+v (ok=%v), want nil (0%% discount)", recurringCard.Discounts, ok)
	}

	oneTimeCard, ok := byName["Setup Fee"]
	if !ok || oneTimeCard.Discounts != nil {
		t.Errorf("setup fee card discount = %+v (ok=%v), want nil", oneTimeCard.Discounts, ok)
	}
}
