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
//     only the v3 change route accepts. It keeps no metadata.
//   - a change is immediate or timed at a custom time, which may be up to
//     two minutes in the past; the new subscription starts then.
//   - a subscription bills from the anchor it was created with, else from
//     its start; a change bills from the anchor it carries, else migrate and
//     v1 change keep the previous one, while a v3 change bills from the
//     change. Anchors are not normalized.
//   - GET /subscriptions/{id} reports the period seeded in billingPeriods,
//     and refuses an "at" time with 501, as OpenMeter does.
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
		at, err := v3ChangeTime(body.Timing)
		if err != nil {
			problem(http.StatusBadRequest, err.Error())
			return true
		}
		plan, ok := f.planVersion(*body.Plan.Key, *body.Plan.Version)
		if !ok || plan.Status != omv3.BillingPlanStatusActive {
			problem(http.StatusBadRequest, "plan is not active")
			return true
		}
		f.v3Changes++
		next := f.changeSubscription(&sub, plan, at, nil)
		if body.BillingAnchor != nil {
			f.setAnchor(next.Id, *body.BillingAnchor)
		}
		writeJSON(http.StatusOK, map[string]any{"current": map[string]string{"id": sub.Id}, "next": map[string]string{"id": next.Id}})
		return true
	}

	// GET /api/v3/openmeter/subscriptions/{id}: the v3 read, with the
	// current billing period and the current phase's items.
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v3/openmeter/subscriptions/") &&
		!strings.Contains(strings.TrimPrefix(r.URL.Path, "/api/v3/openmeter/subscriptions/"), "/") {
		id := strings.TrimPrefix(r.URL.Path, "/api/v3/openmeter/subscriptions/")
		sub, ok := f.subscriptions[id]
		if !ok {
			problem(http.StatusNotFound, "subscription not found")
			return true
		}
		out := omv3.BillingSubscription{Id: sub.Id, ActiveFrom: sub.ActiveFrom, BillingAnchor: sub.BillingAnchor}
		if period, ok := f.billingPeriods[sub.Id]; ok {
			out.CurrentPeriod = &omv3.ClosedPeriod{From: period.From, To: period.To}
		}
		if items, ok := f.subItems[sub.Id]; ok {
			phase := omv3.BillingSubscriptionPhase{Key: "default_phase", ActiveFrom: sub.ActiveFrom}
			for _, item := range items {
				phase.Items = append(phase.Items, omv3.BillingSubscriptionItem{
					ActiveTo: item.ActiveTo, RateCard: omv3.BillingRateCard{Key: item.Key},
				})
			}
			out.Phases = []omv3.BillingSubscriptionPhase{phase}
		}
		writeJSON(http.StatusOK, out)
		return true
	}

	if strings.HasPrefix(r.URL.Path, "/api/v3/openmeter/subscriptions/") && strings.HasSuffix(r.URL.Path, "/edit") {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v3/openmeter/subscriptions/"), "/edit")
		sub, ok := f.subscriptions[id]
		if !ok {
			problem(http.StatusNotFound, "subscription not found")
			return true
		}
		var body omv3.BillingSubscriptionEdit
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Timing == nil {
			problem(http.StatusBadRequest, "bad body")
			return true
		}
		timing, err := body.Timing.AsBillingSubscriptionEditTimingEnum()
		if err != nil {
			problem(http.StatusBadRequest, "bad timing")
			return true
		}
		edit := fakeFeeEdit{subscription: id, timing: string(timing)}
		for _, op := range body.Customizations {
			remove, err := op.AsBillingSubscriptionEditRemoveItem()
			if err != nil || remove.Type != omv3.BillingSubscriptionEditRemoveItemTypeRemoveItem {
				problem(http.StatusBadRequest, "only remove_item is faked")
				return true
			}
			edit.keys = append(edit.keys, remove.PhaseKey+"/"+remove.ItemKey)
			items := f.subItems[id]
			for i := range items {
				if items[i].Key != remove.ItemKey {
					continue
				}
				if timing == omv3.BillingSubscriptionEditTimingEnumImmediate {
					items = append(items[:i], items[i+1:]...)
				} else {
					end := f.billingPeriods[id].To
					items[i].ActiveTo = &end
				}
				break
			}
			f.subItems[id] = items
		}
		f.feeEdits = append(f.feeEdits, edit)
		f.subscriptionWrites++
		writeJSON(http.StatusOK, map[string]string{"id": sub.Id})
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
		from := time.Now()
		if body.Timing != nil {
			at, err := v1ChangeTime(*body.Timing)
			if err != nil {
				problem(http.StatusBadRequest, err.Error())
				return true
			}
			if !at.IsZero() {
				from = at
			}
		}
		sub := f.newSubscription(cust.Id, plan, om.SubscriptionStatusActive, from, body.Metadata)
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
	case r.Method == http.MethodGet && action == "":
		// OpenMeter refuses the v1 read for any subscription with a
		// unit-converting item, as a contract with a customer's own
		// unit-converted rates has; the client reads through v3 instead.
		problem(http.StatusBadRequest, "unit_config_not_representable: this resource uses unit_config and is only available via the v3 API")
		return true

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
		at, err := v1ChangeTime(body.Timing)
		if err != nil {
			problem(http.StatusBadRequest, err.Error())
			return true
		}
		// OpenMeter only ends a subscription at a custom time on one of its
		// billing boundaries.
		if period, ok := f.billingPeriods[sub.Id]; !at.IsZero() && (!ok || (!at.Equal(period.From) && !at.Equal(period.To))) {
			problem(http.StatusBadRequest, "invalid cancelation timing: cannot cancel aligned subscription with custom misaligned timing")
			return true
		}
		anchor := sub.BillingAnchor
		next := f.changeSubscription(&sub, plan, at, body.Metadata)
		if body.BillingAnchor != nil {
			anchor = *body.BillingAnchor
		}
		f.setAnchor(next.Id, anchor)
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

// changeSubscription ends sub at (now when zero) and starts its successor
// on plan then, recording a custom time in changesAt.
func (f *fakeServer) changeSubscription(sub *om.Subscription, plan omv3.BillingPlan, at time.Time, metadata *om.Metadata) om.Subscription {
	f.endSubscription(sub, false)
	start := time.Now()
	if !at.IsZero() {
		f.changesAt[sub.Id] = at
		sub.ActiveTo = &at
		f.subscriptions[sub.Id] = *sub
		start = at
	}
	return f.newSubscription(sub.CustomerId, plan, om.SubscriptionStatusActive, start, metadata)
}

// v1ChangeTime is a v1 change's custom time, zero for immediate.
func v1ChangeTime(t om.SubscriptionTiming) (time.Time, error) {
	if e, err := t.AsSubscriptionTimingEnum(); err == nil && e == om.SubscriptionTimingEnumImmediate {
		return time.Time{}, nil
	}
	at, err := t.AsSubscriptionTiming1()
	if err != nil {
		return time.Time{}, errors.New("only immediate or custom timing is emulated")
	}
	return at, checkChangeTime(at)
}

// v3ChangeTime is a v3 change's custom time, zero for immediate.
func v3ChangeTime(t omv3.BillingSubscriptionEditTiming) (time.Time, error) {
	if e, err := t.AsBillingSubscriptionEditTimingEnum(); err == nil && e == omv3.BillingSubscriptionEditTimingEnumImmediate {
		return time.Time{}, nil
	}
	at, err := t.AsDateTime()
	if err != nil {
		return time.Time{}, errors.New("only immediate or custom timing is emulated")
	}
	return at, checkChangeTime(at)
}

// checkChangeTime refuses to start the next subscription more than two
// minutes in the past, as OpenMeter does.
func checkChangeTime(at time.Time) error {
	if at.Before(time.Now().Add(-2 * time.Minute)) {
		return errors.New("cannot create subscription in the past")
	}
	return nil
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
	if !state.Changed {
		t.Error("Changed = false after creating the subscription")
	}

	writes := f.subscriptionWrites
	again, err := c.EnsureSubscription(context.Background(), desiredSub("p"))
	if err != nil {
		t.Fatalf("second EnsureSubscription: %v", err)
	}
	if f.subscriptionWrites != writes {
		t.Errorf("converged subscription issued %d more writes", f.subscriptionWrites-writes)
	}
	if again.Changed {
		t.Error("Changed = true for a subscription already converged")
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
	if state.Current.Plan.Key != "q" || state.Pending != nil || !state.Changed {
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

// TestEnsureSubscription_MoveAnchor: an account moving onto a contract
// bills on the contract's day from the change, through v1 and v3 alike.
func TestEnsureSubscription_MoveAnchor(t *testing.T) {
	contractDay := time.Date(2026, time.December, 18, 0, 0, 0, 0, time.UTC)
	moving := func(planKey string) DesiredSubscription {
		d := desiredSub(planKey)
		d.BillingAnchor = &contractDay
		d.MoveAnchor = true
		return d
	}

	for _, tc := range []struct{ name, planKey string }{{"v1 change", "q"}, {"v3 change", "r"}} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := newTestClient(t)
			f.seedCustomer("cust")
			f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
			f.seedPlanVersion(tc.planKey, 1, omv3.BillingPlanStatusActive)
			if tc.planKey == "r" {
				f.convertUsage("r")
			}
			if _, err := c.EnsureSubscription(context.Background(), desiredSub("p")); err != nil {
				t.Fatalf("subscribe: %v", err)
			}

			state, err := c.EnsureSubscription(context.Background(), moving(tc.planKey))
			if err != nil {
				t.Fatalf("move onto %s: %v", tc.planKey, err)
			}
			if state.Current.Plan.Key != tc.planKey || !state.Current.BillingAnchor.Equal(contractDay) {
				t.Errorf("plan %s, anchor %s; want %s anchored at %s", state.Current.Plan.Key, state.Current.BillingAnchor, tc.planKey, contractDay)
			}
		})
	}
}

// A contract's subscription starts at the contract's start when the
// provider gets to it within periodChangeTolerance, so OpenMeter prorates
// nothing in its first period: created at it, or changed onto at it when
// it's a billing boundary of the subscription changed. Otherwise it starts
// now.
func TestEnsureSubscription_StartsAtTheContractStart(t *testing.T) {
	startsAt := time.Now().Add(-30 * time.Second).Truncate(time.Second)
	starting := func(planKey string, startsAt time.Time) DesiredSubscription {
		d := desiredSub(planKey)
		d.BillingAnchor = &startsAt
		d.MoveAnchor = true
		d.StartsAt = &startsAt
		d.Now = time.Now()
		return d
	}
	startedAt := func(sub om.Subscription) time.Time { return sub.ActiveFrom.Truncate(time.Second) }

	t.Run("created at the start", func(t *testing.T) {
		c, f := newTestClient(t)
		f.seedCustomer("cust")
		f.seedPlanVersion("q", 1, omv3.BillingPlanStatusActive)
		state, err := c.EnsureSubscription(context.Background(), starting("q", startsAt))
		if err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		if !startedAt(state.Current).Equal(startsAt) {
			t.Errorf("started %s, want the contract's start %s", state.Current.ActiveFrom, startsAt)
		}
	})

	t.Run("created now when the start is past the tolerance", func(t *testing.T) {
		c, f := newTestClient(t)
		f.seedCustomer("cust")
		f.seedPlanVersion("q", 1, omv3.BillingPlanStatusActive)
		early := startsAt.Add(-time.Hour)
		state, err := c.EnsureSubscription(context.Background(), starting("q", early))
		if err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		if !state.Current.ActiveFrom.After(startsAt) {
			t.Errorf("started %s, want now", state.Current.ActiveFrom)
		}
	})

	for _, tc := range []struct{ name, planKey string }{{"v1 change", "q"}, {"v3 change", "r"}} {
		t.Run(tc.name+" at the start, a billing boundary", func(t *testing.T) {
			c, f := newTestClient(t)
			f.seedCustomer("cust")
			f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
			f.seedPlanVersion(tc.planKey, 1, omv3.BillingPlanStatusActive)
			if tc.planKey == "r" {
				f.convertUsage("r")
			}
			before, err := c.EnsureSubscription(context.Background(), desiredSub("p"))
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			old := before.Current
			old.ActiveFrom = startsAt.AddDate(0, -1, 0)
			f.subscriptions[old.Id] = old
			f.billingPeriods[old.Id] = om.Period{From: startsAt, To: startsAt.AddDate(0, 1, 0)}

			state, err := c.EnsureSubscription(context.Background(), starting(tc.planKey, startsAt))
			if err != nil {
				t.Fatalf("move onto %s: %v", tc.planKey, err)
			}
			if at, ok := f.changesAt[old.Id]; !ok || !at.Equal(startsAt) || !startedAt(state.Current).Equal(startsAt) {
				t.Errorf("changed at %s (timed %v), started %s; want both at %s", at, ok, state.Current.ActiveFrom, startsAt)
			}
		})
	}

	t.Run("changed now when the start isn't a billing boundary", func(t *testing.T) {
		c, f := newTestClient(t)
		f.seedCustomer("cust")
		f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
		f.seedPlanVersion("q", 1, omv3.BillingPlanStatusActive)
		before, err := c.EnsureSubscription(context.Background(), desiredSub("p"))
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		old := before.Current
		old.ActiveFrom = startsAt.AddDate(0, 0, -10)
		f.subscriptions[old.Id] = old
		f.billingPeriods[old.Id] = om.Period{From: startsAt.AddDate(0, 0, -10), To: startsAt.AddDate(0, 0, 20)}

		state, err := c.EnsureSubscription(context.Background(), starting("q", startsAt))
		if err != nil {
			t.Fatalf("move onto q: %v", err)
		}
		if _, timed := f.changesAt[old.Id]; timed || state.Current.Plan.Key != "q" {
			t.Errorf("plan %s, timed change %v; want an immediate change onto q", state.Current.Plan.Key, timed)
		}
	})
}

func TestPeriodChangeTime(t *testing.T) {
	started := time.Date(2026, time.November, 18, 9, 30, 0, 0, time.UTC)
	boundary := time.Date(2026, time.December, 18, 0, 0, 0, 0, time.UTC)
	next := boundary.AddDate(0, 1, 0)
	sub := om.Subscription{ActiveFrom: started}

	tests := []struct {
		name     string
		period   *om.Period
		now      time.Time
		due      *time.Time
		wantAt   time.Time
		wantWait *time.Time
	}{
		{name: "no period changes now", now: boundary},
		{name: "first period waits for its end",
			period: &om.Period{From: started, To: boundary}, now: started.Add(time.Minute), wantWait: &boundary},
		{name: "mid period waits for its end",
			period: &om.Period{From: boundary, To: next}, now: boundary.AddDate(0, 0, 10), wantWait: &next},
		{name: "just after a boundary is timed at it",
			period: &om.Period{From: boundary, To: next}, now: boundary.Add(5 * time.Second), wantAt: boundary},
		{name: "at the tolerance is timed at it",
			period: &om.Period{From: boundary, To: next}, now: boundary.Add(periodChangeTolerance), wantAt: boundary},
		{name: "past the tolerance waits for the next one",
			period: &om.Period{From: boundary, To: next}, now: boundary.Add(periodChangeTolerance + time.Second), wantWait: &next},
		{name: "a missed boundary changes now",
			period: &om.Period{From: boundary, To: next}, now: boundary.Add(10 * time.Minute), due: &boundary},
		{name: "not yet due waits",
			period: &om.Period{From: started, To: boundary}, now: boundary.Add(-time.Hour), due: &boundary, wantWait: &boundary},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at, wait := periodChangeTime(sub, tt.period, tt.now, tt.due, 1)
			if !at.Equal(tt.wantAt) {
				t.Errorf("at = %s, want %s", at, tt.wantAt)
			}
			switch {
			case (wait == nil) != (tt.wantWait == nil):
				t.Errorf("wait = %v, want %v", wait, tt.wantWait)
			case wait != nil && !wait.Equal(*tt.wantWait):
				t.Errorf("wait = %s, want %s", wait, tt.wantWait)
			}
		})
	}
}

