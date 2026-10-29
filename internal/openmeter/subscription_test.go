// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
	omv3 "github.com/openmeterio/openmeter/api/v3"
)

// serveSubscriptions emulates OpenMeter's subscription lifecycle closely
// enough to exercise EnsureSubscription and CancelSubscriptions:
//
//   - one live (active, canceled, or scheduled) subscription per customer
//     at a time, else 409;
//   - transitions not allowed in the current state are 403 (cancel and
//     change/migrate only from active, continue only from canceled, delete
//     only when scheduled);
//   - next_billing_cycle ends the current subscription in 30 days and
//     schedules its successor then; immediate ends it now;
//   - v1 change and migrate refuse a target plan that converts usage, which
//     only the v3 change route (immediate) accepts. It keeps no metadata.
//   - a subscription bills from the anchor it was created with, else from
//     its start; v1 change and migrate keep the previous anchor, while a v3
//     change without one bills from the change. Anchors are not normalized.
//
// Statuses are stored, not computed from time.
func (f *fakeServer) serveSubscriptions(w http.ResponseWriter, r *http.Request) bool {
	writeJSON := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	problem := func(status int, detail string) {
		writeJSON(status, map[string]any{"status": status, "detail": detail})
	}

	if strings.HasPrefix(r.URL.Path, "/api/v1/customers/") && strings.HasSuffix(r.URL.Path, "/subscriptions") {
		key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/customers/"), "/subscriptions")
		cust, ok := f.customers[key]
		if !ok {
			problem(http.StatusNotFound, "customer not found")
			return true
		}
		subs := f.customerSubscriptions(cust.Id)
		page, pageSize := pageParams(r)
		start, end := pageBounds(page, pageSize, len(subs))
		writeJSON(http.StatusOK, om.SubscriptionPaginatedResponse{
			Items: subs[start:end], Page: page, PageSize: pageSize, TotalCount: len(subs),
		})
		return true
	}

	if strings.HasPrefix(r.URL.Path, "/api/v3/openmeter/subscriptions/") && strings.HasSuffix(r.URL.Path, "/change") {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v3/openmeter/subscriptions/"), "/change")
		sub, ok := f.subscriptions[id]
		if !ok {
			problem(http.StatusNotFound, "subscription not found")
			return true
		}
		if sub.Status != om.SubscriptionStatusActive {
			problem(http.StatusForbidden, fmt.Sprintf("transition not allowed in state %s", sub.Status))
			return true
		}
		var body omv3.BillingSubscriptionChange
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Plan.Key == nil || body.Plan.Version == nil {
			problem(http.StatusBadRequest, "bad body")
			return true
		}
		if e, err := body.Timing.AsBillingSubscriptionEditTimingEnum(); err != nil || e != omv3.BillingSubscriptionEditTimingEnumImmediate {
			problem(http.StatusBadRequest, "only immediate timing is emulated")
			return true
		}
		plan, ok := f.planVersion(*body.Plan.Key, *body.Plan.Version)
		if !ok || plan.Status != omv3.BillingPlanStatusActive {
			problem(http.StatusBadRequest, "plan is not active")
			return true
		}
		f.v3Changes++
		f.endSubscription(&sub, false)
		next := f.newSubscription(sub.CustomerId, plan, om.SubscriptionStatusActive, time.Now(), nil)
		if body.BillingAnchor != nil {
			f.setAnchor(next.Id, *body.BillingAnchor)
		}
		writeJSON(http.StatusOK, map[string]any{"current": map[string]string{"id": sub.Id}, "next": map[string]string{"id": next.Id}})
		return true
	}

	if !strings.HasPrefix(r.URL.Path, "/api/v1/subscriptions") {
		return false
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/subscriptions"), "/"), "/")

	if r.Method == http.MethodPost && parts[0] == "" {
		var body om.PlanSubscriptionCreate
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.CustomerKey == nil {
			problem(http.StatusBadRequest, "bad body")
			return true
		}
		cust, ok := f.customers[*body.CustomerKey]
		if !ok {
			problem(http.StatusNotFound, "customer not found")
			return true
		}
		if f.billingNotReady {
			problem(http.StatusConflict, "conflict error: invalid billing setup: failed to get stripe customer data: customer has no data for stripe app")
			return true
		}
		if f.liveSubscription(cust.Id) {
			problem(http.StatusConflict, "only_single_subscription_allowed_per_customer_at_a_time")
			return true
		}
		plan, ok := f.activePlan(body.Plan.Key)
		if !ok {
			problem(http.StatusNotFound, "plan not found")
			return true
		}
		sub := f.newSubscription(cust.Id, plan, om.SubscriptionStatusActive, time.Now(), body.Metadata)
		if body.BillingAnchor != nil {
			sub = f.setAnchor(sub.Id, *body.BillingAnchor)
		}
		writeJSON(http.StatusCreated, sub)
		return true
	}

	sub, ok := f.subscriptions[parts[0]]
	if !ok {
		problem(http.StatusNotFound, "subscription not found")
		return true
	}
	forbidden := func() {
		problem(http.StatusForbidden, fmt.Sprintf("transition not allowed in state %s", sub.Status))
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch {
	case r.Method == http.MethodDelete && action == "":
		if sub.Status != om.SubscriptionStatusScheduled {
			forbidden()
			return true
		}
		delete(f.subscriptions, sub.Id)
		f.subscriptionWrites++
		w.WriteHeader(http.StatusNoContent)

	case action == "cancel":
		if sub.Status != om.SubscriptionStatusActive {
			forbidden()
			return true
		}
		var body om.CancelSubscriptionJSONBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.endSubscription(&sub, timingIsNextCycle(body.Timing))
		writeJSON(http.StatusOK, sub)

	case action == "change":
		if sub.Status != om.SubscriptionStatusActive {
			forbidden()
			return true
		}
		var body om.PlanSubscriptionChange
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			problem(http.StatusBadRequest, "bad body")
			return true
		}
		plan, ok := f.activePlan(body.Plan.Key)
		if !ok {
			problem(http.StatusBadRequest, "plan is not active")
			return true
		}
		if planHasUnitConfig(plan) {
			problem(http.StatusBadRequest, "unit_config is not representable in v1")
			return true
		}
		f.endSubscription(&sub, false)
		next := f.newSubscription(sub.CustomerId, plan, om.SubscriptionStatusActive, time.Now(), body.Metadata)
		f.setAnchor(next.Id, sub.BillingAnchor)
		writeJSON(http.StatusOK, om.SubscriptionChangeResponseBody{Current: sub, Next: om.SubscriptionExpanded{Id: next.Id}})

	case action == "migrate":
		if sub.Status != om.SubscriptionStatusActive {
			forbidden()
			return true
		}
		var body om.MigrateSubscriptionJSONBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		var plan omv3.BillingPlan
		found := false
		for _, p := range f.plans {
			if p.Key == sub.Plan.Key && body.TargetVersion != nil && p.Version == *body.TargetVersion && p.DeletedAt == nil {
				plan, found = p, true
			}
		}
		switch {
		case !found:
			problem(http.StatusNotFound, "plan version not found")
			return true
		case plan.Version <= sub.Plan.Version:
			problem(http.StatusBadRequest, "already at version")
			return true
		case planHasUnitConfig(plan):
			problem(http.StatusBadRequest, "unit_config is not representable in v1")
			return true
		}
		nextCycle := timingIsNextCycle(body.Timing)
		f.endSubscription(&sub, nextCycle)
		status, from := om.SubscriptionStatusActive, time.Now()
		if nextCycle {
			status, from = om.SubscriptionStatusScheduled, *sub.ActiveTo
		}
		next := f.newSubscription(sub.CustomerId, plan, status, from, sub.Metadata)
		f.setAnchor(next.Id, sub.BillingAnchor)
		writeJSON(http.StatusOK, om.SubscriptionChangeResponseBody{Current: sub, Next: om.SubscriptionExpanded{Id: next.Id}})

	case action == "restore":
		if sub.Status != om.SubscriptionStatusCanceled {
			forbidden()
			return true
		}
		for id, other := range f.subscriptions {
			if other.CustomerId == sub.CustomerId && other.Status == om.SubscriptionStatusScheduled {
				delete(f.subscriptions, id)
			}
		}
		f.continueSubscription(&sub)
		writeJSON(http.StatusOK, sub)

	case action == "unschedule-cancelation":
		if sub.Status != om.SubscriptionStatusCanceled {
			forbidden()
			return true
		}
		for _, other := range f.subscriptions {
			if other.CustomerId == sub.CustomerId && other.Status == om.SubscriptionStatusScheduled {
				problem(http.StatusConflict, "only_single_subscription_allowed_per_customer_at_a_time")
				return true
			}
		}
		f.continueSubscription(&sub)
		writeJSON(http.StatusOK, sub)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
	return true
}

func timingIsNextCycle(t *om.SubscriptionTiming) bool {
	if t == nil {
		return false
	}
	e, err := t.AsSubscriptionTimingEnum()
	return err == nil && e == om.SubscriptionTimingEnumNextBillingCycle
}

func (f *fakeServer) newSubscription(customerID string, plan omv3.BillingPlan, status om.SubscriptionStatus, from time.Time, metadata *om.Metadata) om.Subscription {
	f.subscriptionSeq++
	f.subscriptionWrites++
	sub := om.Subscription{
		Id:         fmt.Sprintf("sub-%d", f.subscriptionSeq),
		CustomerId: customerID,
		Name:       plan.Name,
		Plan:       &om.PlanReference{Id: plan.Id, Key: plan.Key, Version: plan.Version},
		Status:     status,
		// The sequence breaks ties between subscriptions created in the
		// same instant, keeping the newest-first order deterministic.
		ActiveFrom: from.Add(time.Duration(f.subscriptionSeq) * time.Millisecond),
		Metadata:   metadata,
	}
	sub.BillingAnchor = sub.ActiveFrom
	f.subscriptions[sub.Id] = sub
	return sub
}

func (f *fakeServer) setAnchor(id string, anchor time.Time) om.Subscription {
	sub := f.subscriptions[id]
	sub.BillingAnchor = anchor
	f.subscriptions[id] = sub
	return sub
}

func (f *fakeServer) endSubscription(sub *om.Subscription, nextCycle bool) {
	f.subscriptionWrites++
	end := time.Now()
	sub.Status = om.SubscriptionStatusInactive
	if nextCycle {
		end = end.Add(30 * 24 * time.Hour)
		sub.Status = om.SubscriptionStatusCanceled
	}
	sub.ActiveTo = &end
	f.subscriptions[sub.Id] = *sub
}

func (f *fakeServer) continueSubscription(sub *om.Subscription) {
	f.subscriptionWrites++
	sub.Status = om.SubscriptionStatusActive
	sub.ActiveTo = nil
	f.subscriptions[sub.Id] = *sub
}

func (f *fakeServer) liveSubscription(customerID string) bool {
	for _, s := range f.subscriptions {
		if s.CustomerId == customerID && s.Status != om.SubscriptionStatusInactive {
			return true
		}
	}
	return false
}

func (f *fakeServer) activePlan(key string) (omv3.BillingPlan, bool) {
	for _, p := range f.plans {
		if p.Key == key && p.Status == omv3.BillingPlanStatusActive && p.DeletedAt == nil {
			return p, true
		}
	}
	return omv3.BillingPlan{}, false
}

func (f *fakeServer) planVersion(key string, version int) (omv3.BillingPlan, bool) {
	for _, p := range f.plans {
		if p.Key == key && p.Version == version && p.DeletedAt == nil {
			return p, true
		}
	}
	return omv3.BillingPlan{}, false
}

// customerSubscriptions lists a customer's subscriptions newest first, like
// OpenMeter's default activeFrom-descending order.
func (f *fakeServer) customerSubscriptions(customerID string) []om.Subscription {
	var out []om.Subscription
	for _, s := range f.subscriptions {
		if s.CustomerId == customerID {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ActiveFrom.After(out[j].ActiveFrom) })
	return out
}

// subsByStatus summarizes a customer's subscriptions as "status:key@version"
// in creation order.
func (f *fakeServer) subsByStatus(customerKey string) []string {
	subs := f.customerSubscriptions("id-" + customerKey)
	sort.Slice(subs, func(i, j int) bool { return subs[i].Id < subs[j].Id })
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		out = append(out, fmt.Sprintf("%s:%s@%d", s.Status, s.Plan.Key, s.Plan.Version))
	}
	return out
}

// seedCustomer stores a customer the way serveCustomers does.
func (f *fakeServer) seedCustomer(key string) {
	k := key
	f.customers[key] = om.Customer{Id: "id-" + key, Key: &k, Name: key}
}

// seedPlanVersion stores a plan version with the given status.
func (f *fakeServer) seedPlanVersion(key string, version int, status omv3.BillingPlanStatus) {
	f.planSeq++
	id := fmt.Sprintf("plan-%d", f.planSeq)
	f.plans[id] = omv3.BillingPlan{Id: id, Key: key, Name: key, Version: version, Status: status}
}

// publishPlanVersion archives the active version of key and activates version.
func (f *fakeServer) publishPlanVersion(key string, version int) {
	for id, p := range f.plans {
		if p.Key == key && p.Status == omv3.BillingPlanStatusActive {
			p.Status = omv3.BillingPlanStatusArchived
			f.plans[id] = p
		}
	}
	f.seedPlanVersion(key, version, omv3.BillingPlanStatusActive)
}

// convertUsage gives every version of key a rate card that converts usage.
func (f *fakeServer) convertUsage(key string) {
	for id, p := range f.plans {
		if p.Key == key {
			p.Phases = []omv3.BillingPlanPhase{{Key: "default", Name: "default", RateCards: []omv3.BillingRateCard{{
				Key: "memory", Name: "memory",
				UnitConfig: &omv3.BillingUnitConfig{Operation: omv3.BillingUnitConfigOperationDivide, ConversionFactor: "1073741824"},
			}}}}
			f.plans[id] = p
		}
	}
}

func desiredSub(planKey string) DesiredSubscription {
	return DesiredSubscription{
		CustomerKey: "cust",
		PlanKey:     planKey,
		Metadata:    map[string]string{"miloapis.com/billing-entitlement": "ns/be"},
	}
}

func equalStrings(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}

// ---------------------------------------------------------------------------
// nextSubscriptionStep
// ---------------------------------------------------------------------------

func TestNextSubscriptionStep(t *testing.T) {
	now := time.Now()
	later := now.Add(30 * 24 * time.Hour)
	sub := func(id string, status om.SubscriptionStatus, key string, version int) om.Subscription {
		s := om.Subscription{Id: id, Status: status, ActiveFrom: now}
		if key != "" {
			s.Plan = &om.PlanReference{Key: key, Version: version}
		}
		if status == om.SubscriptionStatusScheduled {
			s.ActiveFrom = later
		}
		return s
	}

	tests := []struct {
		name          string
		subs          []om.Subscription
		activeVersion int
		wantKind      subscriptionStepKind
		wantTarget    string
		wantCurrent   string
		wantPending   string
	}{
		{
			name:          "no subscription creates",
			activeVersion: 1,
			wantKind:      stepCreate,
		},
		{
			name:          "ended subscriptions are history and ignored",
			subs:          []om.Subscription{sub("old", om.SubscriptionStatusInactive, "p", 1)},
			activeVersion: 1,
			wantKind:      stepCreate,
		},
		{
			name:          "active on the active version is converged",
			subs:          []om.Subscription{sub("s1", om.SubscriptionStatusActive, "p", 2)},
			activeVersion: 2,
			wantKind:      stepNone,
			wantCurrent:   "s1",
		},
		{
			name:          "active on an older version migrates",
			subs:          []om.Subscription{sub("s1", om.SubscriptionStatusActive, "p", 1)},
			activeVersion: 2,
			wantKind:      stepMigrate,
			wantTarget:    "s1",
		},
		{
			name:          "active on another plan changes",
			subs:          []om.Subscription{sub("s1", om.SubscriptionStatusActive, "q", 3)},
			activeVersion: 1,
			wantKind:      stepChange,
			wantTarget:    "s1",
		},
		{
			name:          "custom subscription without a plan changes",
			subs:          []om.Subscription{sub("s1", om.SubscriptionStatusActive, "", 0)},
			activeVersion: 1,
			wantKind:      stepChange,
			wantTarget:    "s1",
		},
		{
			name: "migration in flight is converged",
			subs: []om.Subscription{
				sub("next", om.SubscriptionStatusScheduled, "p", 2),
				sub("cur", om.SubscriptionStatusCanceled, "p", 1),
			},
			activeVersion: 2,
			wantKind:      stepNone,
			wantCurrent:   "cur",
			wantPending:   "next",
		},
		{
			name: "canceled with a successor on another plan restores",
			subs: []om.Subscription{
				sub("next", om.SubscriptionStatusScheduled, "q", 1),
				sub("cur", om.SubscriptionStatusCanceled, "p", 1),
			},
			activeVersion: 1,
			wantKind:      stepRestore,
			wantTarget:    "cur",
		},
		{
			name: "canceled with a stale migration restores",
			subs: []om.Subscription{
				sub("next", om.SubscriptionStatusScheduled, "p", 2),
				sub("cur", om.SubscriptionStatusCanceled, "p", 1),
			},
			activeVersion: 3,
			wantKind:      stepRestore,
			wantTarget:    "cur",
		},
		{
			name:          "canceled without a successor restores",
			subs:          []om.Subscription{sub("cur", om.SubscriptionStatusCanceled, "p", 1)},
			activeVersion: 1,
			wantKind:      stepRestore,
			wantTarget:    "cur",
		},
		{
			name:          "scheduled on the desired plan is converged",
			subs:          []om.Subscription{sub("next", om.SubscriptionStatusScheduled, "p", 1)},
			activeVersion: 1,
			wantKind:      stepNone,
			wantCurrent:   "next",
		},
		{
			name:          "scheduled on another plan is deleted",
			subs:          []om.Subscription{sub("next", om.SubscriptionStatusScheduled, "q", 1)},
			activeVersion: 1,
			wantKind:      stepDeleteScheduled,
			wantTarget:    "next",
		},
		{
			name: "stray scheduled beside an active subscription is deleted",
			subs: []om.Subscription{
				sub("stray", om.SubscriptionStatusScheduled, "p", 1),
				sub("cur", om.SubscriptionStatusActive, "p", 1),
			},
			activeVersion: 1,
			wantKind:      stepDeleteScheduled,
			wantTarget:    "stray",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := nextSubscriptionStep(tt.subs, "p", tt.activeVersion)
			if step.kind != tt.wantKind {
				t.Fatalf("kind = %s, want %s", step.kind, tt.wantKind)
			}
			if tt.wantTarget != "" && step.target.Id != tt.wantTarget {
				t.Errorf("target = %q, want %q", step.target.Id, tt.wantTarget)
			}
			if tt.wantCurrent != "" && (step.current == nil || step.current.Id != tt.wantCurrent) {
				t.Errorf("current = %+v, want %q", step.current, tt.wantCurrent)
			}
			gotPending := ""
			if step.pending != nil {
				gotPending = step.pending.Id
			}
			if gotPending != tt.wantPending {
				t.Errorf("pending = %q, want %q", gotPending, tt.wantPending)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// EnsureSubscription
// ---------------------------------------------------------------------------

func TestEnsureSubscription_CreatesThenConverges(t *testing.T) {
	c, f := newTestClient(t)
	f.seedCustomer("cust")
	f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)

	state, err := c.EnsureSubscription(context.Background(), desiredSub("p"))
	if err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}
	if state.Current.Status != om.SubscriptionStatusActive || state.Current.Plan.Key != "p" || state.PlanVersion != 1 {
		t.Fatalf("state = %+v", state)
	}
	if state.Current.Metadata == nil || (*state.Current.Metadata)["miloapis.com/billing-entitlement"] != "ns/be" {
		t.Errorf("metadata not attached: %+v", state.Current.Metadata)
	}

	writes := f.subscriptionWrites
	if _, err := c.EnsureSubscription(context.Background(), desiredSub("p")); err != nil {
		t.Fatalf("second EnsureSubscription: %v", err)
	}
	if f.subscriptionWrites != writes {
		t.Errorf("converged subscription issued %d more writes", f.subscriptionWrites-writes)
	}
}

func TestEnsureSubscription_BillingAnchor(t *testing.T) {
	anchor := time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)
	anchored := func(planKey string) DesiredSubscription {
		d := desiredSub(planKey)
		d.BillingAnchor = &anchor
		return d
	}

	t.Run("a new subscription bills from the anchor, and plan changes keep it", func(t *testing.T) {
		c, f := newTestClient(t)
		f.seedCustomer("cust")
		f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
		f.seedPlanVersion("q", 1, omv3.BillingPlanStatusActive)
		f.seedPlanVersion("r", 1, omv3.BillingPlanStatusActive)
		f.convertUsage("r")

		for _, step := range []struct{ planKey, via string }{
			{"p", "create"}, {"q", "v1 change"}, {"r", "v3 change"},
		} {
			state, err := c.EnsureSubscription(context.Background(), anchored(step.planKey))
			if err != nil {
				t.Fatalf("%s onto %s: %v", step.via, step.planKey, err)
			}
			if state.Current.Plan.Key != step.planKey || !state.Current.BillingAnchor.Equal(anchor) {
				t.Errorf("after %s: plan %s, anchor %s; want %s anchored at %s",
					step.via, state.Current.Plan.Key, state.Current.BillingAnchor, step.planKey, anchor)
			}
		}
		if f.v3Changes != 1 {
			t.Errorf("v3 changes = %d, want 1", f.v3Changes)
		}
	})

	t.Run("an existing subscription keeps the day it started", func(t *testing.T) {
		c, f := newTestClient(t)
		f.seedCustomer("cust")
		f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
		f.seedPlanVersion("q", 1, omv3.BillingPlanStatusActive)
		before, err := c.EnsureSubscription(context.Background(), desiredSub("p"))
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		started := before.Current.BillingAnchor

		writes := f.subscriptionWrites
		state, err := c.EnsureSubscription(context.Background(), anchored("p"))
		if err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		if f.subscriptionWrites != writes || !state.Current.BillingAnchor.Equal(started) {
			t.Errorf("writes = %d, anchor = %s; want none and %s", f.subscriptionWrites-writes, state.Current.BillingAnchor, started)
		}

		state, err = c.EnsureSubscription(context.Background(), anchored("q"))
		if err != nil {
			t.Fatalf("switch to q: %v", err)
		}
		if !state.Current.BillingAnchor.Equal(started) {
			t.Errorf("after a plan change anchor = %s, want %s", state.Current.BillingAnchor, started)
		}
	})
}

func TestEnsureSubscription_ChangesPlanImmediately(t *testing.T) {
	c, f := newTestClient(t)
	f.seedCustomer("cust")
	f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
	f.seedPlanVersion("q", 1, omv3.BillingPlanStatusActive)
	if _, err := c.EnsureSubscription(context.Background(), desiredSub("p")); err != nil {
		t.Fatalf("subscribe to p: %v", err)
	}

	state, err := c.EnsureSubscription(context.Background(), desiredSub("q"))
	if err != nil {
		t.Fatalf("switch to q: %v", err)
	}
	if state.Current.Plan.Key != "q" || state.Pending != nil {
		t.Fatalf("state = %+v", state)
	}
	if got, want := f.subsByStatus("cust"), []string{"inactive:p@1", "active:q@1"}; !equalStrings(got, want) {
		t.Errorf("subscriptions = %v, want %v", got, want)
	}
	if state.Current.Metadata == nil || (*state.Current.Metadata)["miloapis.com/billing-entitlement"] != "ns/be" {
		t.Errorf("change must carry metadata (OpenMeter does not copy it): %+v", state.Current.Metadata)
	}
}

func TestEnsureSubscription_MigratesNewVersionImmediately(t *testing.T) {
	c, f := newTestClient(t)
	f.seedCustomer("cust")
	f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
	if _, err := c.EnsureSubscription(context.Background(), desiredSub("p")); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	f.publishPlanVersion("p", 2)

	state, err := c.EnsureSubscription(context.Background(), desiredSub("p"))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if state.Current.Status != om.SubscriptionStatusActive || state.Current.Plan.Version != 2 || state.Pending != nil {
		t.Fatalf("state = %+v, want active v2 with nothing scheduled", state)
	}
	// Never a scheduled subscription: OpenMeter keeps counting a deleted one
	// as live, which blocks feature archival forever.
	if got, want := f.subsByStatus("cust"), []string{"inactive:p@1", "active:p@2"}; !equalStrings(got, want) {
		t.Errorf("subscriptions = %v, want %v", got, want)
	}
	if state.Current.Metadata == nil || (*state.Current.Metadata)["miloapis.com/billing-entitlement"] != "ns/be" {
		t.Errorf("migration lost the metadata: %+v", state.Current.Metadata)
	}

	writes := f.subscriptionWrites
	if _, err := c.EnsureSubscription(context.Background(), desiredSub("p")); err != nil {
		t.Fatalf("second EnsureSubscription: %v", err)
	}
	if f.subscriptionWrites != writes {
		t.Errorf("converged subscription issued %d more writes", f.subscriptionWrites-writes)
	}
}

// TestEnsureSubscription_UnitConversionGoesThroughV3: OpenMeter's v1 API
// refuses to change or migrate a subscription onto a plan that converts
// usage, so both go through v3 (which keeps no metadata). Creating one
// still goes through v1 and keeps it.
func TestEnsureSubscription_UnitConversionGoesThroughV3(t *testing.T) {
	t.Run("create keeps v1", func(t *testing.T) {
		c, f := newTestClient(t)
		f.seedCustomer("cust")
		f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
		f.convertUsage("p")
		state, err := c.EnsureSubscription(context.Background(), desiredSub("p"))
		if err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		if f.v3Changes != 0 || state.Current.Metadata == nil {
			t.Errorf("v3 changes = %d, metadata = %v; want a v1 create with metadata", f.v3Changes, state.Current.Metadata)
		}
	})

	t.Run("change onto a converting plan", func(t *testing.T) {
		c, f := newTestClient(t)
		f.seedCustomer("cust")
		f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
		f.seedPlanVersion("q", 1, omv3.BillingPlanStatusActive)
		f.convertUsage("q")
		if _, err := c.EnsureSubscription(context.Background(), desiredSub("p")); err != nil {
			t.Fatalf("subscribe to p: %v", err)
		}
		state, err := c.EnsureSubscription(context.Background(), desiredSub("q"))
		if err != nil {
			t.Fatalf("switch to q: %v", err)
		}
		if state.Current.Plan.Key != "q" || f.v3Changes != 1 {
			t.Fatalf("current = %s, v3 changes = %d; want q through v3", state.Current.Plan.Key, f.v3Changes)
		}
		if got, want := f.subsByStatus("cust"), []string{"inactive:p@1", "active:q@1"}; !equalStrings(got, want) {
			t.Errorf("subscriptions = %v, want %v", got, want)
		}
	})

	t.Run("migrate onto a converting version", func(t *testing.T) {
		c, f := newTestClient(t)
		f.seedCustomer("cust")
		f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
		if _, err := c.EnsureSubscription(context.Background(), desiredSub("p")); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		f.publishPlanVersion("p", 2)
		f.convertUsage("p")
		state, err := c.EnsureSubscription(context.Background(), desiredSub("p"))
		if err != nil {
			t.Fatalf("migrate: %v", err)
		}
		if state.Current.Plan.Version != 2 || f.v3Changes != 1 {
			t.Fatalf("current = v%d, v3 changes = %d; want v2 through v3", state.Current.Plan.Version, f.v3Changes)
		}
		if got, want := f.subsByStatus("cust"), []string{"inactive:p@1", "active:p@2"}; !equalStrings(got, want) {
			t.Errorf("subscriptions = %v, want %v", got, want)
		}

		writes := f.subscriptionWrites
		if _, err := c.EnsureSubscription(context.Background(), desiredSub("p")); err != nil {
			t.Fatalf("second EnsureSubscription: %v", err)
		}
		if f.subscriptionWrites != writes {
			t.Errorf("converged subscription issued %d more writes", f.subscriptionWrites-writes)
		}
	})
}

// scheduleExternalMigration reproduces a next-billing-cycle migration made in
// OpenMeter directly (this client never schedules one): the subscription is
// canceled at the end of the cycle with its successor scheduled then.
func scheduleExternalMigration(t *testing.T, f *fakeServer, subID string, version int) {
	t.Helper()
	sub := f.subscriptions[subID]
	var plan omv3.BillingPlan
	for _, p := range f.plans {
		if p.Key == sub.Plan.Key && p.Version == version {
			plan = p
		}
	}
	if plan.Id == "" {
		t.Fatalf("plan %s@%d not seeded", sub.Plan.Key, version)
	}
	f.endSubscription(&sub, true)
	f.newSubscription(sub.CustomerId, plan, om.SubscriptionStatusScheduled, *sub.ActiveTo, sub.Metadata)
}

func TestEnsureSubscription_ExternalPendingMigrationIsConverged(t *testing.T) {
	c, f := newTestClient(t)
	f.seedCustomer("cust")
	f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
	state, err := c.EnsureSubscription(context.Background(), desiredSub("p"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	f.publishPlanVersion("p", 2)
	scheduleExternalMigration(t, f, state.Current.Id, 2)

	writes := f.subscriptionWrites
	state, err = c.EnsureSubscription(context.Background(), desiredSub("p"))
	if err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}
	if state.Pending == nil || state.Pending.Plan.Version != 2 || state.Current.Status != om.SubscriptionStatusCanceled {
		t.Fatalf("state = %+v, want canceled v1 with v2 pending", state)
	}
	if f.subscriptionWrites != writes {
		t.Errorf("pending migration is converged but %d writes were issued", f.subscriptionWrites-writes)
	}
}

func TestEnsureSubscription_SwitchDuringPendingMigration(t *testing.T) {
	c, f := newTestClient(t)
	f.seedCustomer("cust")
	f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
	f.seedPlanVersion("q", 1, omv3.BillingPlanStatusActive)
	state, err := c.EnsureSubscription(context.Background(), desiredSub("p"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	f.publishPlanVersion("p", 2)
	scheduleExternalMigration(t, f, state.Current.Id, 2)

	state, err = c.EnsureSubscription(context.Background(), desiredSub("q"))
	if err != nil {
		t.Fatalf("switch to q: %v", err)
	}
	if state.Current.Plan.Key != "q" || state.Current.Status != om.SubscriptionStatusActive || state.Pending != nil {
		t.Fatalf("state = %+v", state)
	}
	// Restore dropped the scheduled v2, then change ended v1 now.
	if got, want := f.subsByStatus("cust"), []string{"inactive:p@1", "active:q@1"}; !equalStrings(got, want) {
		t.Errorf("subscriptions = %v, want %v", got, want)
	}
}

func TestEnsureSubscription_RestoresExternallyCanceled(t *testing.T) {
	c, f := newTestClient(t)
	f.seedCustomer("cust")
	f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
	state, err := c.EnsureSubscription(context.Background(), desiredSub("p"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	sub := f.subscriptions[state.Current.Id]
	f.endSubscription(&sub, true) // canceled at the end of the cycle

	state, err = c.EnsureSubscription(context.Background(), desiredSub("p"))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if state.Current.Status != om.SubscriptionStatusActive || state.Current.ActiveTo != nil {
		t.Errorf("current = %+v, want active and open-ended", state.Current)
	}
}

func TestEnsureSubscription_Errors(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*fakeServer)
		desired DesiredSubscription
		check   func(*testing.T, error)
	}{
		{
			name:    "missing customer",
			setup:   func(f *fakeServer) { f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive) },
			desired: desiredSub("p"),
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrCustomerNotFound) {
					t.Errorf("err = %v, want ErrCustomerNotFound", err)
				}
			},
		},
		{
			name: "plan only has a draft",
			setup: func(f *fakeServer) {
				f.seedCustomer("cust")
				f.seedPlanVersion("p", 1, omv3.BillingPlanStatusDraft)
			},
			desired: desiredSub("p"),
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrPlanNotPublished) {
					t.Errorf("err = %v, want ErrPlanNotPublished", err)
				}
			},
		},
		{
			name: "customer cannot be invoiced",
			setup: func(f *fakeServer) {
				f.seedCustomer("cust")
				f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
				f.billingNotReady = true
			},
			desired: desiredSub("p"),
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrCustomerBillingNotReady) || !IsPermanent(err) {
					t.Errorf("err = %v, want a permanent ErrCustomerBillingNotReady", err)
				}
			},
		},
		{
			name:    "empty keys",
			desired: DesiredSubscription{},
			check: func(t *testing.T, err error) {
				if !IsPermanent(err) {
					t.Errorf("err = %v, want permanent", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, f := newTestClient(t)
			if tt.setup != nil {
				tt.setup(f)
			}
			_, err := c.EnsureSubscription(context.Background(), tt.desired)
			if err == nil {
				t.Fatal("expected an error")
			}
			tt.check(t, err)
		})
	}
}

