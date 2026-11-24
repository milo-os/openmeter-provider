// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alpacahq/alpacadecimal"
	om "github.com/openmeterio/openmeter/api/client/go"
	omv3 "github.com/openmeterio/openmeter/api/v3"
	"github.com/openmeterio/openmeter/openmeter/productcatalog"
)

// fakeUsage is one metered amount, for serveMeterQuery.
type fakeUsage struct {
	slug       string
	customerID string
	at         time.Time
	value      float64
}

// serveMeterQuery sums the seeded usage of the meter in the query's range,
// per customer when it filters by customers. Group-by filters are ignored.
func (f *fakeServer) serveMeterQuery(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/meters/"), "/query")
	var body om.MeterQueryRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.From == nil || body.To == nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if _, ok := f.meters[slug]; !ok {
		writeMeterNotFound(w, slug)
		return
	}
	f.meterQueries++
	totals := map[string]float64{}
	for _, u := range f.usage {
		if u.slug != slug || u.at.Before(*body.From) || !u.at.Before(*body.To) {
			continue
		}
		if body.FilterCustomerId != nil && !slices.Contains(*body.FilterCustomerId, u.customerID) {
			continue
		}
		totals[u.customerID] += u.value
	}
	var rows []om.MeterQueryRow
	for id, v := range totals {
		id := id
		rows = append(rows, om.MeterQueryRow{CustomerId: &id, Value: v, WindowStart: *body.From, WindowEnd: *body.To})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(om.MeterQueryResult{Data: rows})
}

func bridgeTieredPrice(t *testing.T, tiers ...omv3.BillingPriceTier) omv3.BillingPrice {
	t.Helper()
	var p omv3.BillingPrice
	if err := p.FromBillingPriceGraduated(omv3.BillingPriceGraduated{Type: omv3.BillingPriceGraduatedTypeGraduated, Tiers: tiers}); err != nil {
		t.Fatal(err)
	}
	return p
}

func tier(upTo string, unit string, flat string) omv3.BillingPriceTier {
	var out omv3.BillingPriceTier
	if upTo != "" {
		out.UpToAmount = &upTo
	}
	if unit != "" {
		out.UnitPrice = &omv3.BillingPriceUnit{Amount: unit, Type: omv3.BillingPriceUnitTypeUnit}
	}
	if flat != "" {
		out.FlatPrice = &omv3.BillingPriceFlat{Amount: flat, Type: omv3.BillingPriceFlatTypeFlat}
	}
	return out
}

func tierStrings(t *testing.T, p omv3.BillingPrice) []string {
	t.Helper()
	g, err := p.AsBillingPriceGraduated()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, tr := range g.Tiers {
		s := "inf"
		if tr.UpToAmount != nil {
			s = *tr.UpToAmount
		}
		if tr.UnitPrice != nil {
			s += " @" + tr.UnitPrice.Amount
		}
		if tr.FlatPrice != nil {
			s += " +" + tr.FlatPrice.Amount
		}
		out = append(out, s)
	}
	return out
}

func TestContinueGraduatedTiers(t *testing.T) {
	egress := bridgeTieredPrice(t, tier("200", "0", ""), tier("1000", "0.05", ""), tier("", "0.03", ""))
	tests := []struct {
		name  string
		used  string
		want  []string
		moved bool
	}{
		{"no usage", "0", []string{"200 @0", "1000 @0.05", "inf @0.03"}, false},
		{"inside the free tier", "150", []string{"50 @0", "850 @0.05", "inf @0.03"}, true},
		{"free tier used up exactly", "200", []string{"800 @0.05", "inf @0.03"}, true},
		{"into the second tier", "450.5", []string{"549.5 @0.05", "inf @0.03"}, true},
		{"past every bounded tier", "5000", []string{"inf @0.03"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, moved, err := continueGraduatedTiers(egress, alpacadecimal.RequireFromString(tt.used))
			if err != nil {
				t.Fatal(err)
			}
			if moved != tt.moved || !slices.Equal(tierStrings(t, got), tt.want) {
				t.Errorf("tiers = %v (moved %v), want %v (moved %v)", tierStrings(t, got), moved, tt.want, tt.moved)
			}
		})
	}

	t.Run("flat price of a tier entered is dropped", func(t *testing.T) {
		p := bridgeTieredPrice(t, tier("10", "", "5"), tier("20", "1", "3"), tier("", "0.5", "2"))
		got, _, err := continueGraduatedTiers(p, alpacadecimal.NewFromInt(12))
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"8 @1", "inf @0.5 +2"}; !slices.Equal(tierStrings(t, got), want) {
			t.Errorf("inside tier 2: %v, want %v", tierStrings(t, got), want)
		}
		// Usage reached tier 2's start but not beyond: its flat price is
		// still to come, but OpenMeter would charge a first tier's flat
		// price for no usage at all.
		if _, moved, err := continueGraduatedTiers(p, alpacadecimal.NewFromInt(10)); err != nil || moved {
			t.Errorf("at a flat-priced boundary: moved = %v, err = %v; want left as it is", moved, err)
		}
		got, _, err = continueGraduatedTiers(p, alpacadecimal.NewFromInt(5))
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"5 @0", "15 @1 +3", "inf @0.5 +2"}; !slices.Equal(tierStrings(t, got), want) {
			t.Errorf("inside a flat-only tier: %v, want %v", tierStrings(t, got), want)
		}
	})

	t.Run("unit prices are left alone", func(t *testing.T) {
		var p omv3.BillingPrice
		if err := p.FromBillingPriceUnit(omv3.BillingPriceUnit{Amount: "1", Type: omv3.BillingPriceUnitTypeUnit}); err != nil {
			t.Fatal(err)
		}
		if _, moved, err := continueGraduatedTiers(p, alpacadecimal.NewFromInt(10)); err != nil || moved {
			t.Errorf("moved = %v, err = %v; want a unit price untouched", moved, err)
		}
	})
}

