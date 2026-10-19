// SPDX-License-Identifier: AGPL-3.0-only

package usagequery

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alpacahq/alpacadecimal"
	om "github.com/openmeterio/openmeter/api/client/go"
	"github.com/openmeterio/openmeter/openmeter/productcatalog"

	usagev1alpha1 "go.miloapis.com/billing/api/usage/v1alpha1"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

var cpuSlug = openmeter.MeterSlug("compute.miloapis.com/cpu-hours")

func day(d, hour int) time.Time {
	return time.Date(2026, 10, d, hour, 0, 0, 0, time.UTC)
}

// spendEvent is usage as OpenMeter stores it.
type spendEvent struct {
	at       time.Time
	customer string
	project  string
	region   string
	value    float64
}

// usageFrom answers meter queries from events like OpenMeter: only the
// customers, time range and group-by filters asked for, one row per
// customer, window and combination of the requested group-by values.
func usageFrom(events []spendEvent) func(openmeter.MeterQuery) ([]openmeter.MeterQueryRow, error) {
	return func(q openmeter.MeterQuery) ([]openmeter.MeterQueryRow, error) {
		type key struct {
			customer string
			window   time.Time
			group    string
		}
		rows := map[key]*openmeter.MeterQueryRow{}
		var order []key
		for _, e := range events {
			if e.at.Before(q.From) || !e.at.Before(q.To) {
				continue
			}
			if len(q.CustomerIDs) > 0 && !slices.Contains(q.CustomerIDs, openmeter.CustomerID(e.customer)) {
				continue
			}
			dims := map[string]string{"project_name": e.project, "region": e.region}
			if !matchesFilters(q, dims) {
				continue
			}
			windowStart, windowEnd := q.From, q.To
			if q.WindowSize != nil {
				windowStart = q.From.Add(e.at.Sub(q.From).Truncate(24 * time.Hour))
				windowEnd = windowStart.Add(24 * time.Hour)
			}
			groupBy := map[string]string{}
			var group []string
			for _, k := range q.GroupBy {
				groupBy[k] = dims[k]
				group = append(group, dims[k])
			}
			k := key{e.customer, windowStart, strings.Join(group, "\x00")}
			row, ok := rows[k]
			if !ok {
				row = &openmeter.MeterQueryRow{WindowStart: windowStart, WindowEnd: windowEnd, CustomerID: openmeter.CustomerID(e.customer), GroupBy: groupBy}
				rows[k] = row
				order = append(order, k)
			}
			row.Value += e.value
		}
		var out []openmeter.MeterQueryRow
		for _, k := range order {
			out = append(out, *rows[k])
		}
		return out, nil
	}
}

func matchesFilters(q openmeter.MeterQuery, dims map[string]string) bool {
	for k, values := range q.FilterGroupBy {
		if !slices.Contains(values, dims[k]) {
			return false
		}
	}
	for k, f := range q.Filters {
		if !matches(f, dims[k]) {
			return false
		}
	}
	return true
}

func matches(f om.FilterString, v string) bool {
	switch {
	case f.Eq != nil:
		return v == *f.Eq
	case f.In != nil:
		return slices.Contains(*f.In, v)
	case f.Nin != nil:
		return !slices.Contains(*f.Nin, v)
	case f.And != nil:
		for _, sub := range *f.And {
			if !matches(sub, v) {
				return false
			}
		}
		return true
	}
	return true
}

func unitPrice(amount string) *productcatalog.Price {
	return productcatalog.NewPriceFrom(productcatalog.UnitPrice{Amount: alpacadecimal.RequireFromString(amount)})
}

func tier(upTo, amount string) productcatalog.PriceTier {
	t := productcatalog.PriceTier{UnitPrice: &productcatalog.PriceTierUnitPrice{Amount: alpacadecimal.RequireFromString(amount)}}
	if upTo != "" {
		u := alpacadecimal.RequireFromString(upTo)
		t.UpToAmount = &u
	}
	return t
}

func graduatedPrice(tiers ...productcatalog.PriceTier) *productcatalog.Price {
	return productcatalog.NewPriceFrom(productcatalog.TieredPrice{Mode: productcatalog.GraduatedTieredPrice, Tiers: tiers})
}

