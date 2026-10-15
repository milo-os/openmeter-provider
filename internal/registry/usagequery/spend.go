// SPDX-License-Identifier: AGPL-3.0-only

package usagequery

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alpacahq/alpacadecimal"
	om "github.com/openmeterio/openmeter/api/client/go"
	"github.com/openmeterio/openmeter/pkg/datetime"
	"github.com/openmeterio/openmeter/pkg/timeutil"
	"golang.org/x/sync/errgroup"

	usagev1alpha1 "go.miloapis.com/billing/api/usage/v1alpha1"
	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

// spendCalculator prices each meter's usage with the plans the billing
// accounts are subscribed to, the way OpenMeter prices it on their invoices:
// for every subscription item (one rate card of a plan) whose feature reads a
// queried meter, the usage it covers in the time range is split at billing
// period boundaries and each part is priced with the item's price, tiered
// prices continuing from the usage earlier in that period.
type spendCalculator struct {
	openMeter openmeter.Client
	spec      *usagev1alpha1.UsageQuerySpec
	customers []accountCustomer
	projects  []string
}

// spendResult is one meter's spend, or why it has none.
type spendResult struct {
	spend *usagev1alpha1.UsageSpend
	err   string
}

// pricedRange is the usage of one subscription item over part of one billing
// period, to be priced.
type pricedRange struct {
	plan     int
	customer openmeter.CustomerID
	currency string
	price    om.RateCardUsageBasedPrice
	filters  map[string]om.FilterString
	// from and to bound the usage to price; earlierFrom is where the
	// billing period's usage starts counting for this item, so tiered
	// prices continue from the usage between earlierFrom and from.
	from, to, earlierFrom time.Time
}

// meterSpendTotal accumulates one meter's spend.
type meterSpendTotal struct {
	priced     bool
	currencies map[string]bool
	amount     alpacadecimal.Decimal
	errs       []string
}