// TestContinueGraduatedTiers_PricesLikeEarlierUsage checks the bridge
// against OpenMeter's own rating engine: usage priced from zero on the moved
// tiers costs what it costs priced after the earlier usage on the original
// ones, which is what OpenMeter would charge had the subscription not
// changed. With a unit conversion, the earlier usage is moved in the
// pricing unit, the way OpenMeter converts it.
func TestContinueGraduatedTiers_PricesLikeEarlierUsage(t *testing.T) {
	ladders := map[string]omv3.BillingPrice{
		"free tier":   bridgeTieredPrice(t, tier("200", "0", ""), tier("1000", "0.05", ""), tier("", "0.03", "")),
		"flat prices": bridgeTieredPrice(t, tier("10", "", "5"), tier("100", "1", "3"), tier("", "0.5", "2")),
		"decimals":    bridgeTieredPrice(t, tier("0.5", "2", ""), tier("2.25", "1.5", "0.75"), tier("", "1", "")),
	}
	gib := "1073741824"
	ceiling := omv3.BillingUnitConfigRoundingModeCeiling
	conversions := map[string]*omv3.BillingUnitConfig{
		"no conversion": nil,
		"GiB, ceiling":  {Operation: omv3.BillingUnitConfigOperationDivide, ConversionFactor: gib, Rounding: &ceiling},
	}
	quantities := []float64{0, 0.25, 1, 5, 10, 99.5, 100, 150, 200, 250, 1000, 4321}
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	for name, raw := range ladders {
		price, err := usagePrice(raw)
		if err != nil {
			t.Fatal(err)
		}
		for convName, conv := range conversions {
			var uc *productcatalog.UnitConfig
			if conv != nil {
				if uc, err = unitConfig(*conv); err != nil {
					t.Fatal(err)
				}
			}
			scale := 1.0
			if conv != nil {
				scale = 1073741824
			}
			for _, earlier := range quantities {
				for _, later := range quantities {
					pre := alpacadecimal.NewFromFloat(earlier * scale)
					qty := alpacadecimal.NewFromFloat(later * scale)
					want, err := PriceUsage(UsageToPrice{
						Price: price, UnitConfig: uc, Currency: "USD", From: from, To: from.Add(time.Hour),
						Quantity: qty, PreLinePeriodQuantity: pre,
					})
					if err != nil {
						t.Fatalf("%s/%s: %v", name, convName, err)
					}
					shift := pre
					if uc != nil {
						_, shift = uc.Apply(pre)
					}
					moved, ok, err := continueGraduatedTiers(raw, shift)
					if err != nil {
						t.Fatal(err)
					}
					if !ok && shift.IsPositive() {
						// Left to restart: a flat-priced tier starts
						// exactly where the earlier usage ended.
						continue
					}
					movedPrice, err := usagePrice(moved)
					if err != nil {
						t.Fatal(err)
					}
					got, err := PriceUsage(UsageToPrice{
						Price: movedPrice, UnitConfig: uc, Currency: "USD", From: from, To: from.Add(time.Hour),
						Quantity: qty,
					})
					if err != nil {
						t.Fatalf("%s/%s moved: %v", name, convName, err)
					}
					if !got.Amount.Equal(want.Amount) {
						t.Errorf("%s/%s: %v after %v costs %s on the bridge, want %s", name, convName, later, earlier, got.Amount, want.Amount)
					}
				}
			}
		}
	}
}

