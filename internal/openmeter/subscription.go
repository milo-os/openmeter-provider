// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
	omv3 "github.com/openmeterio/openmeter/api/v3"
)

// ErrPlanNotPublished is returned by EnsureSubscription when the plan has no
// active version to subscribe to (the Offer has not been synced yet, or its
// sync failed).
var ErrPlanNotPublished = errors.New("openmeter: plan has no active version")

// ErrCustomerBillingNotReady is returned by EnsureSubscription when OpenMeter
// refuses the subscription because the customer cannot be invoiced yet —
// typically no Stripe app data, because the BillingAccount has no default
// payment method. OpenMeter only checks this for plans with priced items.
var ErrCustomerBillingNotReady = errors.New("openmeter: customer billing setup is incomplete")

// subscriptionListPageSize is the page size used when listing a customer's
// subscriptions.
const subscriptionListPageSize = 100

// maxSubscriptionSteps bounds how many mutations one EnsureSubscription call
// issues. The longest path is restore, then change; anything beyond that
// means the server state is moving underneath us.
const maxSubscriptionSteps = 4

// periodChangeTolerance is how soon after a billing period starts a plan
// change deferred to it can still be timed at its start. OpenMeter accepts
// a change up to two minutes in the past (the new subscription can't start
// any earlier); this leaves room for the two clocks to differ.
const periodChangeTolerance = 90 * time.Second

// DesiredSubscription is the subscription a reconciler wants a customer to
// have: exactly one live subscription on the active version of PlanKey.
type DesiredSubscription struct {
	// CustomerKey identifies the customer (the BillingAccount UID).
	CustomerKey CustomerKey
	// PlanKey is the plan to subscribe to.
	PlanKey string
	// Metadata is attached to subscriptions this client creates or changes.
	// Migrations keep the previous subscription's metadata.
	Metadata map[string]string
	// BillingAnchor, when set, is the date a new subscription's billing
	// cycles count from. OpenMeter moves it back to the nearest cycle
	// boundary before the subscription starts, so any 1st of a month bills
	// on the 1st, and a contract starting on the 18th bills on the 18th
	// (the last day of shorter months for the 29th to 31st). Unset, a
	// subscription bills from the day it starts. Changes and migrations
	// keep the existing subscription's anchor, unless MoveAnchor is set.
	BillingAnchor *time.Time
	// MoveAnchor makes a plan change move the subscription to
	// BillingAnchor instead of keeping its own: an account moving onto a
	// contract bills on the contract's day from then on.
	MoveAnchor bool
	// ChangeAtPeriodEnd makes a plan change wait until the subscription's
	// current billing period ends, so the period is billed entirely by the
	// plan it started on: an amended contract carries one price per
	// invoice. Until then nothing changes, and SubscriptionState.ChangeAt
	// says when it will. The change is timed at the boundary, so it must be
	// made within periodChangeTolerance of it; later than that it waits for
	// the next one, unless ChangeDue has passed. Migrations to a new version
	// of the same plan are still immediate.
	//
	// When the current plan bills a quarterly or annual fee in advance, the
	// change waits for the end of that fee's period instead, a boundary
	// every three or twelve months from the billing anchor: OpenMeter
	// doesn't credit an issued invoice, so a change mid-period would charge
	// the rest of the period again on the new plan. Such a boundary is never
	// made late, even after ChangeDue: a missed one waits for the next.
	ChangeAtPeriodEnd bool
	// ChangeDue is when a change deferred by ChangeAtPeriodEnd was due, as
	// SubscriptionState.ChangeAt reported it. Once it has passed, a change
	// that missed its boundary (the provider was down, say) is made
	// immediately rather than a period late.
	ChangeDue *time.Time
	// Now is the current time; time.Now when zero.
	Now time.Time
}

