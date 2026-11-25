// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alpacahq/alpacadecimal"
	om "github.com/openmeterio/openmeter/api/client/go"
	omv3 "github.com/openmeterio/openmeter/api/v3"
)

// Bridge plans.
//
// A plan change in OpenMeter ends the customer's subscription and starts a
// new one, and OpenMeter prices the new subscription's usage as if the
// billing period had started with it: tiered prices count only the usage
// since the change (quantitysnapshot.go reaches back no further than the
// subscription item's own line). A customer moved to another Offer
// mid-period would get its free and cheaper tiers again.
//
// So a change in the middle of a billing period goes onto a bridge plan
// instead: a copy of the plan's active version, for this customer and
// period only, whose graduated tiers start where the usage earlier in the
// period left off. At the end of the period the subscription changes onto
// the plan itself, timed at the boundary, and the bridge plan is deleted.
// The bridge carries the same rate cards (keys, features, discounts, tax),
// so invoices and usage reports read it like the plan it copies.
//
// The usage it continues from is what OpenMeter has when the change is made:
// usage reported later with an earlier timestamp is billed on the previous
// subscription as usual, but doesn't move the bridge's tiers.

// bridgePlanKeyInfix separates the plan key from the customer and period
// hash in a bridge plan's key.
const bridgePlanKeyInfix = "_bridge_"

// maxPlanKeyLength is OpenMeter's limit on a resource key.
const maxPlanKeyLength = 64

// BridgePlanKey is the key of the bridge plan that carries customer onto
// planKey for the billing period starting at periodFrom.
func BridgePlanKey(planKey string, customer CustomerKey, periodFrom time.Time) string {
	sum := sha256.Sum256([]byte(string(customer) + "/" + strconv.FormatInt(periodFrom.Unix(), 10)))
	return planKey + bridgePlanKeyInfix + hex.EncodeToString(sum[:])[:12]
}

// BridgedPlanKey returns the key of the plan a bridge plan's key carries a
// customer onto, and whether key is a bridge plan's key.
func BridgedPlanKey(key string) (string, bool) {
	i := strings.LastIndex(key, bridgePlanKeyInfix)
	if i <= 0 {
		return "", false
	}
	return key[:i], true
}

// onBridgeOf reports whether sub is on a bridge plan of planKey.
func onBridgeOf(sub om.Subscription, planKey string) bool {
	if sub.Plan == nil {
		return false
	}
	base, ok := BridgedPlanKey(sub.Plan.Key)
	return ok && base == planKey
}

// onBridge reports whether sub is on any bridge plan.
func onBridge(sub om.Subscription) bool {
	if sub.Plan == nil {
		return false
	}
	_, ok := BridgedPlanKey(sub.Plan.Key)
	return ok
}

// subscriptionAnchor is the time sub's billing periods are counted from.
func subscriptionAnchor(sub om.Subscription) time.Time {
	if sub.BillingAnchor.IsZero() {
		return sub.ActiveFrom
	}
	return sub.BillingAnchor
}

// alignedPeriodStart is the start of the monthly billing period counted from
// anchor that contains t. OpenMeter reports a subscription's first period
// from the subscription's start, so the start of a period the subscription
// began in the middle of is worked out here.
func alignedPeriodStart(anchor, t time.Time) time.Time {
	months := monthsFrom(anchor, t)
	start := addMonthsClamped(anchor, months)
	for start.After(t) {
		months--
		start = addMonthsClamped(anchor, months)
	}
	for next := addMonthsClamped(anchor, months+1); !next.After(t); next = addMonthsClamped(anchor, months+1) {
		months++
		start = next
	}
	return start
}

// bridgeExitTime decides when a subscription on a bridge plan moves onto the
// plan itself. While the bridge's first billing period lasts, it returns its
// end, to wait for. Within periodChangeTolerance of the next period
// starting, it returns that start, to time the change at. Later than that
// the boundary was missed, and it returns neither: the change is made now,
// onto a new bridge for the period under way.
func bridgeExitTime(sub om.Subscription, now time.Time) (at time.Time, wait *time.Time) {
	anchor := subscriptionAnchor(sub)
	start := alignedPeriodStart(anchor, now)
	if !start.After(sub.ActiveFrom) {
		end := addMonthsClamped(anchor, monthsFrom(anchor, start)+1)
		return time.Time{}, &end
	}
	if now.Sub(start) <= periodChangeTolerance {
		return start, nil
	}
	return time.Time{}, nil
}