func TestBridgePlanKey(t *testing.T) {
	planKey := "0f6c1b4e_5d2a_4c3b_9e8f_1a2b3c4d5e6f"
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	key := BridgePlanKey(planKey, "cust", from)
	if len(key) > maxPlanKeyLength {
		t.Errorf("key %q is %d characters, over OpenMeter's %d", key, len(key), maxPlanKeyLength)
	}
	if base, ok := BridgedPlanKey(key); !ok || base != planKey {
		t.Errorf("BridgedPlanKey(%q) = %q, %v; want %q", key, base, ok, planKey)
	}
	if _, ok := BridgedPlanKey(planKey); ok {
		t.Errorf("an Offer's plan key reads as a bridge")
	}
	if BridgePlanKey(planKey, "cust", from.AddDate(0, 1, 0)) == key || BridgePlanKey(planKey, "other", from) == key {
		t.Errorf("bridge keys must differ by period and customer")
	}
	if BridgePlanKey(planKey, "cust", from.In(time.FixedZone("x", 3600))) != key {
		t.Errorf("the key must not depend on the time zone")
	}
}

func TestAlignedPeriodStart(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	tests := []struct{ anchor, t, want string }{
		{"2026-01-15T10:00:00Z", "2026-11-24T09:00:00Z", "2026-11-15T10:00:00Z"},
		{"2026-01-15T10:00:00Z", "2026-11-15T09:00:00Z", "2026-10-15T10:00:00Z"},
		{"2026-01-15T10:00:00Z", "2026-11-15T10:00:00Z", "2026-11-15T10:00:00Z"},
		{"2026-01-31T00:00:00Z", "2026-03-01T00:00:00Z", "2026-02-28T00:00:00Z"},
		{"2026-01-31T00:00:00Z", "2026-02-27T00:00:00Z", "2026-01-31T00:00:00Z"},
		{"2026-11-01T00:00:00Z", "2026-11-24T00:00:00Z", "2026-11-01T00:00:00Z"},
		// An anchor after t still aligns t's period.
		{"2026-12-10T00:00:00Z", "2026-11-24T00:00:00Z", "2026-11-10T00:00:00Z"},
	}
	for _, tt := range tests {
		if got := alignedPeriodStart(at(tt.anchor), at(tt.t)); !got.Equal(at(tt.want)) {
			t.Errorf("alignedPeriodStart(%s, %s) = %s, want %s", tt.anchor, tt.t, got.Format(time.RFC3339), tt.want)
		}
	}
}

func TestBridgeExitTime(t *testing.T) {
	anchor := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	boundary := anchor.AddDate(0, 1, 0)
	sub := om.Subscription{ActiveFrom: anchor.AddDate(0, 0, 13), BillingAnchor: anchor}

	if at, wait := bridgeExitTime(sub, anchor.AddDate(0, 0, 20)); !at.IsZero() || wait == nil || !wait.Equal(boundary) {
		t.Errorf("mid period: at %s, wait %v; want to wait for %s", at, wait, boundary)
	}
	if at, wait := bridgeExitTime(sub, boundary.Add(30*time.Second)); wait != nil || !at.Equal(boundary) {
		t.Errorf("just after the boundary: at %s, wait %v; want the change timed at %s", at, wait, boundary)
	}
	if at, wait := bridgeExitTime(sub, boundary.Add(time.Hour)); wait != nil || !at.IsZero() {
		t.Errorf("boundary missed: at %s, wait %v; want a change now", at, wait)
	}
}