// A plan billing a quarterly or annual fee in advance changes only at the
// end of that fee's period, counted from the billing anchor, and never late:
// changing mid-period would charge the rest of the period again.
func TestPeriodChangeTime_InAdvanceFeePeriod(t *testing.T) {
	anchor := time.Date(2026, time.April, 18, 0, 0, 0, 0, time.UTC)
	sub := om.Subscription{ActiveFrom: anchor, BillingAnchor: anchor}
	month := func(m int) time.Time { return anchor.AddDate(0, m, 0) }
	period := func(m int) *om.Period { return &om.Period{From: month(m), To: month(m + 1)} }

	tests := []struct {
		name     string
		every    int
		period   *om.Period
		now      time.Time
		due      *time.Time
		wantAt   time.Time
		wantWait time.Time
	}{
		{name: "annual: mid year waits for the anniversary",
			every: 12, period: period(4), now: month(4).AddDate(0, 0, 3), wantWait: month(12)},
		{name: "annual: a monthly boundary that isn't the anniversary waits",
			every: 12, period: period(5), now: month(5).Add(5 * time.Second), wantWait: month(12)},
		{name: "annual: just after the anniversary is timed at it",
			every: 12, period: period(12), now: month(12).Add(5 * time.Second), wantAt: month(12)},
		{name: "annual: a missed anniversary waits a year, even past due",
			every: 12, period: period(12), now: month(12).Add(10 * time.Minute), due: ptr(month(12)), wantWait: month(24)},
		{name: "quarterly: waits for the quarter's end",
			every: 3, period: period(4), now: month(4).AddDate(0, 0, 3), wantWait: month(6)},
		{name: "quarterly: at a quarter boundary",
			every: 3, period: period(6), now: month(6).Add(time.Second), wantAt: month(6)},
		{name: "first period of an annual plan waits for the anniversary",
			every: 12, period: period(0), now: anchor.Add(time.Hour), wantWait: month(12)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at, wait := periodChangeTime(sub, tt.period, tt.now, tt.due, tt.every)
			if !at.Equal(tt.wantAt) {
				t.Errorf("at = %s, want %s", at, tt.wantAt)
			}
			switch {
			case tt.wantWait.IsZero() && wait != nil:
				t.Errorf("wait = %s, want none", wait)
			case !tt.wantWait.IsZero() && (wait == nil || !wait.Equal(tt.wantWait)):
				t.Errorf("wait = %v, want %s", wait, tt.wantWait)
			}
		})
	}
}