var (
	unitPrice2 = unitPrice("2")
	unitPrice3 = unitPrice("3")
	// 0-100 at 1, 100-1000 at 0.5, above at 0.1.
	graduated = graduatedPrice(tier("100", "1"), tier("1000", "0.5"), tier("", "0.1"))
)

// spendCard is a plan's usage rate card pricing slug.
func spendCard(key, slug string, price *productcatalog.Price, filters map[string]om.FilterString) openmeter.UsageRateCard {
	return openmeter.UsageRateCard{Key: key, MeterSlug: slug, Filters: filters, Price: price}
}

// fakeSubscription is a subscription with the usage rate cards of the plan
// version it bills.
type fakeSubscription struct {
	sub   om.Subscription
	cards []openmeter.UsageRateCard
}

// monthly is a subscription billed monthly from anchor, active from `from`,
// on a plan version of its own with the given rate cards.
func monthly(id, currency string, from, anchor time.Time, cards ...openmeter.UsageRateCard) fakeSubscription {
	return fakeSubscription{
		sub: om.Subscription{
			Id: id, Currency: om.CurrencyCode(currency), ActiveFrom: from,
			BillingAnchor: anchor, BillingCadence: "P1M",
			Plan: &om.PlanReference{Id: "plan-" + id, Key: "plan", Version: 1},
		},
		cards: cards,
	}
}

// endedAt ends s at `to`, as a plan change or migration does.
func endedAt(s fakeSubscription, to time.Time) fakeSubscription {
	s.sub.ActiveTo = &to
	return s
}

func spendQuery(meters ...string) *usagev1alpha1.UsageQuery {
	q := baseQuery() // 2026-10-05 to 2026-10-07, daily
	q.Spec.Meters = meters
	q.Spec.IncludeSpend = true
	return q
}

func meterResult(t *testing.T, q *usagev1alpha1.UsageQuery, name string) usagev1alpha1.MeterUsage {
	t.Helper()
	for _, m := range q.Status.Meters {
		if m.MeterDefinition == name {
			return m
		}
	}
	t.Fatalf("no result for meter %s", name)
	return usagev1alpha1.MeterUsage{}
}

func wantSpend(t *testing.T, m usagev1alpha1.MeterUsage, amount, currency string) {
	t.Helper()
	if m.SpendError != "" {
		t.Fatalf("%s: spendError %q", m.MeterDefinition, m.SpendError)
	}
	if m.Spend == nil {
		t.Fatalf("%s: no spend, want %s %s", m.MeterDefinition, amount, currency)
	}
	if m.Spend.Amount != amount || m.Spend.Currency != currency {
		t.Errorf("%s: spend = %s %s, want %s %s", m.MeterDefinition, m.Spend.Amount, m.Spend.Currency, amount, currency)
	}
}