// SubscriptionState is a customer's subscription after EnsureSubscription.
type SubscriptionState struct {
	// Current is the subscription billing the customer now.
	Current om.Subscription
	// Pending, when set, is a scheduled successor of Current on the desired
	// plan version. This controller never schedules one itself; it is only
	// reported when one was created in OpenMeter directly.
	Pending *om.Subscription
	// PlanVersion is the active version of the desired plan.
	PlanVersion int
	// ChangeAt, when set, is when Current changes to the desired plan,
	// which it isn't on yet: the end of its billing period, for a
	// DesiredSubscription with ChangeAtPeriodEnd.
	ChangeAt *time.Time
	// Changed reports that this call changed the customer's subscriptions:
	// created, changed, migrated or restored one, or deleted a scheduled one.
	// OpenMeter invoices charges billed in advance, such as
	// one-time fees, as soon as a subscription starts, so a new invoice may
	// follow shortly.
	Changed bool
}

// subscriptionStepKind is the next mutation needed to converge.
type subscriptionStepKind int

const (
	stepNone subscriptionStepKind = iota
	stepCreate
	stepDeleteScheduled
	stepRestore
	stepChange
	stepMigrate
)

func (k subscriptionStepKind) String() string {
	switch k {
	case stepNone:
		return "none"
	case stepCreate:
		return "create"
	case stepDeleteScheduled:
		return "delete-scheduled"
	case stepRestore:
		return "restore"
	case stepChange:
		return "change"
	case stepMigrate:
		return "migrate"
	}
	return "unknown"
}

// subscriptionStep is one decision of nextSubscriptionStep.
type subscriptionStep struct {
	kind subscriptionStepKind
	// target is the subscription the step acts on (unset for create).
	target om.Subscription
	// at, for a change, is when it takes effect; zero means immediately.
	at time.Time
	// current and pending describe the converged state (kind == stepNone).
	current *om.Subscription
	pending *om.Subscription
}

// nextSubscriptionStep decides the next mutation that moves a customer's
// subscriptions towards one live subscription on planKey@activeVersion.
// It is pure so every branch is unit-testable.
//
// OpenMeter allows one live subscription per customer at a time. A live
// subscription is active (open-ended), canceled (ends at activeTo), or
// scheduled (starts in the future). The rules:
//
//   - Plan switch (another plan key): change immediately — the customer has
//     been moved to a different Offer and should be billed by it now.
//   - New version of the same plan: migrate immediately. Migrating at the
//     next billing cycle would avoid splitting the period, but it creates a
//     scheduled subscription, and OpenMeter (v1.0.0-beta.232) keeps counting
//     a scheduled subscription as live after it is deleted (its activeTo
//     stays null), so the features it references can never be archived
//     again. This controller therefore never creates scheduled subscriptions.
//   - A canceled subscription whose scheduled successor is the desired one
//     (created outside this controller) is a migration in flight: converged.
//   - Any other canceled subscription is restored (continued indefinitely,
//     dropping its successors) and then re-evaluated.
//   - Scheduled subscriptions not on the desired plan are deleted.
func nextSubscriptionStep(subs []om.Subscription, planKey string, activeVersion int) subscriptionStep {
	matches := func(s om.Subscription) bool {
		return s.Plan != nil && s.Plan.Key == planKey && s.Plan.Version >= activeVersion
	}

	var current *om.Subscription
	var scheduled []om.Subscription
	for i := range subs {
		switch subs[i].Status {
		case om.SubscriptionStatusActive, om.SubscriptionStatusCanceled:
			if current == nil {
				current = &subs[i]
			}
		case om.SubscriptionStatusScheduled:
			scheduled = append(scheduled, subs[i])
		}
	}
	sort.Slice(scheduled, func(i, j int) bool { return scheduled[i].ActiveFrom.Before(scheduled[j].ActiveFrom) })

	if current == nil {
		var pending *om.Subscription
		for i := range scheduled {
			if !matches(scheduled[i]) {
				return subscriptionStep{kind: stepDeleteScheduled, target: scheduled[i]}
			}
			if pending == nil {
				pending = &scheduled[i]
			}
		}
		if pending != nil {
			// Starts in the future: nothing bills the customer until then,
			// but there is nothing to change either.
			return subscriptionStep{kind: stepNone, current: pending}
		}
		return subscriptionStep{kind: stepCreate}
	}

	if current.Status == om.SubscriptionStatusCanceled {
		if len(scheduled) == 1 && matches(scheduled[0]) {
			return subscriptionStep{kind: stepNone, current: current, pending: &scheduled[0]}
		}
		return subscriptionStep{kind: stepRestore, target: *current}
	}

	// Active (open-ended) subscriptions cannot have successors, so any
	// scheduled subscription is stray and only blocks later changes.
	if len(scheduled) > 0 {
		return subscriptionStep{kind: stepDeleteScheduled, target: scheduled[0]}
	}
	switch {
	case current.Plan == nil || current.Plan.Key != planKey:
		return subscriptionStep{kind: stepChange, target: *current}
	case current.Plan.Version < activeVersion:
		return subscriptionStep{kind: stepMigrate, target: *current}
	}
	return subscriptionStep{kind: stepNone, current: current}
}