// bridgePlan returns the bridge plan step should change onto instead of
// plan, published, or nil when the change needs none: it falls on a billing
// period boundary, the customer had no subscription earlier in the period,
// or no graduated tier would move.
func (c *client) bridgePlan(ctx context.Context, desired DesiredSubscription, step subscriptionStep, plan omv3.BillingPlan, subs []om.Subscription, now time.Time) (*omv3.BillingPlan, error) {
	if step.kind != stepChange {
		return nil, nil
	}
	// A quarterly or annual fee billed in advance can't be moved off the
	// bridge at a monthly boundary without charging its period again.
	if planInAdvanceFeeMonths(plan) > 1 {
		return nil, nil
	}
	at := step.at
	if at.IsZero() {
		at = now
	}
	anchor := subscriptionAnchor(step.target)
	if desired.MoveAnchor && desired.BillingAnchor != nil {
		anchor = *desired.BillingAnchor
	}
	periodFrom := alignedPeriodStart(anchor, at)
	from := latest(periodFrom, chainStart(step.target, subs, periodFrom))
	if !from.Before(at.Add(-time.Second)) {
		return nil, nil
	}
	key := BridgePlanKey(plan.Key, desired.CustomerKey, periodFrom)
	if len(key) > maxPlanKeyLength {
		return nil, nil
	}

	phases, shifted, err := c.continueTiers(ctx, plan, step.target.CustomerId, from, at)
	if err != nil {
		return nil, err
	}
	if !shifted {
		return nil, nil
	}
	bridge, err := c.EnsurePlan(ctx, DesiredPlan{Key: key, Name: plan.Name, Currency: plan.Currency, Phases: phases})
	if err != nil {
		return nil, fmt.Errorf("publish bridge plan %s: %w", key, err)
	}
	return &bridge, nil
}

// chainStart is when the run of back-to-back subscriptions ending with sub
// started, looking back no further than from: the customer has been
// subscribed, and billed, since then.
func chainStart(sub om.Subscription, subs []om.Subscription, from time.Time) time.Time {
	start := sub.ActiveFrom
	for start.After(from) {
		prev := -1
		for i := range subs {
			s := subs[i]
			if s.Id == sub.Id || s.ActiveTo == nil || !s.ActiveFrom.Before(start) {
				continue
			}
			if d := start.Sub(*s.ActiveTo); d >= -time.Second && d <= time.Second {
				prev = i
				break
			}
		}
		if prev < 0 {
			return start
		}
		sub, start = subs[prev], subs[prev].ActiveFrom
	}
	return start
}

// continueTiers returns a copy of plan's phases in which every graduated
// price starts from the customer's usage of its rate card between from and
// to, and whether any price changed.
func (c *client) continueTiers(ctx context.Context, plan omv3.BillingPlan, customerID string, from, to time.Time) ([]omv3.BillingPlanPhase, bool, error) {
	cards, err := c.PlanUsageRateCards(ctx, plan.Id)
	if err != nil {
		return nil, false, err
	}
	byKey := make(map[string]UsageRateCard, len(cards))
	for _, card := range cards {
		byKey[card.Key] = card
	}
	phases, err := copyPhases(plan.Phases)
	if err != nil {
		return nil, false, &PermanentError{Err: err}
	}

	shifted := false
	for p := range phases {
		for r := range phases[p].RateCards {
			rc := &phases[p].RateCards[r]
			card, ok := byKey[rc.Key]
			if !ok || !isGraduated(rc.Price) {
				continue
			}
			used, err := c.cardUsage(ctx, card, customerID, from, to)
			if err != nil {
				return nil, false, fmt.Errorf("rate card %s: %w", rc.Key, err)
			}
			if card.UnitConfig != nil {
				// Tiers are in the pricing unit, and OpenMeter converts
				// earlier usage on its own, as it does here.
				_, used = card.UnitConfig.Apply(used)
			}
			price, moved, err := continueGraduatedTiers(rc.Price, used)
			if err != nil {
				return nil, false, &PermanentError{Err: fmt.Errorf("rate card %s: %w", rc.Key, err)}
			}
			if moved {
				rc.Price = price
				shifted = true
			}
		}
	}
	return phases, shifted, nil
}