// Boundaries are counted on the anchor's day, or the month's last day.
func TestAddMonthsClamped(t *testing.T) {
	anchor := time.Date(2026, time.January, 31, 0, 0, 0, 0, time.UTC)
	for months, want := range map[int]time.Time{
		1:  time.Date(2026, time.February, 28, 0, 0, 0, 0, time.UTC),
		3:  time.Date(2026, time.April, 30, 0, 0, 0, 0, time.UTC),
		12: time.Date(2027, time.January, 31, 0, 0, 0, 0, time.UTC),
	} {
		if got := addMonthsClamped(anchor, months); !got.Equal(want) {
			t.Errorf("addMonthsClamped(+%d) = %s, want %s", months, got, want)
		}
	}
}

func TestPlanInAdvanceFeeMonths(t *testing.T) {
	card := func(cadence string, term omv3.BillingPricePaymentTerm) omv3.BillingRateCard {
		c := omv3.BillingRateCard{PaymentTerm: &term}
		if cadence != "" {
			c.BillingCadence = &cadence
		}
		return c
	}
	inAdvance, inArrears := omv3.BillingPricePaymentTermInAdvance, omv3.BillingPricePaymentTermInArrears
	plan := func(cards ...omv3.BillingRateCard) omv3.BillingPlan {
		return omv3.BillingPlan{Phases: []omv3.BillingPlanPhase{{RateCards: cards}}}
	}
	tests := []struct {
		name string
		plan omv3.BillingPlan
		want int
	}{
		{"monthly fees", plan(card("P1M", inAdvance), card("P1M", inArrears)), 1},
		{"one-time fee", plan(card("", inAdvance)), 1},
		{"annual in arrears", plan(card("P1Y", inArrears)), 1},
		{"quarterly in advance", plan(card("P1M", inAdvance), card("P3M", inAdvance)), 3},
		{"annual in advance", plan(card("P3M", inAdvance), card("P1Y", inAdvance)), 12},
	}
	for _, tt := range tests {
		if got := planInAdvanceFeeMonths(tt.plan); got != tt.want {
			t.Errorf("%s: %d, want %d", tt.name, got, tt.want)
		}
	}
}