// ---------------------------------------------------------------------------
// CancelSubscriptions
// ---------------------------------------------------------------------------

func TestCancelSubscriptions(t *testing.T) {
	t.Run("active is canceled immediately", func(t *testing.T) {
		c, f := newTestClient(t)
		f.seedCustomer("cust")
		f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
		if _, err := c.EnsureSubscription(context.Background(), desiredSub("p")); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		if err := c.CancelSubscriptions(context.Background(), "cust"); err != nil {
			t.Fatalf("CancelSubscriptions: %v", err)
		}
		if got, want := f.subsByStatus("cust"), []string{"inactive:p@1"}; !equalStrings(got, want) {
			t.Errorf("subscriptions = %v, want %v", got, want)
		}
	})

	t.Run("pending migration is ended entirely", func(t *testing.T) {
		c, f := newTestClient(t)
		f.seedCustomer("cust")
		f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
		state, err := c.EnsureSubscription(context.Background(), desiredSub("p"))
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		f.publishPlanVersion("p", 2)
		scheduleExternalMigration(t, f, state.Current.Id, 2)
		if err := c.CancelSubscriptions(context.Background(), "cust"); err != nil {
			t.Fatalf("CancelSubscriptions: %v", err)
		}
		// The scheduled successor is deleted; the canceled one is continued
		// and canceled now, since it cannot be canceled twice.
		if got, want := f.subsByStatus("cust"), []string{"inactive:p@1"}; !equalStrings(got, want) {
			t.Errorf("subscriptions = %v, want %v", got, want)
		}
	})

	t.Run("nothing live is a no-op", func(t *testing.T) {
		c, f := newTestClient(t)
		f.seedCustomer("cust")
		if err := c.CancelSubscriptions(context.Background(), "cust"); err != nil {
			t.Fatalf("CancelSubscriptions: %v", err)
		}
		if f.subscriptionWrites != 0 {
			t.Errorf("issued %d writes", f.subscriptionWrites)
		}
	})

	t.Run("missing customer is success", func(t *testing.T) {
		c, _ := newTestClient(t)
		if err := c.CancelSubscriptions(context.Background(), "gone"); err != nil {
			t.Fatalf("CancelSubscriptions: %v", err)
		}
	})

	t.Run("server error is transient", func(t *testing.T) {
		c, f := newTestClient(t)
		f.statusOverride = http.StatusServiceUnavailable
		if err := c.CancelSubscriptions(context.Background(), "cust"); !IsTransient(err) {
			t.Errorf("err = %v, want transient", err)
		}
	})
}