// cardUsage is the customer's metered usage of card between from and to.
// A meter OpenMeter no longer has counts as no usage.
func (c *client) cardUsage(ctx context.Context, card UsageRateCard, customerID string, from, to time.Time) (alpacadecimal.Decimal, error) {
	rows, err := c.QueryMeter(ctx, MeterQuery{
		Slug:        card.MeterSlug,
		From:        from.Truncate(time.Second),
		To:          to.Truncate(time.Second),
		CustomerIDs: []CustomerID{CustomerID(customerID)},
		Filters:     card.Filters,
	})
	if errors.Is(err, ErrMeterNotFound) {
		return alpacadecimal.Zero, nil
	}
	if err != nil {
		return alpacadecimal.Zero, err
	}
	total := alpacadecimal.Zero
	for _, row := range rows {
		total = total.Add(alpacadecimal.NewFromFloat(row.Value))
	}
	return total, nil
}

func copyPhases(phases []omv3.BillingPlanPhase) ([]omv3.BillingPlanPhase, error) {
	raw, err := json.Marshal(phases)
	if err != nil {
		return nil, fmt.Errorf("copy plan phases: %w", err)
	}
	var out []omv3.BillingPlanPhase
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("copy plan phases: %w", err)
	}
	return out, nil
}

func isGraduated(p omv3.BillingPrice) bool {
	disc, err := p.Discriminator()
	return err == nil && disc == string(omv3.BillingPriceGraduatedTypeGraduated)
}

// continueGraduatedTiers returns price with its tiers moved down by used,
// so that pricing usage from zero on the result costs what pricing it after
// used units does on price: tiers used up are dropped, the tier used is
// shortened, and the flat price of a tier already entered isn't charged
// again. It reports whether anything moved; prices other than graduated
// ones are returned as they are, and so is one whose usage ended exactly at
// the start of a tier with a flat price. The provider writes no flat tier
// prices.
func continueGraduatedTiers(price omv3.BillingPrice, used alpacadecimal.Decimal) (omv3.BillingPrice, bool, error) {
	if !used.IsPositive() || !isGraduated(price) {
		return price, false, nil
	}
	graduated, err := price.AsBillingPriceGraduated()
	if err != nil {
		return price, false, err
	}
	lower := alpacadecimal.Zero
	var tiers []omv3.BillingPriceTier
	for _, tier := range graduated.Tiers {
		entered := used.GreaterThan(lower)
		if tier.UpToAmount != nil {
			upTo, err := alpacadecimal.NewFromString(*tier.UpToAmount)
			if err != nil {
				return price, false, fmt.Errorf("tier upToAmount %q: %w", *tier.UpToAmount, err)
			}
			lower = upTo
			if !upTo.GreaterThan(used) {
				continue
			}
			left := upTo.Sub(used).String()
			tier.UpToAmount = &left
		}
		if entered && tier.FlatPrice != nil {
			tier.FlatPrice = nil
			if tier.UnitPrice == nil {
				tier.UnitPrice = &omv3.BillingPriceUnit{Amount: "0", Type: omv3.BillingPriceUnitTypeUnit}
			}
		}
		tiers = append(tiers, tier)
	}
	if len(tiers) == 0 {
		// Every tier is bounded and used up: OpenMeter can't price beyond
		// the last one either way.
		return price, false, nil
	}
	if tiers[0].FlatPrice != nil {
		// The usage ended exactly where a tier with a flat price starts.
		// OpenMeter charges the first tier's flat price even for no usage,
		// so it can't lead the bridge; the price is left to restart.
		return price, false, nil
	}
	graduated.Tiers = tiers
	var out omv3.BillingPrice
	if err := out.FromBillingPriceGraduated(graduated); err != nil {
		return price, false, err
	}
	return out, true, nil
}

// deleteBridgePlan deletes every version of a bridge plan the customer has
// left. Its subscription keeps its own copy of the rate cards, and deleted
// plans can still be read by id. Best effort: a version left behind is
// archived, and no one can subscribe to it.
func (c *client) deleteBridgePlan(ctx context.Context, key string) error {
	if _, ok := BridgedPlanKey(key); !ok {
		return nil
	}
	versions, err := c.ListPlanVersions(ctx, key)
	if err != nil {
		return err
	}
	var errs []error
	for _, v := range versions {
		if err := c.DeletePlan(ctx, v); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