// TestEnsureSubscription_ChangeAtPeriodEnd: an amended contract keeps its
// plan until the billing period ends, then changes timed at the boundary,
// keeping its billing day.
func TestEnsureSubscription_ChangeAtPeriodEnd(t *testing.T) {
	c, f := newTestClient(t)
	f.seedCustomer("cust")
	f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
	f.seedPlanVersion("q", 1, omv3.BillingPlanStatusActive)
	before, err := c.EnsureSubscription(context.Background(), desiredSub("p"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	sub := before.Current
	amend := desiredSub("q")
	amend.ChangeAtPeriodEnd = true

	// Mid period: nothing changes until it ends.
	now := time.Now()
	periodEnd := now.Add(10 * 24 * time.Hour)
	f.billingPeriods[sub.Id] = om.Period{From: sub.ActiveFrom, To: periodEnd}
	writes := f.subscriptionWrites
	amend.Now = now
	state, err := c.EnsureSubscription(context.Background(), amend)
	if err != nil {
		t.Fatalf("amend mid period: %v", err)
	}
	if f.subscriptionWrites != writes || state.Current.Id != sub.Id || state.ChangeAt == nil || !state.ChangeAt.Equal(periodEnd) || state.Changed {
		t.Fatalf("writes = %d, state = %+v; want none, still %s, changing at %s", f.subscriptionWrites-writes, state, sub.Id, periodEnd)
	}

	// Just after the next period starts: the change is timed at its start.
	boundary := sub.ActiveFrom.Add(time.Second)
	f.billingPeriods[sub.Id] = om.Period{From: boundary, To: boundary.AddDate(0, 1, 0)}
	amend.Now = boundary.Add(20 * time.Second)
	state, err = c.EnsureSubscription(context.Background(), amend)
	if err != nil {
		t.Fatalf("amend at the boundary: %v", err)
	}
	if state.Current.Plan.Key != "q" || state.ChangeAt != nil || !state.Changed {
		t.Fatalf("state = %+v, want changed to q with nothing pending", state)
	}
	if at := f.changesAt[sub.Id]; !at.Equal(boundary) {
		t.Errorf("change timed at %s, want %s", at, boundary)
	}
	if !state.Current.BillingAnchor.Equal(sub.BillingAnchor) {
		t.Errorf("anchor = %s, want the contract's %s", state.Current.BillingAnchor, sub.BillingAnchor)
	}

	// A new version of the same plan still migrates straight away.
	f.publishPlanVersion("q", 2)
	state, err = c.EnsureSubscription(context.Background(), amend)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if state.Current.Plan.Version != 2 || state.ChangeAt != nil {
		t.Errorf("state = %+v, want migrated to v2 now", state)
	}
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

// fakeFeeEdit is one item-removal edit EndFees sent.
type fakeFeeEdit struct {
	subscription string
	timing       string
	// keys are "phase/item".
	keys []string
}

func TestEndFees(t *testing.T) {
	anchor := time.Date(2026, time.November, 12, 0, 0, 0, 0, time.UTC)
	month := func(m int) time.Time { return anchor.AddDate(0, m, 0) }
	setup := func(t *testing.T, period int) (Client, *fakeServer, string) {
		t.Helper()
		c, f := newTestClient(t)
		f.seedCustomer("cust")
		f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
		state, err := c.EnsureSubscription(context.Background(), desiredSub("p"))
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		id := state.Current.Id
		f.billingPeriods[id] = om.Period{From: month(period), To: month(period + 1)}
		f.subItems[id] = []om.SubscriptionItem{{Key: "promo_fee"}, {Key: "rack_space"}}
		return c, f, id
	}
	// A fee charged for three months from 12 November ends on 12 February.
	promo := FeeEnd{ItemKey: "promo_fee", EndsAt: month(3)}

	t.Run("before its last period, nothing yet", func(t *testing.T) {
		c, f, id := setup(t, 1)
		res, err := c.EndFees(context.Background(), id, []FeeEnd{promo}, month(1).Add(time.Hour))
		if err != nil {
			t.Fatalf("EndFees: %v", err)
		}
		if len(f.feeEdits) != 0 || res.NextCheck == nil || !res.NextCheck.Equal(month(2)) {
			t.Fatalf("edits = %v, next check = %v; want none, and to look again at %s", f.feeEdits, res.NextCheck, month(2))
		}
	})

	t.Run("in its last period, removed at the next billing cycle, once", func(t *testing.T) {
		c, f, id := setup(t, 2)
		res, err := c.EndFees(context.Background(), id, []FeeEnd{promo}, month(2).Add(time.Hour))
		if err != nil {
			t.Fatalf("EndFees: %v", err)
		}
		if len(res.Scheduled) != 1 || len(f.feeEdits) != 1 || f.feeEdits[0].timing != "next_billing_cycle" ||
			f.feeEdits[0].keys[0] != "default_phase/promo_fee" {
			t.Fatalf("result = %+v, edits = %+v", res, f.feeEdits)
		}
		// Scheduled already: the next pass sends nothing.
		if _, err := c.EndFees(context.Background(), id, []FeeEnd{promo}, month(2).Add(2*time.Hour)); err != nil {
			t.Fatalf("EndFees again: %v", err)
		}
		if len(f.feeEdits) != 1 {
			t.Errorf("sent %d edits, want 1", len(f.feeEdits))
		}
	})

	t.Run("still charging after its end, removed now and reported", func(t *testing.T) {
		c, f, id := setup(t, 4)
		res, err := c.EndFees(context.Background(), id, []FeeEnd{promo}, month(4).Add(time.Hour))
		if err != nil {
			t.Fatalf("EndFees: %v", err)
		}
		if len(res.RemovedLate) != 1 || len(f.feeEdits) != 1 || f.feeEdits[0].timing != "immediate" {
			t.Fatalf("result = %+v, edits = %+v", res, f.feeEdits)
		}
		if len(f.subItems[id]) != 1 || f.subItems[id][0].Key != "rack_space" {
			t.Errorf("items = %+v, want only rack_space", f.subItems[id])
		}
	})

	t.Run("already gone, nothing to do", func(t *testing.T) {
		c, f, id := setup(t, 4)
		f.subItems[id] = []om.SubscriptionItem{{Key: "rack_space"}}
		res, err := c.EndFees(context.Background(), id, []FeeEnd{promo}, month(4).Add(time.Hour))
		if err != nil || len(f.feeEdits) != 0 || len(res.RemovedLate)+len(res.Scheduled) != 0 {
			t.Fatalf("err = %v, result = %+v, edits = %+v", err, res, f.feeEdits)
		}
	})
}