// EnsureSubscription converges the customer's subscriptions to a single live
// subscription on the active version of desired.PlanKey, one mutation at a
// time, re-reading the server state after each.
//
// Returns ErrCustomerNotFound when the customer does not exist,
// ErrPlanNotPublished when the plan has no active version, and
// ErrCustomerBillingNotReady when OpenMeter refuses to bill the customer.
func (c *client) EnsureSubscription(ctx context.Context, desired DesiredSubscription) (SubscriptionState, error) {
	if desired.CustomerKey == "" || desired.PlanKey == "" {
		return SubscriptionState{}, &PermanentError{Err: errors.New("DesiredSubscription.CustomerKey and PlanKey are required")}
	}

	versions, err := c.ListPlanVersions(ctx, desired.PlanKey)
	if err != nil {
		return SubscriptionState{}, fmt.Errorf("list plan versions: %w", err)
	}
	var active *omv3.BillingPlan
	for i := range versions {
		if versions[i].Status == omv3.BillingPlanStatusActive {
			active = &versions[i]
		}
	}
	if active == nil {
		return SubscriptionState{}, fmt.Errorf("plan %q: %w", desired.PlanKey, ErrPlanNotPublished)
	}
	activeVersion := active.Version
	now := desired.Now
	if now.IsZero() {
		now = time.Now()
	}

	changed := false
	for range maxSubscriptionSteps {
		subs, err := c.ListSubscriptions(ctx, desired.CustomerKey)
		if err != nil {
			return SubscriptionState{}, err
		}
		step := nextSubscriptionStep(subs, desired.PlanKey, activeVersion)
		if step.kind == stepNone {
			return SubscriptionState{Current: *step.current, Pending: step.pending, PlanVersion: activeVersion, Changed: changed}, nil
		}
		if step.kind == stepChange && desired.ChangeAtPeriodEnd {
			period, err := c.currentBillingPeriod(ctx, step.target.Id)
			if err != nil {
				return SubscriptionState{}, fmt.Errorf("get billing period: %w", err)
			}
			every, err := c.inAdvanceFeeMonths(ctx, step.target)
			if err != nil {
				return SubscriptionState{}, fmt.Errorf("read the current plan's fees: %w", err)
			}
			at, wait := periodChangeTime(step.target, period, now, desired.ChangeDue, every)
			if wait != nil {
				return SubscriptionState{Current: step.target, PlanVersion: activeVersion, ChangeAt: wait, Changed: changed}, nil
			}
			step.at = at
		}
		if err := c.applySubscriptionStep(ctx, desired, step, *active); err != nil {
			return SubscriptionState{}, fmt.Errorf("%s subscription: %w", step.kind, err)
		}
		changed = true
	}
	return SubscriptionState{}, &TransientError{Err: fmt.Errorf(
		"subscription for customer %s did not converge after %d steps", desired.CustomerKey, maxSubscriptionSteps)}
}

