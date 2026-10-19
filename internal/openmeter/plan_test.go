// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"net/http"
	"sort"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
	omv3 "github.com/openmeterio/openmeter/api/v3"
	"github.com/openmeterio/openmeter/openmeter/productcatalog"
)

// desiredPlanFixture returns a minimal valid DesiredPlan with one usage rate
// card referencing featureKey.
func desiredPlanFixture(key, name, featureKey string) DesiredPlan {
	return DesiredPlan{
		Key:  key,
		Name: name,
		Phases: []omv3.BillingPlanPhase{{
			Key:       "default_phase",
			Name:      "Default Phase",
			RateCards: []omv3.BillingRateCard{usageRateCard(featureKey, featureKey)},
		}},
	}
}

// plansByKey returns every plan stored under key, ordered by version.
func plansByKey(f *fakeServer, key string) []omv3.BillingPlan {
	var out []omv3.BillingPlan
	for _, p := range f.plans {
		if p.Key == key {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

func TestEnsurePlan_CreatesAndPublishes(t *testing.T) {
	c, _ := newTestClient(t)
	desired := desiredPlanFixture("offer_uid", "Offer", "feat_a")

	plan, err := c.EnsurePlan(context.Background(), desired)
	if err != nil {
		t.Fatalf("EnsurePlan: %v", err)
	}
	if plan.Status != omv3.BillingPlanStatusActive {
		t.Fatalf("status = %q, want active — a draft plan cannot be subscribed to", plan.Status)
	}
	if plan.Version != 1 {
		t.Errorf("version = %d, want 1", plan.Version)
	}
	wantHash, _ := desired.SpecHash()
	if got := planSpecHash(plan); got != wantHash {
		t.Errorf("spec hash = %q, want %q", got, wantHash)
	}
}

func TestEnsurePlan_NoOpWhenUnchanged(t *testing.T) {
	c, f := newTestClient(t)
	desired := desiredPlanFixture("offer_uid", "Offer", "feat_a")
	if _, err := c.EnsurePlan(context.Background(), desired); err != nil {
		t.Fatalf("EnsurePlan: %v", err)
	}
	writes := f.planWrites

	for i := 0; i < 3; i++ {
		plan, err := c.EnsurePlan(context.Background(), desired)
		if err != nil {
			t.Fatalf("EnsurePlan #%d: %v", i+2, err)
		}
		if plan.Version != 1 || plan.Status != omv3.BillingPlanStatusActive {
			t.Fatalf("plan = v%d %s, want v1 active", plan.Version, plan.Status)
		}
	}
	if f.planWrites != writes {
		t.Fatalf("plan writes = %d after converging, want %d: EnsurePlan must not write when nothing changed",
			f.planWrites, writes)
	}
}

// TestEnsurePlan_ChangeCreatesNewVersion pins the versioning rule: an active
// plan cannot be updated, so a change publishes v2 and OpenMeter archives v1.
func TestEnsurePlan_ChangeCreatesNewVersion(t *testing.T) {
	c, f := newTestClient(t)
	if _, err := c.EnsurePlan(context.Background(), desiredPlanFixture("offer_uid", "Offer", "feat_a")); err != nil {
		t.Fatalf("EnsurePlan v1: %v", err)
	}

	renamed := desiredPlanFixture("offer_uid", "Offer Renamed", "feat_a")
	plan, err := c.EnsurePlan(context.Background(), renamed)
	if err != nil {
		t.Fatalf("EnsurePlan v2: %v", err)
	}
	if plan.Version != 2 || plan.Status != omv3.BillingPlanStatusActive || plan.Name != "Offer Renamed" {
		t.Fatalf("plan = v%d %s %q, want v2 active 'Offer Renamed'", plan.Version, plan.Status, plan.Name)
	}
	versions := plansByKey(f, "offer_uid")
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(versions))
	}
	if versions[0].Status != omv3.BillingPlanStatusArchived {
		t.Errorf("v1 status = %q, want archived", versions[0].Status)
	}
}

// TestEnsurePlan_PublishesLeftoverDraft covers a crash between create and
// publish: the draft is updated in place and published, not duplicated
// (OpenMeter allows only one draft per key).
func TestEnsurePlan_PublishesLeftoverDraft(t *testing.T) {
	c, f := newTestClient(t)
	f.plans["plan-draft"] = omv3.BillingPlan{
		Id: "plan-draft", Key: "offer_uid", Name: "stale", Version: 1, Status: omv3.BillingPlanStatusDraft,
	}

	plan, err := c.EnsurePlan(context.Background(), desiredPlanFixture("offer_uid", "Offer", "feat_a"))
	if err != nil {
		t.Fatalf("EnsurePlan: %v", err)
	}
	if plan.Id != "plan-draft" || plan.Status != omv3.BillingPlanStatusActive || plan.Name != "Offer" {
		t.Fatalf("plan = %s %s %q, want plan-draft active 'Offer'", plan.Id, plan.Status, plan.Name)
	}
	if n := len(plansByKey(f, "offer_uid")); n != 1 {
		t.Fatalf("versions = %d, want 1 (draft reused)", n)
	}
}

// TestEnsurePlan_RepublishesArchivedPlan: a GA Offer must always have an
// assignable plan, so a plan archived out of band gets a new active version.
func TestEnsurePlan_RepublishesArchivedPlan(t *testing.T) {
	c, f := newTestClient(t)
	desired := desiredPlanFixture("offer_uid", "Offer", "feat_a")
	v1, err := c.EnsurePlan(context.Background(), desired)
	if err != nil {
		t.Fatalf("EnsurePlan: %v", err)
	}
	archived := f.plans[v1.Id]
	archived.Status = omv3.BillingPlanStatusArchived
	f.plans[v1.Id] = archived

	plan, err := c.EnsurePlan(context.Background(), desired)
	if err != nil {
		t.Fatalf("EnsurePlan after archive: %v", err)
	}
	if plan.Version != 2 || plan.Status != omv3.BillingPlanStatusActive {
		t.Fatalf("plan = v%d %s, want v2 active", plan.Version, plan.Status)
	}
}

// TestPlanSpecHash: the hash is read back from the description; a plan
// without one (e.g. written before the provider used v3) has none, so it gets
// a new version once.
func TestPlanSpecHash(t *testing.T) {
	for desc, want := range map[string]string{
		"milo-spec-hash:abc": "abc",
		"payg-3":             "",
		"":                   "",
	} {
		plan := omv3.BillingPlan{Description: &desc}
		if got := planSpecHash(plan); got != want {
			t.Errorf("planSpecHash(%q) = %q, want %q", desc, got, want)
		}
	}
	if got := planSpecHash(omv3.BillingPlan{}); got != "" {
		t.Errorf("planSpecHash(no description) = %q, want empty", got)
	}
}

func TestEnsurePlan_EmptyKeyIsPermanent(t *testing.T) {
	c, _ := newTestClient(t)
	_, err := c.EnsurePlan(context.Background(), DesiredPlan{})
	if !IsPermanent(err) {
		t.Fatalf("EnsurePlan(empty key) err = %v, want permanent", err)
	}
}

func TestEnsurePlan_ServerErrorIsTransient(t *testing.T) {
	c, f := newTestClient(t)
	f.statusOverride = http.StatusServiceUnavailable
	_, err := c.EnsurePlan(context.Background(), desiredPlanFixture("offer_uid", "Offer", "feat_a"))
	if !IsTransient(err) {
		t.Fatalf("EnsurePlan on 503 err = %v, want transient", err)
	}
}

func TestDesiredPlanSpecHash(t *testing.T) {
	a := desiredPlanFixture("k", "Name", "feat_a")
	b := desiredPlanFixture("k", "Name", "feat_a")
	ha, err := a.SpecHash()
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}
	hb, _ := b.SpecHash()
	if ha != hb {
		t.Fatalf("equal plans hash differently: %s vs %s", ha, hb)
	}

	for name, mutate := range map[string]func(*DesiredPlan){
		"name": func(d *DesiredPlan) { d.Name = "Other" },
		"rate card": func(d *DesiredPlan) {
			d.Phases[0].RateCards = []omv3.BillingRateCard{usageRateCard("feat_b", "feat_b")}
		},
		"unit conversion": func(d *DesiredPlan) {
			d.Phases[0].RateCards[0].UnitConfig = &omv3.BillingUnitConfig{
				Operation: omv3.BillingUnitConfigOperationDivide, ConversionFactor: "1073741824",
			}
		},
	} {
		d := desiredPlanFixture("k", "Name", "feat_a")
		mutate(&d)
		if h, _ := d.SpecHash(); h == ha {
			t.Errorf("changing %s did not change the hash", name)
		}
	}
}