func TestSpend(t *testing.T) {
	const main, eu = "organization-acme/acme-main", "organization-acme/acme-eu"

	tests := []struct {
		name   string
		ctx    inParentCtx
		subs   map[string][]fakeSubscription
		events []spendEvent
		want   string // amount in USD
	}{
		{
			name: "unit price",
			subs: map[string][]fakeSubscription{main: {monthly("s1", "USD", day(1, 0), day(1, 0),
				spendCard("cpu", cpuSlug, unitPrice2, nil))}},
			events: []spendEvent{
				{at: day(5, 3), customer: "01ACMEMAIN", value: 10},
				{at: day(6, 3), customer: "01ACMEMAIN", value: 5},
				{at: day(4, 3), customer: "01ACMEMAIN", value: 1000}, // before the range
			},
			want: "30",
		},
		{
			name: "tiers continue from the usage earlier in the billing period",
			subs: map[string][]fakeSubscription{main: {monthly("s1", "USD", day(1, 0), day(1, 0),
				spendCard("cpu", cpuSlug, graduated, nil))}},
			events: []spendEvent{
				{at: day(2, 0), customer: "01ACMEMAIN", value: 950}, // earlier this period
				{at: day(5, 3), customer: "01ACMEMAIN", value: 150},
			},
			want: "35", // 50 at 0.5 + 100 at 0.1
		},
		{
			name: "a range across two billing periods restarts the tiers",
			subs: map[string][]fakeSubscription{main: {monthly("s1", "USD", day(1, 0), day(6, 0),
				spendCard("cpu", cpuSlug, graduated, nil))}},
			events: []spendEvent{
				{at: day(2, 0), customer: "01ACMEMAIN", value: 950}, // earlier in the period ending Oct 6
				{at: day(5, 12), customer: "01ACMEMAIN", value: 150},
				{at: day(6, 12), customer: "01ACMEMAIN", value: 150}, // new period
			},
			want: "160", // 35 + (100 at 1 + 50 at 0.5)
		},
		{
			name: "usage before the subscription started is not billed",
			subs: map[string][]fakeSubscription{main: {monthly("s1", "USD", day(6, 0), day(6, 0),
				spendCard("cpu", cpuSlug, unitPrice2, nil))}},
			events: []spendEvent{
				{at: day(5, 3), customer: "01ACMEMAIN", value: 10},
				{at: day(6, 3), customer: "01ACMEMAIN", value: 5},
			},
			want: "10",
		},
		{
			name: "a price change in the range prices each part with its price",
			subs: map[string][]fakeSubscription{main: {
				endedAt(monthly("s1", "USD", day(1, 0), day(1, 0), spendCard("cpu", cpuSlug, unitPrice2, nil)), day(6, 0)),
				monthly("s2", "USD", day(6, 0), day(1, 0), spendCard("cpu", cpuSlug, unitPrice3, nil)),
			}},
			events: []spendEvent{
				{at: day(5, 3), customer: "01ACMEMAIN", value: 10},
				{at: day(6, 3), customer: "01ACMEMAIN", value: 5},
			},
			want: "35", // 10 at 2 + 5 at 3
		},
		{
			name: "rate cards with dimension filters price only their usage",
			subs: map[string][]fakeSubscription{main: {monthly("s1", "USD", day(1, 0), day(1, 0),
				spendCard("cpu-us", cpuSlug, unitPrice2, map[string]om.FilterString{"region": {Eq: new("us-east4")}}),
				spendCard("cpu-rest", cpuSlug, unitPrice3, map[string]om.FilterString{"region": {Nin: &[]string{"us-east4"}}}))}},
			events: []spendEvent{
				{at: day(5, 3), customer: "01ACMEMAIN", region: "us-east4", value: 10},
				{at: day(5, 4), customer: "01ACMEMAIN", region: "eu-west1", value: 1},
			},
			want: "23", // 10 at 2 + 1 at 3
		},
		{
			name: "every billing account of the organization adds up",
			subs: map[string][]fakeSubscription{
				main: {monthly("s1", "USD", day(1, 0), day(1, 0), spendCard("cpu", cpuSlug, unitPrice2, nil))},
				eu:   {monthly("s2", "USD", day(1, 0), day(1, 0), spendCard("cpu", cpuSlug, unitPrice3, nil))},
			},
			events: []spendEvent{
				{at: day(5, 3), customer: "01ACMEMAIN", value: 10},
				{at: day(5, 3), customer: "01ACMEEU", value: 1},
			},
			want: "23",
		},
		{
			name: "usage metered in bytes is priced per GiB",
			subs: map[string][]fakeSubscription{main: {monthly("s1", "USD", day(1, 0), day(1, 0), func() openmeter.UsageRateCard {
				// The first 200 GiB free, then 0.05 per GiB.
				c := spendCard("egress", cpuSlug, graduatedPrice(tier("200", "0"), tier("", "0.05")), nil)
				c.UnitConfig = &productcatalog.UnitConfig{
					Operation:        productcatalog.UnitConfigOperationDivide,
					ConversionFactor: alpacadecimal.NewFromInt(1 << 30),
				}
				return c
			}())}},
			events: []spendEvent{
				{at: day(2, 0), customer: "01ACMEMAIN", value: 100 << 30}, // earlier this period
				{at: day(5, 3), customer: "01ACMEMAIN", value: 300 << 30},
			},
			want: "10", // 100 GiB free + 200 GiB at 0.05
		},
		{
			name: "a project query prices only that project's usage",
			ctx:  inParentCtx{"Project", "web"},
			subs: map[string][]fakeSubscription{main: {monthly("s1", "USD", day(1, 0), day(1, 0),
				spendCard("cpu", cpuSlug, unitPrice2, nil))}},
			events: []spendEvent{
				{at: day(5, 3), customer: "01ACMEMAIN", project: "web", value: 10},
				{at: day(5, 3), customer: "01ACMEMAIN", project: "api", value: 7},
			},
			want: "20",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			omc := &fakeOpenMeter{customers: everyCustomer(), subscriptions: tt.subs, query: usageFrom(tt.events)}
			ctx := tt.ctx
			if ctx.kind == "" {
				ctx = inParentCtx{"Organization", "acme"}
			}
			got, err := create(t, inParent(ctx.kind, ctx.name), NewREST(world(t), omc), spendQuery("cpu-hours"))
			if err != nil {
				t.Fatal(err)
			}
			wantSpend(t, meterResult(t, got, "cpu-hours"), tt.want, "USD")
		})
	}
}