func (c *client) applySubscriptionStep(ctx context.Context, desired DesiredSubscription, step subscriptionStep, active omv3.BillingPlan) error {
	metadata := om.Metadata(desired.Metadata)
	if (step.kind == stepChange || step.kind == stepMigrate) && planHasUnitConfig(active) {
		return c.changeSubscriptionV3(ctx, desired, step, active.Version)
	}
	switch step.kind {
	case stepCreate:
		key := string(desired.CustomerKey)
		var body om.SubscriptionCreate
		if err := body.FromPlanSubscriptionCreate(om.PlanSubscriptionCreate{
			CustomerKey: &key,
			// Version omitted: OpenMeter subscribes to the version active
			// now, which avoids racing a concurrent publish.
			Plan:          om.PlanReferenceInput{Key: desired.PlanKey},
			Metadata:      &metadata,
			BillingAnchor: desired.BillingAnchor,
		}); err != nil {
			return &PermanentError{Err: err}
		}
		rsp, err := c.api.CreateSubscriptionWithResponse(ctx, body)
		if err != nil {
			return classify(nil, nil, err)
		}
		return classifySubscription(rsp.HTTPResponse, rsp.Body)

	case stepDeleteScheduled:
		rsp, err := c.api.DeleteSubscriptionWithResponse(ctx, step.target.Id)
		if err != nil {
			return classify(nil, nil, err)
		}
		if rsp.StatusCode() == http.StatusNotFound {
			return nil
		}
		return classifySubscription(rsp.HTTPResponse, rsp.Body)

	case stepRestore:
		rsp, err := c.api.RestoreSubscriptionWithResponse(ctx, step.target.Id)
		if err != nil {
			return classify(nil, nil, err)
		}
		return classifySubscription(rsp.HTTPResponse, rsp.Body)

	case stepChange:
		var timing om.SubscriptionTiming
		if step.at.IsZero() {
			if err := timing.FromSubscriptionTimingEnum(om.SubscriptionTimingEnumImmediate); err != nil {
				return &PermanentError{Err: err}
			}
		} else if err := timing.FromSubscriptionTiming1(step.at); err != nil {
			return &PermanentError{Err: err}
		}
		change := om.PlanSubscriptionChange{
			Plan:     om.PlanReferenceInput{Key: desired.PlanKey},
			Timing:   timing,
			Metadata: &metadata,
		}
		if desired.MoveAnchor {
			change.BillingAnchor = desired.BillingAnchor
		}
		var body om.SubscriptionChange
		if err := body.FromPlanSubscriptionChange(change); err != nil {
			return &PermanentError{Err: err}
		}
		rsp, err := c.api.ChangeSubscriptionWithResponse(ctx, step.target.Id, body)
		if err != nil {
			return classify(nil, nil, err)
		}
		return classifySubscription(rsp.HTTPResponse, rsp.Body)

	case stepMigrate:
		// Immediate, never next_billing_cycle: see nextSubscriptionStep.
		var timing om.SubscriptionTiming
		if err := timing.FromSubscriptionTimingEnum(om.SubscriptionTimingEnumImmediate); err != nil {
			return &PermanentError{Err: err}
		}
		target := active.Version
		rsp, err := c.api.MigrateSubscriptionWithResponse(ctx, step.target.Id, om.MigrateSubscriptionJSONRequestBody{
			TargetVersion: &target,
			Timing:        &timing,
		})
		if err != nil {
			return classify(nil, nil, err)
		}
		return classifySubscription(rsp.HTTPResponse, rsp.Body)
	}
	return &PermanentError{Err: fmt.Errorf("unknown subscription step %d", step.kind)}
}