func TestListPlanVersions(t *testing.T) {
	c, f := newTestClient(t)
	deletedAt := time.Now().UTC()
	f.plans["p3"] = omv3.BillingPlan{Id: "p3", Key: "k", Version: 3, Status: omv3.BillingPlanStatusActive}
	f.plans["p1"] = omv3.BillingPlan{Id: "p1", Key: "k", Version: 1, Status: omv3.BillingPlanStatusArchived, DeletedAt: &deletedAt}
	f.plans["p2"] = omv3.BillingPlan{Id: "p2", Key: "k", Version: 2, Status: omv3.BillingPlanStatusArchived}
	f.plans["other"] = omv3.BillingPlan{Id: "other", Key: "other", Version: 1, Status: omv3.BillingPlanStatusActive}

	got, err := c.ListPlanVersions(context.Background(), "k")
	if err != nil {
		t.Fatalf("ListPlanVersions: %v", err)
	}
	if len(got) != 2 || got[0].Id != "p2" || got[1].Id != "p3" {
		t.Fatalf("ListPlanVersions(k) = %+v, want [p2 p3] by version", ids(got))
	}

	got, err = c.ListPlanVersions(context.Background(), "missing")
	if err != nil || len(got) != 0 {
		t.Fatalf("ListPlanVersions(missing) = %v, %v; want empty, nil", ids(got), err)
	}
}