func TestChainStart(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := func(d int) *time.Time { v := t0.AddDate(0, 0, d); return &v }
	a := om.Subscription{Id: "a", ActiveFrom: t0.AddDate(0, 0, -40), ActiveTo: end(5)}
	b := om.Subscription{Id: "b", ActiveFrom: t0.AddDate(0, 0, 5), ActiveTo: end(10)}
	c := om.Subscription{Id: "c", ActiveFrom: t0.AddDate(0, 0, 10)}
	subs := []om.Subscription{c, b, a}

	if got := chainStart(c, subs, t0); !got.Equal(a.ActiveFrom) {
		t.Errorf("back-to-back since before the period: %s, want %s", got, a.ActiveFrom)
	}
	gap := om.Subscription{Id: "b", ActiveFrom: t0.AddDate(0, 0, 5), ActiveTo: end(8)}
	if got := chainStart(c, []om.Subscription{c, gap, a}, t0); !got.Equal(c.ActiveFrom) {
		t.Errorf("after a gap: %s, want the subscription's own start %s", got, c.ActiveFrom)
	}
}

// bridgeFixture has a customer subscribed to plan "p" since well before its
// current billing period, which started ten days ago, and an active plan
// "q" whose egress card is free up to 200 GB, then tiered.
func bridgeFixture(t *testing.T, convert bool) (Client, *fakeServer, om.Subscription, time.Time) {
	t.Helper()
	c, f := newTestClient(t)
	f.seedCustomer("cust")
	f.meters["egress"] = om.Meter{Id: "id-egress", Slug: "egress"}
	seedFeature(f, featureFixture("egress", "egress"))
	f.seedPlanVersion("p", 1, omv3.BillingPlanStatusActive)
	f.seedPlanVersion("q", 1, omv3.BillingPlanStatusActive)
	card := usageRateCard("egress", "egress")
	card.Price = bridgeTieredPrice(t, tier("200", "0", ""), tier("1000", "0.05", ""), tier("", "0.03", ""))
	if convert {
		card.UnitConfig = &omv3.BillingUnitConfig{Operation: omv3.BillingUnitConfigOperationDivide, ConversionFactor: "1000000000"}
	}
	for id, p := range f.plans {
		if p.Key == "q" {
			p.Phases = []omv3.BillingPlanPhase{{Key: "default", Name: "default", RateCards: []omv3.BillingRateCard{card}}}
			f.plans[id] = p
		}
	}
	state, err := c.EnsureSubscription(context.Background(), desiredSub("p"))
	if err != nil {
		t.Fatalf("subscribe to p: %v", err)
	}
	anchor := time.Now().UTC().AddDate(0, 0, -10).Truncate(time.Second)
	sub := state.Current
	sub.ActiveFrom = anchor.AddDate(0, -2, 0)
	sub.BillingAnchor = anchor
	f.subscriptions[sub.Id] = sub

	scale := 1.0
	if convert {
		scale = 1e9
	}
	f.usage = []fakeUsage{
		{slug: "egress", customerID: sub.CustomerId, at: anchor.Add(-time.Hour), value: 500 * scale}, // last period
		{slug: "egress", customerID: sub.CustomerId, at: anchor.AddDate(0, 0, 2), value: 100 * scale},
		{slug: "egress", customerID: sub.CustomerId, at: anchor.AddDate(0, 0, 5), value: 50 * scale},
		{slug: "egress", customerID: "someone-else", at: anchor.AddDate(0, 0, 5), value: 1000 * scale},
	}
	return c, f, sub, anchor
}

func bridgeTiers(t *testing.T, f *fakeServer, key string) []string {
	t.Helper()
	plan, ok := f.activePlan(key)
	if !ok {
		t.Fatalf("no active plan %s", key)
	}
	return tierStrings(t, plan.Phases[0].RateCards[0].Price)
}