// changeSubscriptionV3 moves the subscription onto planKey@version, the
// change and the migration in one, immediately or at step.at. The v1 API
// refuses both when the plan converts usage, so they go through v3, which
// cannot carry the subscription's metadata: v3 labels allow no "/" in keys
// or values. Unlike v1, a v3 change without an anchor bills from the moment
// of the change, so it carries the existing subscription's anchor over
// (or desired.BillingAnchor, with MoveAnchor).
func (c *client) changeSubscriptionV3(ctx context.Context, desired DesiredSubscription, step subscriptionStep, version int) error {
	target := step.target
	var timing omv3.BillingSubscriptionEditTiming
	if step.at.IsZero() {
		if err := timing.FromBillingSubscriptionEditTimingEnum(omv3.BillingSubscriptionEditTimingEnumImmediate); err != nil {
			return &PermanentError{Err: err}
		}
	} else if err := timing.FromDateTime(step.at); err != nil {
		return &PermanentError{Err: err}
	}
	body := omv3.BillingSubscriptionChange{Timing: timing}
	switch {
	case step.kind == stepChange && desired.MoveAnchor && desired.BillingAnchor != nil:
		anchor := *desired.BillingAnchor
		body.BillingAnchor = &anchor
	case !target.BillingAnchor.IsZero():
		anchor := target.BillingAnchor
		body.BillingAnchor = &anchor
	}
	key := string(desired.CustomerKey)
	body.Customer.Key = &key
	body.Plan = &struct {
		Id      *omv3.ULID        `json:"id,omitempty"`
		Key     *omv3.ResourceKey `json:"key,omitempty"`
		Version *int              `json:"version,omitempty"`
	}{Key: &desired.PlanKey, Version: &version}
	_, err := c.v3.do(ctx, http.MethodPost, "/subscriptions/"+url.PathEscape(target.Id)+"/change", nil, body, nil)
	return reclassifySubscription(err)
}

// periodChangeTime decides when a plan change deferred to the end of the
// subscription's billing period happens. Within periodChangeTolerance of a
// period starting, it returns that start, to time the change at. Once due
// has passed, the boundary was missed, and it returns neither, to change
// immediately. Otherwise it returns when to come back: the end of the
// current period.
//
// A subscription in its first period, which started when it did, waits for
// the period to end, since a change at its start would leave it empty. With
// no aligned period (OpenMeter reports one for active, aligned
// subscriptions), the change happens immediately.
//
// everyMonths above 1 is the longest period of a fee the current plan bills
// in advance (inAdvanceFeeMonths): only a boundary that many months on from
// the subscription's billing anchor will do, and a missed one, even past
// due, waits for the next, since changing mid-period would charge the rest
// of the fee's period again.
func periodChangeTime(sub om.Subscription, period *om.Period, now time.Time, due *time.Time, everyMonths int) (at time.Time, wait *time.Time) {
	if period == nil {
		return time.Time{}, nil
	}
	anchor := sub.BillingAnchor
	if anchor.IsZero() {
		anchor = sub.ActiveFrom
	}
	aligned := everyMonths <= 1 || monthsFrom(anchor, period.From)%everyMonths == 0
	if aligned && period.From.After(sub.ActiveFrom) && !now.Before(period.From) && now.Sub(period.From) <= periodChangeTolerance {
		return period.From, nil
	}
	if everyMonths <= 1 {
		if due != nil && !now.Before(*due) {
			return time.Time{}, nil
		}
		end := period.To
		return time.Time{}, &end
	}
	months := monthsFrom(anchor, period.To)
	if r := months % everyMonths; r != 0 {
		months += everyMonths - r
	}
	end := addMonthsClamped(anchor, months)
	return time.Time{}, &end
}

// monthsFrom is how many whole months a billing period boundary is from the
// anchor it's counted from.
func monthsFrom(anchor, boundary time.Time) int {
	anchor, boundary = anchor.UTC(), boundary.UTC()
	return (boundary.Year()-anchor.Year())*12 + int(boundary.Month()-anchor.Month())
}

// addMonthsClamped is the billing period boundary months after anchor: the
// anchor's day, or the month's last day when it's shorter, at the anchor's
// time of day, as OpenMeter aligns periods.
func addMonthsClamped(anchor time.Time, months int) time.Time {
	anchor = anchor.UTC()
	first := time.Date(anchor.Year(), anchor.Month()+time.Month(months), 1, 0, 0, 0, 0, time.UTC)
	day := anchor.Day()
	if last := first.AddDate(0, 1, -1).Day(); day > last {
		day = last
	}
	return time.Date(first.Year(), first.Month(), day,
		anchor.Hour(), anchor.Minute(), anchor.Second(), anchor.Nanosecond(), time.UTC)
}