type inParentCtx struct{ kind, name string }

func TestSpend_MeterNoPlanPricesHasNone(t *testing.T) {
	omc := &fakeOpenMeter{customers: everyCustomer(), query: usageFrom(nil), subscriptions: map[string][]fakeSubscription{
		"organization-acme/acme-main": {monthly("s1", "USD", day(1, 0), day(1, 0),
			spendCard("cpu", cpuSlug, unitPrice2, nil))},
	}}
	got, err := create(t, inParent("Organization", "acme"), NewREST(world(t), omc), spendQuery("cpu-hours", "peak-storage"))
	if err != nil {
		t.Fatal(err)
	}
	// Priced, but no usage.
	wantSpend(t, meterResult(t, got, "cpu-hours"), "0", "USD")
	if m := meterResult(t, got, "peak-storage"); m.Spend != nil || m.SpendError != "" {
		t.Errorf("peak-storage is in no plan: spend %+v, spendError %q", m.Spend, m.SpendError)
	}
}

func TestSpend_DifferentCurrenciesIsAnError(t *testing.T) {
	omc := &fakeOpenMeter{customers: everyCustomer(), query: usageFrom(nil), subscriptions: map[string][]fakeSubscription{
		"organization-acme/acme-main": {monthly("s1", "USD", day(1, 0), day(1, 0), spendCard("cpu", cpuSlug, unitPrice2, nil))},
		"organization-acme/acme-eu":   {monthly("s2", "EUR", day(1, 0), day(1, 0), spendCard("cpu", cpuSlug, unitPrice2, nil))},
	}}
	got, err := create(t, inParent("Organization", "acme"), NewREST(world(t), omc), spendQuery("cpu-hours"))
	if err != nil {
		t.Fatal(err)
	}
	m := meterResult(t, got, "cpu-hours")
	if m.Spend != nil || !strings.Contains(m.SpendError, "EUR, USD") {
		t.Errorf("spend %+v, spendError %q; want an error naming both currencies", m.Spend, m.SpendError)
	}
}

func TestSpend_PlansUnreadableKeepsTheUsage(t *testing.T) {
	omc := &fakeOpenMeter{
		customers:       everyCustomer(),
		query:           usageFrom([]spendEvent{{at: day(5, 3), customer: "01ACMEMAIN", value: 10}}),
		subscriptionErr: errors.New("boom"),
	}
	got, err := create(t, inParent("Organization", "acme"), NewREST(world(t), omc), spendQuery("cpu-hours"))
	if err != nil {
		t.Fatal(err)
	}
	m := meterResult(t, got, "cpu-hours")
	if m.Spend != nil || !strings.Contains(m.SpendError, "boom") {
		t.Errorf("spend %+v, spendError %q", m.Spend, m.SpendError)
	}
	if m.Groupings[0].Series[0].Total != "10" {
		t.Errorf("usage = %+v, want it still returned", m.Groupings)
	}
	if got.Status.Spend != nil || !strings.Contains(got.Status.SpendError, "cpu-hours") {
		t.Errorf("status.spend %+v, spendError %q; want no total and the meter named", got.Status.Spend, got.Status.SpendError)
	}
}

func TestSpend_OnlyWhenAskedFor(t *testing.T) {
	omc := &fakeOpenMeter{customers: everyCustomer(), query: usageFrom(nil)}
	q := spendQuery("cpu-hours")
	q.Spec.IncludeSpend = false
	got, err := create(t, inParent("Organization", "acme"), NewREST(world(t), omc), q)
	if err != nil {
		t.Fatal(err)
	}
	if omc.subscriptionCalls != 0 {
		t.Errorf("subscriptions read %d times", omc.subscriptionCalls)
	}
	if m := meterResult(t, got, "cpu-hours"); m.Spend != nil {
		t.Errorf("spend %+v without includeSpend", m.Spend)
	}
	if got.Status.Spend != nil || got.Status.SpendError != "" {
		t.Errorf("status.spend %+v, spendError %q without includeSpend", got.Status.Spend, got.Status.SpendError)
	}
}