func ids(plans []omv3.BillingPlan) []string {
	out := make([]string, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.Id)
	}
	return out
}

func TestDeletePlan(t *testing.T) {
	t.Run("active plan is archived then deleted", func(t *testing.T) {
		c, f := newTestClient(t)
		plan, err := c.EnsurePlan(context.Background(), desiredPlanFixture("k", "Offer", "feat_a"))
		if err != nil {
			t.Fatalf("EnsurePlan: %v", err)
		}
		if err := c.DeletePlan(context.Background(), plan); err != nil {
			t.Fatalf("DeletePlan(active): %v", err)
		}
		got := f.plans[plan.Id]
		if got.DeletedAt == nil || got.Status != omv3.BillingPlanStatusArchived {
			t.Fatalf("plan after delete = %s deleted=%v, want archived and deleted", got.Status, got.DeletedAt)
		}
	})

	t.Run("draft plan is deleted directly", func(t *testing.T) {
		c, f := newTestClient(t)
		f.plans["d"] = omv3.BillingPlan{Id: "d", Key: "k", Version: 1, Status: omv3.BillingPlanStatusDraft}
		if err := c.DeletePlan(context.Background(), f.plans["d"]); err != nil {
			t.Fatalf("DeletePlan(draft): %v", err)
		}
		if f.plans["d"].DeletedAt == nil {
			t.Fatalf("draft not deleted")
		}
	})

	t.Run("already deleted and missing plans are success", func(t *testing.T) {
		c, f := newTestClient(t)
		now := time.Now().UTC()
		if err := c.DeletePlan(context.Background(), omv3.BillingPlan{Id: "gone", DeletedAt: &now}); err != nil {
			t.Fatalf("DeletePlan(deleted): %v", err)
		}
		if err := c.DeletePlan(context.Background(), omv3.BillingPlan{Id: "missing", Status: omv3.BillingPlanStatusActive}); err != nil {
			t.Fatalf("DeletePlan(404): %v", err)
		}
		if f.planWrites != 0 {
			t.Fatalf("plan writes = %d, want 0", f.planWrites)
		}
	})
}

func TestPlanFeatureIDs(t *testing.T) {
	flat := omv3.BillingRateCard{Key: "fee", Name: "fee"}
	plan := planFixture("p", "p", usageRateCard("rc", "usage_feature"), flat, usageRateCard("rc2", "other"))

	got := PlanFeatureIDs(plan)
	if len(got) != 2 || got[0] != "feat-usage_feature" || got[1] != "feat-other" {
		t.Fatalf("PlanFeatureIDs = %v, want [feat-usage_feature feat-other]", got)
	}
}

// TestPlanUsageRateCards reads a plan version's usage rate cards with what
// pricing needs: the feature's meter and filters (archived features
// included, since older versions may reference them), the price and the
// unit conversion. Flat fees are left out.
func TestPlanUsageRateCards(t *testing.T) {
	c, f := newTestClient(t)
	region := "us-east"
	active := featureFixture("memory", "memory_seconds")
	active.AdvancedMeterGroupByFilters = &map[string]om.FilterString{"region": {Eq: &region}}
	seedFeature(f, active)
	archived := featureFixture("old", "old_meter")
	now := time.Now().UTC()
	archived.ArchivedAt = &now
	seedFeature(f, archived)

	memory := usageRateCard("memory", "memory")
	memory.UnitConfig = &omv3.BillingUnitConfig{Operation: omv3.BillingUnitConfigOperationDivide, ConversionFactor: "1073741824"}
	fee := omv3.BillingRateCard{Key: "fee", Name: "fee"}
	if err := fee.Price.FromBillingPriceFlat(omv3.BillingPriceFlat{Type: omv3.BillingPriceFlatTypeFlat, Amount: "5"}); err != nil {
		t.Fatal(err)
	}
	f.plans["p1"] = planFixture("p1", "k", memory, usageRateCard("old", "old"), fee)

	cards, err := c.PlanUsageRateCards(context.Background(), "p1")
	if err != nil {
		t.Fatalf("PlanUsageRateCards: %v", err)
	}
	if len(cards) != 2 {
		t.Fatalf("cards = %+v, want the two usage cards", cards)
	}
	mem := cards[0]
	if mem.MeterSlug != "memory_seconds" || mem.Filters["region"].Eq == nil || *mem.Filters["region"].Eq != region {
		t.Errorf("memory card meter/filters = %q %+v", mem.MeterSlug, mem.Filters)
	}
	if mem.Price == nil || mem.Price.Type() != productcatalog.UnitPriceType {
		t.Errorf("memory card price = %+v, want unit", mem.Price)
	}
	if mem.UnitConfig == nil || mem.UnitConfig.ConversionFactor.String() != "1073741824" {
		t.Errorf("memory card unit config = %+v", mem.UnitConfig)
	}
	if cards[1].MeterSlug != "old_meter" {
		t.Errorf("card on an archived feature = %+v", cards[1])
	}

	if _, err := c.PlanUsageRateCards(context.Background(), "missing"); err == nil {
		t.Error("missing plan: want an error")
	}
}