// inAdvanceFeeMonths is the longest cadence, in months, of a fee the
// subscription's plan bills in advance: 3 for a quarterly fee, 12 for an
// annual one, and 1 when it has neither (monthly fees, fees in arrears and
// usage are billed for no more than the month). A plan that can't be found
// counts as 1.
func (c *client) inAdvanceFeeMonths(ctx context.Context, sub om.Subscription) (int, error) {
	if sub.Plan == nil || sub.Plan.Key == "" {
		return 1, nil
	}
	versions, err := c.ListPlanVersions(ctx, sub.Plan.Key)
	if err != nil {
		return 0, err
	}
	for _, plan := range versions {
		if plan.Version == sub.Plan.Version {
			return planInAdvanceFeeMonths(plan), nil
		}
	}
	return 1, nil
}

// planInAdvanceFeeMonths is inAdvanceFeeMonths for a plan already read.
func planInAdvanceFeeMonths(plan omv3.BillingPlan) int {
	longest := 1
	for _, phase := range plan.Phases {
		for _, card := range phase.RateCards {
			if card.PaymentTerm == nil || *card.PaymentTerm != omv3.BillingPricePaymentTermInAdvance || card.BillingCadence == nil {
				continue
			}
			months := 1
			switch *card.BillingCadence {
			case "P3M":
				months = 3
			case "P1Y", "P12M":
				months = 12
			}
			longest = max(longest, months)
		}
	}
	return longest
}

// currentBillingPeriod returns the subscription's current billing period,
// or nil when OpenMeter reports none.
func (c *client) currentBillingPeriod(ctx context.Context, subscriptionID string) (*om.Period, error) {
	sub, err := c.getSubscriptionV3(ctx, subscriptionID)
	if err != nil {
		return nil, err
	}
	return periodOf(sub), nil
}

// getSubscriptionV3 reads a subscription through the v3 API. The v1 read
// refuses any subscription with an item that converts usage into its
// pricing unit (400 unit_config_not_representable: "only available via the
// v3 API"), which a contract billing a customer's own unit-converted usage
// rates has, so every read of a subscription's period or items goes through
// v3.
func (c *client) getSubscriptionV3(ctx context.Context, subscriptionID string) (omv3.BillingSubscription, error) {
	var sub omv3.BillingSubscription
	if _, err := c.v3.do(ctx, http.MethodGet, "/subscriptions/"+url.PathEscape(subscriptionID), nil, nil, &sub); err != nil {
		return omv3.BillingSubscription{}, reclassifySubscription(err)
	}
	return sub, nil
}

// periodOf is a v3 subscription's current billing period, or nil when it
// has none.
func periodOf(sub omv3.BillingSubscription) *om.Period {
	if sub.CurrentPeriod == nil {
		return nil
	}
	return &om.Period{From: sub.CurrentPeriod.From, To: sub.CurrentPeriod.To}
}