func TestTotalSpend(t *testing.T) {
	usd := func(amount string) *usagev1alpha1.UsageSpend {
		return &usagev1alpha1.UsageSpend{Amount: amount, Currency: "USD"}
	}
	tests := []struct {
		name    string
		meters  []usagev1alpha1.MeterUsage
		want    *usagev1alpha1.UsageSpend
		wantErr string
	}{
		{
			name:   "adds up every meter",
			meters: []usagev1alpha1.MeterUsage{{MeterDefinition: "a", Spend: usd("5.5")}, {MeterDefinition: "b", Spend: usd("19")}},
			want:   usd("24.5"),
		},
		{
			name:   "a meter no plan prices adds nothing",
			meters: []usagev1alpha1.MeterUsage{{MeterDefinition: "a", Spend: usd("5.5")}, {MeterDefinition: "b"}},
			want:   usd("5.5"),
		},
		{
			name:   "no meter priced, no total",
			meters: []usagev1alpha1.MeterUsage{{MeterDefinition: "a"}},
		},
		{
			name:    "a meter whose spend failed leaves the total out",
			meters:  []usagev1alpha1.MeterUsage{{MeterDefinition: "a", Spend: usd("5.5")}, {MeterDefinition: "b", SpendError: "boom"}},
			wantErr: "could not be read: b",
		},
		{
			name:    "a meter whose usage failed leaves the total out",
			meters:  []usagev1alpha1.MeterUsage{{MeterDefinition: "a", Spend: usd("5.5")}, {MeterDefinition: "b", Error: "not set up"}},
			wantErr: "could not be read: b",
		},
		{
			name: "different currencies leave the total out",
			meters: []usagev1alpha1.MeterUsage{
				{MeterDefinition: "a", Spend: usd("1")},
				{MeterDefinition: "b", Spend: &usagev1alpha1.UsageSpend{Amount: "1", Currency: "EUR"}},
			},
			wantErr: "EUR, USD",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotErr := totalSpend(tt.meters)
			if tt.wantErr != "" {
				if got != nil || !strings.Contains(gotErr, tt.wantErr) {
					t.Errorf("got %+v, %q; want no total and an error containing %q", got, gotErr, tt.wantErr)
				}
				return
			}
			if gotErr != "" {
				t.Fatalf("unexpected error %q", gotErr)
			}
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSpend_TotalOfEveryMeter(t *testing.T) {
	peakSlug := openmeter.MeterSlug("compute.miloapis.com/peak-storage")
	omc := &fakeOpenMeter{customers: everyCustomer(), subscriptions: map[string][]fakeSubscription{
		"organization-acme/acme-main": {monthly("s1", "USD", day(1, 0), day(1, 0),
			spendCard("cpu", cpuSlug, unitPrice2, nil),
			spendCard("peak", peakSlug, unitPrice3, nil))},
	}}
	omc.query = func(q openmeter.MeterQuery) ([]openmeter.MeterQueryRow, error) {
		value := 10.0 // cpu-hours
		if q.Slug == peakSlug {
			value = 4
		}
		return []openmeter.MeterQueryRow{{WindowStart: q.From, WindowEnd: q.To, CustomerID: "01ACMEMAIN", GroupBy: map[string]string{}, Value: value}}, nil
	}
	got, err := create(t, inParent("Organization", "acme"), NewREST(world(t), omc), spendQuery("cpu-hours", "peak-storage"))
	if err != nil {
		t.Fatal(err)
	}
	wantSpend(t, meterResult(t, got, "cpu-hours"), "20", "USD")
	wantSpend(t, meterResult(t, got, "peak-storage"), "12", "USD")
	if s := got.Status.Spend; s == nil || s.Amount != "32" || s.Currency != "USD" || got.Status.SpendError != "" {
		t.Errorf("status.spend = %+v, spendError %q; want 32 USD", s, got.Status.SpendError)
	}
}