func (c *spendCalculator) run(ctx context.Context, plans []meterPlan) []spendResult {
	results := make([]spendResult, len(plans))
	totals := make([]meterSpendTotal, len(plans))
	for i := range totals {
		totals[i].currencies = map[string]bool{}
	}
	var mu sync.Mutex
	fail := func(plan int, err error) {
		mu.Lock()
		defer mu.Unlock()
		totals[plan].errs = append(totals[plan].errs, err.Error())
	}

	ranges, err := c.pricedRanges(ctx, plans)
	if err != nil {
		for i := range results {
			results[i].err = fmt.Sprintf("could not read the billing accounts' plans: %v", err)
		}
		return results
	}
	for _, r := range ranges {
		totals[r.plan].priced = true
		totals[r.plan].currencies[r.currency] = true
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(meterQueries)
	for _, r := range ranges {
		g.Go(func() error {
			amount, err := c.priceRange(gctx, plans[r.plan], r)
			if err != nil {
				fail(r.plan, err)
				return nil
			}
			mu.Lock()
			totals[r.plan].amount = totals[r.plan].amount.Add(amount)
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait()

	for i, t := range totals {
		switch {
		case len(t.errs) > 0:
			slices.Sort(t.errs)
			results[i].err = "could not price the usage: " + strings.Join(slices.Compact(t.errs), "; ")
		case !t.priced:
			// No plan prices this meter: its usage is not billed.
		case len(t.currencies) > 1:
			results[i].err = fmt.Sprintf("the billing accounts are billed in different currencies (%s)",
				strings.Join(slices.Sorted(maps.Keys(t.currencies)), ", "))
		default:
			currency := slices.Collect(maps.Keys(t.currencies))[0]
			results[i].spend = &usagev1alpha1.UsageSpend{Amount: t.amount.String(), Currency: currency}
		}
	}
	return results
}

// pricedRanges lists, for every billing account, the parts of the time range
// each subscription item prices for a queried meter.
func (c *spendCalculator) pricedRanges(ctx context.Context, plans []meterPlan) ([]pricedRange, error) {
	planBySlug := make(map[string]int, len(plans))
	for i, p := range plans {
		planBySlug[p.slug] = i
	}

	var (
		mu  sync.Mutex
		out []pricedRange
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(meterQueries)
	for _, customer := range c.customers {
		g.Go(func() error {
			ranges, err := c.customerRanges(gctx, customer, planBySlug)
			if err != nil {
				return err
			}
			mu.Lock()
			out = append(out, ranges...)
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *spendCalculator) customerRanges(ctx context.Context, customer accountCustomer, planBySlug map[string]int) ([]pricedRange, error) {
	start, end := c.spec.StartTime.Time, c.spec.EndTime.Time

	subs, err := c.openMeter.ListSubscriptions(ctx, customer.key)
	if errors.Is(err, openmeter.ErrCustomerNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var out []pricedRange
	for _, sub := range subs {
		if sub.DeletedAt != nil || !overlaps(sub.ActiveFrom, sub.ActiveTo, start, end) {
			continue
		}
		expanded, err := c.openMeter.GetSubscription(ctx, sub.Id)
		if err != nil {
			return nil, fmt.Errorf("subscription %s: %w", sub.Id, err)
		}
		for _, phase := range expanded.Phases {
			for _, item := range subscriptionItems(phase) {
				ranges, err := itemRanges(expanded, phase, item, planBySlug, start, end)
				if err != nil {
					return nil, fmt.Errorf("subscription %s item %s: %w", sub.Id, item.Key, err)
				}
				for i := range ranges {
					ranges[i].customer = customer.id
				}
				out = append(out, ranges...)
			}
		}
	}
	return out, nil
}

// subscriptionItems returns every version of a phase's items: the
// itemTimelines when OpenMeter returns them, else the current items.
func subscriptionItems(phase om.SubscriptionPhaseExpanded) []om.SubscriptionItem {
	if len(phase.ItemTimelines) == 0 {
		return phase.Items
	}
	var items []om.SubscriptionItem
	for _, key := range slices.Sorted(maps.Keys(phase.ItemTimelines)) {
		items = append(items, phase.ItemTimelines[key]...)
	}
	return items
}

// itemRanges splits the part of [start, end) the item was active in at its
// billing period boundaries. It returns nothing for items that price no
// queried meter.
func itemRanges(sub om.SubscriptionExpanded, phase om.SubscriptionPhaseExpanded, item om.SubscriptionItem, planBySlug map[string]int, start, end time.Time) ([]pricedRange, error) {
	if item.DeletedAt != nil || item.Price == nil || item.Included == nil || item.Included.Feature.MeterSlug == nil {
		return nil, nil
	}
	plan, ok := planBySlug[*item.Included.Feature.MeterSlug]
	if !ok {
		return nil, nil
	}

	// When the item was active: the item, its phase and its subscription
	// all bound it.
	activeFrom := latest(item.ActiveFrom, phase.ActiveFrom, sub.ActiveFrom)
	activeTo := earliest(item.ActiveTo, phase.ActiveTo, sub.ActiveTo)
	from := latest(activeFrom, start)
	to := end
	if activeTo != nil && activeTo.Before(to) {
		to = *activeTo
	}
	if !from.Before(to) {
		return nil, nil
	}

	cadence := sub.BillingCadence
	if item.BillingCadence != nil && *item.BillingCadence != "" {
		cadence = *item.BillingCadence
	}
	interval, err := datetime.ISODurationString(cadence).Parse()
	if err != nil {
		return nil, fmt.Errorf("billing cadence %q: %w", cadence, err)
	}
	periods, err := timeutil.NewRecurrenceFromISODuration(interval, sub.BillingAnchor)
	if err != nil {
		return nil, fmt.Errorf("billing cadence %q: %w", cadence, err)
	}

	var filters map[string]om.FilterString
	if f := item.Included.Feature.AdvancedMeterGroupByFilters; f != nil {
		filters = *f
	}

	var out []pricedRange
	for t := from; t.Before(to); {
		period, err := periods.GetPeriodAt(t)
		if err != nil {
			return nil, fmt.Errorf("billing period at %s: %w", t.Format(time.RFC3339), err)
		}
		partEnd := to
		if period.To.Before(partEnd) {
			partEnd = period.To
		}
		// OpenMeter's billing truncates line periods to whole seconds
		// (streaming.MinimumWindowSizeDuration); so do these.
		r := pricedRange{
			plan:        plan,
			currency:    string(sub.Currency),
			price:       *item.Price,
			filters:     filters,
			from:        t.Truncate(time.Second),
			to:          partEnd.Truncate(time.Second),
			earlierFrom: latest(period.From, activeFrom).Truncate(time.Second),
		}
		if r.from.Before(r.to) {
			out = append(out, r)
		}
		t = partEnd
	}
	return out, nil
}

// priceRange queries the usage r covers, and the usage earlier in its billing
// period when the price needs it, and prices it.
func (c *spendCalculator) priceRange(ctx context.Context, plan meterPlan, r pricedRange) (alpacadecimal.Decimal, error) {
	quantity, err := c.usage(ctx, plan, r, r.from, r.to)
	if err != nil {
		return alpacadecimal.Zero, err
	}
	if quantity.IsZero() {
		return alpacadecimal.Zero, nil
	}
	earlier := alpacadecimal.Zero
	if openmeter.PriceNeedsEarlierUsage(r.price) && r.earlierFrom.Before(r.from) {
		if earlier, err = c.usage(ctx, plan, r, r.earlierFrom, r.from); err != nil {
			return alpacadecimal.Zero, err
		}
	}
	amount, err := openmeter.PriceUsage(openmeter.UsageToPrice{
		Price:                 r.price,
		Currency:              r.currency,
		From:                  r.from,
		To:                    r.to,
		Quantity:              quantity,
		PreLinePeriodQuantity: earlier,
	})
	if err != nil {
		return alpacadecimal.Zero, fmt.Errorf("meter %s: %w", plan.definition.Name, err)
	}
	return amount, nil
}

// usage is the meter's usage between from and to that r's rate card covers:
// the customer's, through the feature's filters and the query's project
// filter.
func (c *spendCalculator) usage(ctx context.Context, plan meterPlan, r pricedRange, from, to time.Time) (alpacadecimal.Decimal, error) {
	q := openmeter.MeterQuery{
		Slug:        plan.slug,
		From:        from,
		To:          to,
		CustomerIDs: []openmeter.CustomerID{r.customer},
		Filters:     r.filters,
	}
	if len(c.projects) > 0 {
		key, ok := plan.groupByKeys[billingv1alpha1.SystemDimensionProjectName]
		if !ok {
			return alpacadecimal.Zero, errMeterLacksDimension
		}
		q.FilterGroupBy = map[string][]string{key: c.projects}
	}
	rows, err := c.openMeter.QueryMeter(ctx, q)
	if err != nil {
		return alpacadecimal.Zero, fmt.Errorf("meter %s: %w", plan.definition.Name, err)
	}
	total := alpacadecimal.Zero
	for _, row := range rows {
		total = total.Add(alpacadecimal.NewFromFloat(row.Value))
	}
	return total, nil
}

func overlaps(from time.Time, to *time.Time, start, end time.Time) bool {
	return from.Before(end) && (to == nil || to.After(start))
}

func latest(first time.Time, rest ...time.Time) time.Time {
	out := first
	for _, t := range rest {
		if t.After(out) {
			out = t
		}
	}
	return out
}

func earliest(times ...*time.Time) *time.Time {
	var out *time.Time
	for _, t := range times {
		if t != nil && (out == nil || t.Before(*out)) {
			out = t
		}
	}
	return out
}

// totalSpend adds up the meters' spend. It returns an error message instead
// when any meter's usage or spend could not be read, or the meters are billed
// in different currencies, so a partial total is never shown as the whole.
// Meters no plan prices add nothing; with none priced there is no total.
func totalSpend(meters []usagev1alpha1.MeterUsage) (*usagev1alpha1.UsageSpend, string) {
	var unreadable []string
	currencies := map[string]bool{}
	total := alpacadecimal.Zero
	for _, m := range meters {
		if m.Error != "" || m.SpendError != "" {
			unreadable = append(unreadable, m.MeterDefinition)
			continue
		}
		if m.Spend == nil {
			continue
		}
		amount, err := alpacadecimal.NewFromString(m.Spend.Amount)
		if err != nil {
			unreadable = append(unreadable, m.MeterDefinition)
			continue
		}
		currencies[m.Spend.Currency] = true
		total = total.Add(amount)
	}
	switch {
	case len(unreadable) > 0:
		return nil, "the spend of these meters could not be read: " + strings.Join(unreadable, ", ")
	case len(currencies) > 1:
		return nil, fmt.Sprintf("the meters are billed in different currencies (%s)",
			strings.Join(slices.Sorted(maps.Keys(currencies)), ", "))
	case len(currencies) == 0:
		return nil, ""
	}
	return &usagev1alpha1.UsageSpend{Amount: total.String(), Currency: slices.Collect(maps.Keys(currencies))[0]}, ""
}