// CancelSubscriptions ends every live subscription of the customer now:
// scheduled ones are deleted, active ones canceled immediately, and canceled
// ones (ending in the future) continued and then canceled immediately — a
// canceled subscription cannot be canceled again. A missing customer is
// treated as success.
func (c *client) CancelSubscriptions(ctx context.Context, customerKey CustomerKey) error {
	if customerKey == "" {
		return &PermanentError{Err: errors.New("customerKey is required")}
	}
	subs, err := c.ListSubscriptions(ctx, customerKey)
	if errors.Is(err, ErrCustomerNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	// Scheduled successors first: continuing a canceled subscription fails
	// while one is scheduled after it.
	for _, s := range subs {
		if s.Status != om.SubscriptionStatusScheduled {
			continue
		}
		rsp, err := c.api.DeleteSubscriptionWithResponse(ctx, s.Id)
		if err != nil {
			return classify(nil, nil, err)
		}
		if rsp.StatusCode() != http.StatusNotFound {
			if err := classifySubscription(rsp.HTTPResponse, rsp.Body); err != nil {
				return fmt.Errorf("delete scheduled subscription %s: %w", s.Id, err)
			}
		}
	}

	var immediate om.SubscriptionTiming
	if err := immediate.FromSubscriptionTimingEnum(om.SubscriptionTimingEnumImmediate); err != nil {
		return &PermanentError{Err: err}
	}
	for _, s := range subs {
		switch s.Status {
		case om.SubscriptionStatusCanceled:
			rsp, err := c.api.UnscheduleCancelationWithResponse(ctx, s.Id)
			if err != nil {
				return classify(nil, nil, err)
			}
			if err := classifySubscription(rsp.HTTPResponse, rsp.Body); err != nil {
				return fmt.Errorf("continue subscription %s before canceling: %w", s.Id, err)
			}
		case om.SubscriptionStatusActive:
		default:
			continue
		}
		rsp, err := c.api.CancelSubscriptionWithResponse(ctx, s.Id, om.CancelSubscriptionJSONRequestBody{Timing: &immediate})
		if err != nil {
			return classify(nil, nil, err)
		}
		if err := classifySubscription(rsp.HTTPResponse, rsp.Body); err != nil {
			return fmt.Errorf("cancel subscription %s: %w", s.Id, err)
		}
	}
	return nil
}

// ListSubscriptions returns every subscription of the customer, live or
// ended, newest first. Returns ErrCustomerNotFound when the customer does not
// exist.
func (c *client) ListSubscriptions(ctx context.Context, customerKey CustomerKey) ([]om.Subscription, error) {
	var all []om.Subscription
	pageSize := om.PaginationPageSize(subscriptionListPageSize)
	for page := 1; ; page++ {
		pp := om.PaginationPage(page)
		rsp, err := c.api.ListCustomerSubscriptionsWithResponse(ctx, string(customerKey), &om.ListCustomerSubscriptionsParams{
			Page:     &pp,
			PageSize: &pageSize,
		})
		if err != nil {
			return nil, classify(nil, nil, err)
		}
		if rsp.StatusCode() == http.StatusNotFound {
			return nil, ErrCustomerNotFound
		}
		if err := classify(rsp.HTTPResponse, rsp.Body, nil); err != nil {
			return nil, err
		}
		if rsp.JSON200 == nil {
			return nil, &TransientError{Err: errors.New("unexpected empty 200 response from ListCustomerSubscriptions")}
		}
		all = append(all, rsp.JSON200.Items...)
		if len(rsp.JSON200.Items) == 0 || page*subscriptionListPageSize >= rsp.JSON200.TotalCount {
			return all, nil
		}
	}
}

// classifySubscription classifies a subscription endpoint response. Two
// statuses mean something specific here and are not what classify assumes:
//
//   - 403: OpenMeter uses it for a lifecycle transition the subscription's
//     current state does not allow (e.g. canceling a canceled subscription),
//     not for authentication. It is reported as such; the caller re-reads
//     the state and decides again.
//   - 409 "invalid billing setup": the customer cannot be invoiced yet, which
//     is ErrCustomerBillingNotReady rather than a generic conflict.
func classifySubscription(resp *http.Response, body []byte) error {
	return reclassifySubscription(classify(resp, body, nil))
}

// reclassifySubscription applies classifySubscription's rules to an error
// classify already returned.
func reclassifySubscription(err error) error {
	if err == nil {
		return nil
	}
	var perm *PermanentError
	if !errors.As(err, &perm) {
		return err
	}
	switch {
	case perm.StatusCode == http.StatusForbidden:
		return &TransientError{
			Err:        fmt.Errorf("openmeter refused the subscription transition in its current state: %s", perm.ResponseBody),
			StatusCode: perm.StatusCode,
		}
	case perm.StatusCode == http.StatusConflict && strings.Contains(perm.ResponseBody, "invalid billing setup"):
		perm.Err = fmt.Errorf("%w: %s", ErrCustomerBillingNotReady, perm.ResponseBody)
		return perm
	}
	return err
}
