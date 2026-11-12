// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
	omv3 "github.com/openmeterio/openmeter/api/v3"
)

// FeeEnd is a fee a subscription stops charging once its last period is
// over: a contract fee that runs for a set number of periods
// (servicePricingRefs[].periods).
type FeeEnd struct {
	// ItemKey is the fee's subscription item key: its rate card key.
	ItemKey string
	// EndsAt is when its last period ends, on a billing boundary.
	EndsAt time.Time
}

// FeeEndResult is what EndFees did.
type FeeEndResult struct {
	// Scheduled are fees whose removal was just sent, to take effect at the
	// end of the current billing period, their EndsAt.
	Scheduled []FeeEnd
	// RemovedLate are fees removed now because their EndsAt had passed with
	// the fee still on the subscription: the provider was down, or the fee
	// was added back by a later change of plan. Any period charged after
	// EndsAt needs crediting by hand.
	RemovedLate []FeeEnd
	// NextCheck, when set, is when a fee's removal can next be sent: the end
	// of the current billing period, so the next starts the fee's last.
	NextCheck *time.Time
}

// subscriptionView is the part of a subscription EndFees reads: the items
// of its current phase and its current billing period.
type subscriptionView struct {
	phaseKey string
	// items by key, with when each stops (nil while it runs on).
	items  map[string]*time.Time
	period *om.Period
}

func (c *client) viewSubscription(ctx context.Context, subscriptionID string, now time.Time) (subscriptionView, error) {
	rsp, err := c.api.GetSubscriptionWithResponse(ctx, subscriptionID, nil)
	if err != nil {
		return subscriptionView{}, classify(nil, nil, err)
	}
	if err := classifySubscription(rsp.HTTPResponse, rsp.Body); err != nil {
		return subscriptionView{}, err
	}
	view := subscriptionView{items: map[string]*time.Time{}}
	if rsp.JSON200 == nil {
		return view, nil
	}
	if rsp.JSON200.Alignment != nil {
		view.period = rsp.JSON200.Alignment.CurrentAlignedBillingPeriod
	}
	for _, phase := range rsp.JSON200.Phases {
		if phase.ActiveFrom.After(now) || (phase.ActiveTo != nil && !phase.ActiveTo.After(now)) {
			continue
		}
		view.phaseKey = phase.Key
		for _, item := range phase.Items {
			view.items[item.Key] = item.ActiveTo
		}
	}
	return view, nil
}

// EndFees stops charging each fee once its last period ends. OpenMeter
// can't give an item an end date in advance, only remove it with an edit
// that takes effect now or at the next billing cycle (the subscription's
// monthly boundary). So in the billing period that ends at a fee's EndsAt,
// the fee's last, it sends that removal timed next_billing_cycle; earlier
// it does nothing and reports when to look again. A fee already removed, or
// with its removal already scheduled, is left alone.
//
// A fee found still charging after its EndsAt is removed immediately and
// reported, rather than left to run on: its periods after EndsAt are
// charged and need crediting by hand. So is one whose EndsAt falls inside
// a billing period instead of at its end, once EndsAt passes, since a
// removal at the next cycle would come too late.
func (c *client) EndFees(ctx context.Context, subscriptionID string, fees []FeeEnd, now time.Time) (FeeEndResult, error) {
	var result FeeEndResult
	if len(fees) == 0 {
		return result, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	view, err := c.viewSubscription(ctx, subscriptionID, now)
	if err != nil {
		return result, fmt.Errorf("read subscription: %w", err)
	}
	var atCycle, removeNow []FeeEnd
	for _, fee := range fees {
		activeTo, present := view.items[fee.ItemKey]
		if !present || (activeTo != nil && !activeTo.After(fee.EndsAt)) {
			continue
		}
		switch {
		case !now.Before(fee.EndsAt):
			removeNow = append(removeNow, fee)
		case view.period != nil && view.period.To.Equal(fee.EndsAt):
			atCycle = append(atCycle, fee)
		default:
			next := fee.EndsAt
			if view.period != nil && view.period.To.Before(next) {
				next = view.period.To
			}
			if result.NextCheck == nil || next.Before(*result.NextCheck) {
				result.NextCheck = &next
			}
		}
	}
	if len(atCycle) > 0 {
		if err := c.removeItems(ctx, subscriptionID, view.phaseKey, atCycle, omv3.BillingSubscriptionEditTimingEnumNextBillingCycle); err != nil {
			return result, err
		}
		result.Scheduled = atCycle
	}
	if len(removeNow) > 0 {
		if err := c.removeItems(ctx, subscriptionID, view.phaseKey, removeNow, omv3.BillingSubscriptionEditTimingEnumImmediate); err != nil {
			return result, err
		}
		result.RemovedLate = removeNow
	}
	return result, nil
}

// removeItems removes fees' items from the subscription's phase in one
// edit, at the given timing.
func (c *client) removeItems(ctx context.Context, subscriptionID, phaseKey string, fees []FeeEnd, timing omv3.BillingSubscriptionEditTimingEnum) error {
	keys := make([]string, 0, len(fees))
	for _, fee := range fees {
		keys = append(keys, fee.ItemKey)
	}
	sort.Strings(keys)
	body := omv3.BillingSubscriptionEdit{}
	for _, key := range keys {
		var op omv3.BillingSubscriptionEditOperation
		if err := op.FromBillingSubscriptionEditRemoveItem(omv3.BillingSubscriptionEditRemoveItem{
			Type:     omv3.BillingSubscriptionEditRemoveItemTypeRemoveItem,
			PhaseKey: phaseKey,
			ItemKey:  key,
		}); err != nil {
			return &PermanentError{Err: fmt.Errorf("encode removal of %s: %w", key, err)}
		}
		body.Customizations = append(body.Customizations, op)
	}
	var t omv3.BillingSubscriptionEditTiming
	if err := t.FromBillingSubscriptionEditTimingEnum(timing); err != nil {
		return &PermanentError{Err: fmt.Errorf("encode edit timing: %w", err)}
	}
	body.Timing = &t
	_, err := c.v3.do(ctx, http.MethodPost, "/subscriptions/"+url.PathEscape(subscriptionID)+"/edit", nil, body, nil)
	if err != nil {
		return fmt.Errorf("remove %v from subscription %s (%s): %w", keys, subscriptionID, timing, reclassifySubscription(err))
	}
	return nil
}
