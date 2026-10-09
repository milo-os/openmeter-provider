// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"net/http"
	"sort"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// desiredPlanFixture returns a minimal valid DesiredPlan with one usage rate
// card referencing featureKey.
func desiredPlanFixture(key, name, featureKey string) DesiredPlan {
	return DesiredPlan{
		Key:         key,
		Name:        name,
		Description: "offer-name",
		Metadata:    map[string]string{"miloapis.com/offer-name": "offer-name"},
		Phases: []om.PlanPhase{{
			Key:       "default_phase",
			Name:      "Default Phase",
			RateCards: []om.RateCard{usageRateCard(featureKey, featureKey)},
		}},
	}
}

// plansByKey returns every plan stored under key, ordered by version.
func plansByKey(f *fakeServer, key string) []om.Plan {
	var out []om.Plan
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
	if plan.Status != om.PlanStatusActive {
		t.Fatalf("status = %q, want active — a draft plan cannot be subscribed to", plan.Status)
	}
	if plan.Version != 1 {
		t.Errorf("version = %d, want 1", plan.Version)
	}
	wantHash, _ := desired.SpecHash()
	if got := planSpecHash(plan); got != wantHash {
		t.Errorf("spec hash metadata = %q, want %q", got, wantHash)
	}
	if plan.Metadata == nil || (*plan.Metadata)["miloapis.com/offer-name"] != "offer-name" {
		t.Errorf("caller metadata not carried: %v", plan.Metadata)
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
		if plan.Version != 1 || plan.Status != om.PlanStatusActive {
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
	if plan.Version != 2 || plan.Status != om.PlanStatusActive || plan.Name != "Offer Renamed" {
		t.Fatalf("plan = v%d %s %q, want v2 active 'Offer Renamed'", plan.Version, plan.Status, plan.Name)
	}
	versions := plansByKey(f, "offer_uid")
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(versions))
	}
	if versions[0].Status != om.PlanStatusArchived {
		t.Errorf("v1 status = %q, want archived", versions[0].Status)
	}
}

// TestEnsurePlan_PublishesLeftoverDraft covers a crash between create and
// publish: the draft is updated in place and published, not duplicated
// (OpenMeter allows only one draft per key).
func TestEnsurePlan_PublishesLeftoverDraft(t *testing.T) {
	c, f := newTestClient(t)
	f.plans["plan-draft"] = om.Plan{
		Id: "plan-draft", Key: "offer_uid", Name: "stale", Version: 1, Status: om.PlanStatusDraft,
	}

	plan, err := c.EnsurePlan(context.Background(), desiredPlanFixture("offer_uid", "Offer", "feat_a"))
	if err != nil {
		t.Fatalf("EnsurePlan: %v", err)
	}
	if plan.Id != "plan-draft" || plan.Status != om.PlanStatusActive || plan.Name != "Offer" {
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
	archived.Status = om.PlanStatusArchived
	f.plans[v1.Id] = archived

	plan, err := c.EnsurePlan(context.Background(), desired)
	if err != nil {
		t.Fatalf("EnsurePlan after archive: %v", err)
	}
	if plan.Version != 2 || plan.Status != om.PlanStatusActive {
		t.Fatalf("plan = v%d %s, want v2 active", plan.Version, plan.Status)
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

	// The reserved hash key never feeds back into the hash.
	b.Metadata[PlanSpecHashMetadataKey] = "anything"
	if hb, _ = b.SpecHash(); ha != hb {
		t.Fatalf("hash depends on the reserved metadata key")
	}

	for name, mutate := range map[string]func(*DesiredPlan){
		"name":      func(d *DesiredPlan) { d.Name = "Other" },
		"metadata":  func(d *DesiredPlan) { d.Metadata["x"] = "y" },
		"rate card": func(d *DesiredPlan) { d.Phases[0].RateCards = []om.RateCard{usageRateCard("feat_b", "feat_b")} },
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
	f.plans["p3"] = om.Plan{Id: "p3", Key: "k", Version: 3, Status: om.PlanStatusActive}
	f.plans["p1"] = om.Plan{Id: "p1", Key: "k", Version: 1, Status: om.PlanStatusArchived, DeletedAt: &deletedAt}
	f.plans["p2"] = om.Plan{Id: "p2", Key: "k", Version: 2, Status: om.PlanStatusArchived}
	f.plans["other"] = om.Plan{Id: "other", Key: "other", Version: 1, Status: om.PlanStatusActive}

	got, err := c.ListPlanVersions(context.Background(), "k", false)
	if err != nil {
		t.Fatalf("ListPlanVersions: %v", err)
	}
	if len(got) != 2 || got[0].Id != "p2" || got[1].Id != "p3" {
		t.Fatalf("ListPlanVersions(k) = %+v, want [p2 p3] by version", ids(got))
	}

	got, err = c.ListPlanVersions(context.Background(), "k", true)
	if err != nil {
		t.Fatalf("ListPlanVersions(includeDeleted): %v", err)
	}
	if len(got) != 3 || got[0].Id != "p1" {
		t.Fatalf("ListPlanVersions(k, deleted) = %v, want [p1 p2 p3]", ids(got))
	}

	got, err = c.ListPlanVersions(context.Background(), "missing", false)
	if err != nil || len(got) != 0 {
		t.Fatalf("ListPlanVersions(missing) = %v, %v; want empty, nil", ids(got), err)
	}
}

func ids(plans []om.Plan) []string {
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
		if got.DeletedAt == nil || got.Status != om.PlanStatusArchived {
			t.Fatalf("plan after delete = %s deleted=%v, want archived and deleted", got.Status, got.DeletedAt)
		}
	})

	t.Run("draft plan is deleted directly", func(t *testing.T) {
		c, f := newTestClient(t)
		f.plans["d"] = om.Plan{Id: "d", Key: "k", Version: 1, Status: om.PlanStatusDraft}
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
		if err := c.DeletePlan(context.Background(), om.Plan{Id: "gone", DeletedAt: &now}); err != nil {
			t.Fatalf("DeletePlan(deleted): %v", err)
		}
		if err := c.DeletePlan(context.Background(), om.Plan{Id: "missing", Status: om.PlanStatusActive}); err != nil {
			t.Fatalf("DeletePlan(404): %v", err)
		}
		if f.planWrites != 0 {
			t.Fatalf("plan writes = %d, want 0", f.planWrites)
		}
	})
}

func TestPlanFeatureKeys(t *testing.T) {
	flatKey := "flat_feature"
	var flatRef, flatNoRef om.RateCard
	if err := flatRef.FromRateCardFlatFee(om.RateCardFlatFee{
		Type: om.RateCardFlatFeeTypeFlatFee, Key: "fee", Name: "fee", FeatureKey: &flatKey,
	}); err != nil {
		t.Fatal(err)
	}
	if err := flatNoRef.FromRateCardFlatFee(om.RateCardFlatFee{
		Type: om.RateCardFlatFeeTypeFlatFee, Key: "fee2", Name: "fee2",
	}); err != nil {
		t.Fatal(err)
	}
	plan := planFixture("p", "p", usageRateCard("rc", "usage_feature"), flatRef, flatNoRef)

	got := PlanFeatureKeys(plan)
	if len(got) != 2 || got[0] != "usage_feature" || got[1] != flatKey {
		t.Fatalf("PlanFeatureKeys = %v, want [usage_feature flat_feature]", got)
	}
}