func TestEnsureSubscription_MidPeriodChangeGoesOntoABridge(t *testing.T) {
	for _, convert := range []bool{false, true} {
		t.Run(fmt.Sprintf("unit conversion %v", convert), func(t *testing.T) {
			c, f, before, anchor := bridgeFixture(t, convert)
			boundary := anchor.AddDate(0, 1, 0)
			bridgeKey := BridgePlanKey("q", "cust", anchor)

			state, err := c.EnsureSubscription(context.Background(), desiredSub("q"))
			if err != nil {
				t.Fatalf("switch to q: %v", err)
			}
			if state.Current.Plan.Key != bridgeKey || !state.Changed || state.ChangeAt != nil {
				t.Fatalf("state = %+v, want changed onto %s", state, bridgeKey)
			}
			if state.BridgedUntil == nil || !state.BridgedUntil.Equal(boundary) {
				t.Errorf("BridgedUntil = %v, want %s", state.BridgedUntil, boundary)
			}
			// 150 GB used this period, only the customer's own.
			if got, want := bridgeTiers(t, f, bridgeKey), []string{"50 @0", "850 @0.05", "inf @0.03"}; !slices.Equal(got, want) {
				t.Errorf("bridge tiers = %v, want %v", got, want)
			}
			if got, want := f.subsByStatus("cust"), []string{"inactive:p@1", "active:" + bridgeKey + "@1"}; !equalStrings(got, want) {
				t.Errorf("subscriptions = %v, want %v", got, want)
			}
			if convert != (f.v3Changes == 1) {
				t.Errorf("v3 changes = %d; a plan that converts usage changes through v3", f.v3Changes)
			}
			if !state.Current.BillingAnchor.Equal(before.BillingAnchor) {
				t.Errorf("anchor = %s, want the subscription's own %s", state.Current.BillingAnchor, before.BillingAnchor)
			}

			// Converged until the period ends.
			writes, planWrites := f.subscriptionWrites, f.planWrites
			state, err = c.EnsureSubscription(context.Background(), desiredSub("q"))
			if err != nil {
				t.Fatalf("again: %v", err)
			}
			if f.subscriptionWrites != writes || f.planWrites != planWrites || state.Changed ||
				state.BridgedUntil == nil || !state.BridgedUntil.Equal(boundary) {
				t.Fatalf("writes = %d/%d, state = %+v; want nothing until %s", f.subscriptionWrites-writes, f.planWrites-planWrites, state, boundary)
			}

			// A new version of q while bridged waits for the boundary too.
			f.publishPlanVersion("q", 2)
			if state, err = c.EnsureSubscription(context.Background(), desiredSub("q")); err != nil || state.Changed {
				t.Fatalf("new version of q: state %+v, err %v; want the bridge kept", state, err)
			}

			// Just after the boundary: onto q itself, timed at it, and the
			// bridge plan is deleted.
			bridgeSub := state.Current
			f.billingPeriods[bridgeSub.Id] = om.Period{From: boundary, To: boundary.AddDate(0, 1, 0)}
			after := desiredSub("q")
			after.Now = boundary.Add(20 * time.Second)
			state, err = c.EnsureSubscription(context.Background(), after)
			if err != nil {
				t.Fatalf("at the boundary: %v", err)
			}
			if state.Current.Plan.Key != "q" || state.Current.Plan.Version != 2 || state.BridgedUntil != nil || !state.Changed {
				t.Fatalf("state = %+v, want on q@2", state)
			}
			if at := f.changesAt[bridgeSub.Id]; !at.Equal(boundary) {
				t.Errorf("change timed at %s, want %s", at, boundary)
			}
			for _, p := range f.plans {
				if p.Key == bridgeKey && p.DeletedAt == nil {
					t.Errorf("bridge plan version %d left behind", p.Version)
				}
			}
		})
	}
}

func TestEnsureSubscription_MissedBridgeBoundary(t *testing.T) {
	c, f, _, anchor := bridgeFixture(t, false)
	if _, err := c.EnsureSubscription(context.Background(), desiredSub("q")); err != nil {
		t.Fatalf("switch to q: %v", err)
	}
	// The provider was down over the boundary: an hour into the next
	// period, the account moves onto a bridge for that period, continuing
	// from its usage since the boundary.
	boundary := anchor.AddDate(0, 1, 0)
	f.usage = append(f.usage, fakeUsage{slug: "egress", customerID: "id-cust", at: boundary.Add(10 * time.Minute), value: 30})
	late := desiredSub("q")
	late.Now = boundary.Add(time.Hour)
	f.clock = late.Now
	state, err := c.EnsureSubscription(context.Background(), late)
	if err != nil {
		t.Fatalf("after the boundary: %v", err)
	}
	next := BridgePlanKey("q", "cust", boundary)
	if state.Current.Plan.Key != next {
		t.Fatalf("current = %s, want the next period's bridge %s", state.Current.Plan.Key, next)
	}
	if got, want := bridgeTiers(t, f, next), []string{"170 @0", "970 @0.05", "inf @0.03"}; !slices.Equal(got, want) {
		t.Errorf("bridge tiers = %v, want %v", got, want)
	}
	if _, ok := f.activePlan(BridgePlanKey("q", "cust", anchor)); ok {
		t.Errorf("the missed period's bridge plan is still active")
	}
}