// ---------------------------------------------------------------------------
// classifySubscription
// ---------------------------------------------------------------------------

func TestClassifySubscription(t *testing.T) {
	resp := func(status int) *http.Response { return &http.Response{StatusCode: status, Header: http.Header{}} }

	if err := classifySubscription(resp(http.StatusOK), nil); err != nil {
		t.Errorf("200: %v", err)
	}

	err := classifySubscription(resp(http.StatusForbidden), []byte(`{"detail":"transition cancel in state canceled not allowed"}`))
	if !IsTransient(err) || strings.Contains(err.Error(), "authentication") {
		t.Errorf("403 = %v, want a transient state-transition error, not an auth error", err)
	}

	err = classifySubscription(resp(http.StatusConflict), []byte(`{"detail":"conflict error: invalid billing setup: no stripe data"}`))
	if !errors.Is(err, ErrCustomerBillingNotReady) || !IsPermanent(err) {
		t.Errorf("409 billing = %v, want permanent ErrCustomerBillingNotReady", err)
	}

	err = classifySubscription(resp(http.StatusConflict), []byte(`{"detail":"only_single_subscription_allowed_per_customer_at_a_time"}`))
	if errors.Is(err, ErrCustomerBillingNotReady) || !IsPermanent(err) {
		t.Errorf("409 overlap = %v, want a plain permanent error", err)
	}
}
