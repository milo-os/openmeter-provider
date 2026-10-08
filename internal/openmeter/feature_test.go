// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// featureFixture returns a Feature bound to the given meter, keyed by key.
// idOrKey is deterministic so tests can assert on it, and the fake stores
// each feature under both its key and its id (mirroring OpenMeter's
// /features/{idOrKey} union).
func featureFixture(key, meterSlug string) om.Feature {
	return om.Feature{
		Id:        "feat-" + key,
		Key:       key,
		Name:      key,
		MeterSlug: &meterSlug,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
}

// seedFeature registers a feature in the fake under both its key and its id,
// matching how the fake's create handler stores features.
func seedFeature(f *fakeServer, feat om.Feature) {
	f.features[feat.Key] = feat
	f.featureByID[feat.Id] = feat.Key
}

// usageRateCard builds an om.RateCard union that references featureKey, using
// the same FromRateCardUsageBased conversion offered_controller.go uses to
// assemble plan rate cards.
func usageRateCard(key, featureKey string) om.RateCard {
	var card om.RateCard
	if err := card.FromRateCardUsageBased(om.RateCardUsageBased{
		Type:       om.RateCardUsageBasedTypeUsageBased,
		Key:        key,
		Name:       key,
		FeatureKey: &featureKey,
	}); err != nil {
		panic(err)
	}
	return card
}

// planFixture returns a Plan with an id/key and the given usage rate cards.
func planFixture(id, key string, cards ...om.RateCard) om.Plan {
	return om.Plan{
		Id:             id,
		Key:            key,
		Name:           key,
		BillingCadence: "P1M",
		Currency:       "USD",
		Status:         om.PlanStatusActive,
		Phases: []om.PlanPhase{{
			Key:       "default",
			Name:      "default",
			RateCards: cards,
		}},
	}
}

func TestListFeatures(t *testing.T) {
	t.Run("returns only active features for a meter", func(t *testing.T) {
		c, f := newTestClient(t)
		feat := featureFixture("m1_default", "m1")
		seedFeature(f, feat)
		other := featureFixture("m2_default", "m2")
		seedFeature(f, other)

		meterSlug := "m1"
		got, err := c.ListFeatures(context.Background(), &meterSlug)
		if err != nil {
			t.Fatalf("ListFeatures: %v", err)
		}
		if len(got) != 1 || got[0].Key != "m1_default" {
			t.Fatalf("ListFeatures(m1) = %+v, want only m1_default", got)
		}
	})

	t.Run("excludes archived features", func(t *testing.T) {
		c, f := newTestClient(t)
		archived := featureFixture("archived_feature", "m1")
		now := time.Now().UTC()
		archived.ArchivedAt = &now
		seedFeature(f, archived)

		got, err := c.ListFeatures(context.Background(), nil)
		if err != nil {
			t.Fatalf("ListFeatures: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("ListFeatures() = %+v, want empty (archived excluded)", got)
		}
	})

	t.Run("nil meterSlug returns all active features", func(t *testing.T) {
		c, f := newTestClient(t)
		for _, key := range []string{"a_default", "b_default", "c_default"} {
			feat := featureFixture(key, key[:1])
			seedFeature(f, feat)
		}

		got, err := c.ListFeatures(context.Background(), nil)
		if err != nil {
			t.Fatalf("ListFeatures: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("ListFeatures() = %d features, want 3", len(got))
		}
	})

	t.Run("server failure is surfaced as an error", func(t *testing.T) {
		c, f := newTestClient(t)
		f.statusOverride = http.StatusInternalServerError

		if _, err := c.ListFeatures(context.Background(), nil); err == nil {
			t.Fatalf("ListFeatures error = nil, want an error surfaced on 5xx")
		}
	})
}

func TestDeleteFeature(t *testing.T) {
	t.Run("archives an existing feature", func(t *testing.T) {
		c, f := newTestClient(t)
		feat := featureFixture("m1_default", "m1")
		seedFeature(f, feat)

		if err := c.DeleteFeature(context.Background(), "m1_default"); err != nil {
			t.Fatalf("DeleteFeature: %v", err)
		}
		got, ok := f.lookupFeature("m1_default")
		if !ok {
			t.Fatalf("feature m1_default vanished; archive must keep the record")
		}
		if got.ArchivedAt == nil {
			t.Fatalf("feature m1_default.ArchivedAt = nil, want set (soft archive)")
		}
	})

	t.Run("404 is treated as success", func(t *testing.T) {
		c, _ := newTestClient(t)
		if err := c.DeleteFeature(context.Background(), "does_not_exist"); err != nil {
			t.Fatalf("DeleteFeature(404) error = %v, want nil", err)
		}
	})

	t.Run("empty key is a permanent error", func(t *testing.T) {
		c, _ := newTestClient(t)
		err := c.DeleteFeature(context.Background(), "")
		if !IsPermanent(err) {
			t.Fatalf("DeleteFeature(\"\") error = %v, want permanent", err)
		}
	})
}

func TestArchiveFeaturesIfUnreferenced(t *testing.T) {
	t.Run("archives unreferenced and keeps referenced", func(t *testing.T) {
		c, f := newTestClient(t)
		// feature referenced by a live plan and one that is orphaned.
		referenced := featureFixture("used_key", "m1")
		orphaned := featureFixture("orphaned_key", "m1")
		for _, feat := range []om.Feature{referenced, orphaned} {
			seedFeature(f, feat)
		}
		// A live plan references used_key.
		f.plans["plan-1"] = planFixture("plan-1", "plan-1",
			usageRateCard("used_key_ratecard", "used_key"))

		keys := []string{"used_key", "orphaned_key"}
		if err := c.ArchiveFeaturesIfUnreferenced(context.Background(), keys); err != nil {
			t.Fatalf("ArchiveFeaturesIfUnreferenced: %v", err)
		}
		used, ok := f.lookupFeature("used_key")
		if !ok {
			t.Fatalf("used_key vanished")
		}
		if used.ArchivedAt != nil {
			t.Fatalf("used_key archived but a live plan references it")
		}
		orphan, ok := f.lookupFeature("orphaned_key")
		if !ok {
			t.Fatalf("orphaned_key vanished")
		}
		if orphan.ArchivedAt == nil {
			t.Fatalf("orphaned_key.ArchivedAt = nil, want archived")
		}
	})

	t.Run("empty candidate list is a no-op", func(t *testing.T) {
		c, f := newTestClient(t)
		f.plans["plan-1"] = planFixture("plan-1", "plan-1",
			usageRateCard("rc1", "k1"))
		if err := c.ArchiveFeaturesIfUnreferenced(context.Background(), nil); err != nil {
			t.Fatalf("ArchiveFeaturesIfUnreferenced(nil): %v", err)
		}
		if _, ok := f.lookupFeature("k1"); ok {
			t.Fatalf("feature k1 should not have been created")
		}
	})
}

func TestArchiveFeaturesIfUnreferenced_WalksAllPlanPages(t *testing.T) {
	// referencedFeatureKeys must walk every plan page: a feature whose
	// references only appear after the first page (the fake serves 100
	// plans/page, matching the real API default) must still be recognized
	// as referenced. Without the pagination loop this test would archive a
	// feature a live plan still depends on.
	c, f := newTestClient(t)

	// 250 seeded plans (pages 1-3). Feature references on page 3.
	for i := 0; i < 250; i++ {
		fkey := "pg" + strconv.Itoa(i)
		f.plans[fkey] = om.Plan{Id: fkey, Key: fkey, Name: fkey}
	}
	lastPlan := f.plans["pg249"]
	lastPlan.Phases = []om.PlanPhase{{
		Key:       "default",
		Name:      "default",
		RateCards: []om.RateCard{usageRateCard("rc", "referenced_on_pg3")},
	}}
	f.plans["pg249"] = lastPlan

	// Seed both candidate features so archive decisions are observable.
	ref := featureFixture("referenced_on_pg3", "m1")
	orphan := featureFixture("orphan_feature", "m1")
	for _, feat := range []om.Feature{ref, orphan} {
		seedFeature(f, feat)
	}

	if err := c.ArchiveFeaturesIfUnreferenced(context.Background(), []string{"referenced_on_pg3", "orphan_feature"}); err != nil {
		t.Fatalf("ArchiveFeaturesIfUnreferenced: %v", err)
	}

	if got, ok := f.lookupFeature("referenced_on_pg3"); !ok {
		t.Fatalf("referenced_on_pg3 vanished")
	} else if got.ArchivedAt != nil {
		t.Fatalf("feature referenced only on plan page 3 was archived")
	}
	if got, ok := f.lookupFeature("orphan_feature"); !ok {
		t.Fatalf("orphan_feature vanished")
	} else if got.ArchivedAt == nil {
		t.Fatalf("orphan_feature.ArchivedAt = nil, want archived")
	}
}

func TestArchiveUnreferencedMeterFeatures(t *testing.T) {
	c, f := newTestClient(t)
	// Two active features bound to the meter, one also referenced by a
	// live plan (must be kept), one orphaned (must be archived). A feature
	// bound to a different meter must be untouched.
	kept := featureFixture("meter_default", "meter_x")
	orphan := featureFixture("meter_orphan", "meter_x")
	otherMeter := featureFixture("other_meter_feature", "other_meter")
	for _, feat := range []om.Feature{kept, orphan, otherMeter} {
		seedFeature(f, feat)
	}
	f.plans["plan-keeps"] = planFixture("plan-keeps", "plan-keeps",
		usageRateCard("rc-keeps", "meter_default"))

	if err := c.ArchiveUnreferencedMeterFeatures(context.Background(), "meter_x"); err != nil {
		t.Fatalf("ArchiveUnreferencedMeterFeatures: %v", err)
	}

	if got, _ := f.lookupFeature("meter_default"); got.ArchivedAt != nil {
		t.Fatalf("meter_default archived but a live plan references it")
	}
	if got, _ := f.lookupFeature("meter_orphan"); got.ArchivedAt == nil {
		t.Fatalf("meter_orphan.ArchivedAt = nil, want archived")
	}
	if got, _ := f.lookupFeature("other_meter_feature"); got.ArchivedAt != nil {
		t.Fatalf("other_meter_feature archived though it belongs to a different meter")
	}
}

// TestEnsureFeature ensures the existing EnsureFeature path still works
// against the fake server (create + idempotent get).
func TestEnsureFeature(t *testing.T) {
	t.Run("creates then idempotently returns", func(t *testing.T) {
		c, f := newTestClient(t)
		desired := DesiredFeature{Key: "m1_default", Name: "m1_default", MeterSlug: "m1"}
		feat, err := c.EnsureFeature(context.Background(), desired)
		if err != nil {
			t.Fatalf("EnsureFeature create: %v", err)
		}
		if feat.Key != "m1_default" {
			t.Fatalf("created feature Key = %q, want m1_default", feat.Key)
		}
		again, err := c.EnsureFeature(context.Background(), desired)
		if err != nil {
			t.Fatalf("EnsureFeature second call: %v", err)
		}
		if f.features["m1_default"].Id != again.Id {
			t.Fatalf("second EnsureFeature returned a different feature")
		}
	})
}