func TestEnsureSubscription_ChangeWithoutBridge(t *testing.T) {
	t.Run("no usage earlier in the period", func(t *testing.T) {
		c, f, _, _ := bridgeFixture(t, false)
		f.usage = nil
		state, err := c.EnsureSubscription(context.Background(), desiredSub("q"))
		if err != nil {
			t.Fatalf("switch to q: %v", err)
		}
		if state.Current.Plan.Key != "q" || state.BridgedUntil != nil {
			t.Fatalf("state = %+v, want straight onto q", state)
		}
	})

	t.Run("subscribed only since the period started", func(t *testing.T) {
		c, f, sub, _ := bridgeFixture(t, false)
		sub.ActiveFrom = time.Now()
		f.subscriptions[sub.Id] = sub
		queries := f.meterQueries
		state, err := c.EnsureSubscription(context.Background(), desiredSub("q"))
		if err != nil {
			t.Fatalf("switch to q: %v", err)
		}
		if state.Current.Plan.Key != "q" || f.meterQueries != queries {
			t.Fatalf("current = %s after %d usage queries; want q with none", state.Current.Plan.Key, f.meterQueries-queries)
		}
	})

	t.Run("a quarterly fee billed in advance", func(t *testing.T) {
		c, f, _, _ := bridgeFixture(t, false)
		inAdvance, quarterly := omv3.BillingPricePaymentTermInAdvance, "P3M"
		for id, p := range f.plans {
			if p.Key == "q" {
				p.Phases[0].RateCards = append(p.Phases[0].RateCards, omv3.BillingRateCard{
					Key: "support", Name: "support", PaymentTerm: &inAdvance, BillingCadence: &quarterly,
				})
				f.plans[id] = p
			}
		}
		state, err := c.EnsureSubscription(context.Background(), desiredSub("q"))
		if err != nil {
			t.Fatalf("switch to q: %v", err)
		}
		if state.Current.Plan.Key != "q" {
			t.Fatalf("current = %s, want q: a bridge would end mid-quarter", state.Current.Plan.Key)
		}
	})
}

// TestEnsureSubscription_BridgeOfAnotherPlan: moved again in the same
// period, the account gets a bridge of the newest plan, continuing from all
// its usage this period, and the first bridge plan goes.
func TestEnsureSubscription_BridgeOfAnotherPlan(t *testing.T) {
	c, f, _, anchor := bridgeFixture(t, false)
	if _, err := c.EnsureSubscription(context.Background(), desiredSub("q")); err != nil {
		t.Fatalf("switch to q: %v", err)
	}
	f.seedPlanVersion("r", 1, omv3.BillingPlanStatusActive)
	for id, p := range f.plans {
		if p.Key == "r" {
			card := usageRateCard("egress", "egress")
			card.Price = bridgeTieredPrice(t, tier("100", "0", ""), tier("", "0.04", ""))
			p.Phases = []omv3.BillingPlanPhase{{Key: "default", Name: "default", RateCards: []omv3.BillingRateCard{card}}}
			f.plans[id] = p
		}
	}
	state, err := c.EnsureSubscription(context.Background(), desiredSub("r"))
	if err != nil {
		t.Fatalf("switch to r: %v", err)
	}
	key := BridgePlanKey("r", "cust", anchor)
	if state.Current.Plan.Key != key {
		t.Fatalf("current = %s, want %s", state.Current.Plan.Key, key)
	}
	if got, want := bridgeTiers(t, f, key), []string{"inf @0.04"}; !slices.Equal(got, want) {
		t.Errorf("bridge tiers = %v, want %v", got, want)
	}
	if _, ok := f.activePlan(BridgePlanKey("q", "cust", anchor)); ok {
		t.Errorf("q's bridge plan is still active")
	}
}

func TestCancelSubscriptions_DeletesTheBridgePlan(t *testing.T) {
	c, f, _, anchor := bridgeFixture(t, false)
	if _, err := c.EnsureSubscription(context.Background(), desiredSub("q")); err != nil {
		t.Fatalf("switch to q: %v", err)
	}
	if err := c.CancelSubscriptions(context.Background(), "cust"); err != nil {
		t.Fatalf("CancelSubscriptions: %v", err)
	}
	if _, ok := f.activePlan(BridgePlanKey("q", "cust", anchor)); ok {
		t.Errorf("the canceled subscription's bridge plan is still active")
	}
}
