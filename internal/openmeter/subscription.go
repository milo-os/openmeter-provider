// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	om "github.com/openmeterio/openmeter/api/client/go"
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

	versions, err := c.ListPlanVersions(ctx, desired.PlanKey, false)
	if err != nil {
		return SubscriptionState{}, fmt.Errorf("list plan versions: %w", err)
	}
	activeVersion := 0
	for _, v := range versions {
		if v.Status == om.PlanStatusActive {
			activeVersion = v.Version
		}
	}
	if activeVersion == 0 {
		return SubscriptionState{}, fmt.Errorf("plan %q: %w", desired.PlanKey, ErrPlanNotPublished)
	}

	for range maxSubscriptionSteps {
		subs, err := c.ListSubscriptions(ctx, desired.CustomerKey)
		if err != nil {
			return SubscriptionState{}, err
		}
		step := nextSubscriptionStep(subs, desired.PlanKey, activeVersion)
		if step.kind == stepNone {
			return SubscriptionState{Current: *step.current, Pending: step.pending, PlanVersion: activeVersion}, nil
		}
		if err := c.applySubscriptionStep(ctx, desired, step, activeVersion); err != nil {
			return SubscriptionState{}, fmt.Errorf("%s subscription: %w", step.kind, err)
		}
	}
	return SubscriptionState{}, &TransientError{Err: fmt.Errorf(
		"subscription for customer %s did not converge after %d steps", desired.CustomerKey, maxSubscriptionSteps)}
}

func (c *client) applySubscriptionStep(ctx context.Context, desired DesiredSubscription, step subscriptionStep, activeVersion int) error {
	metadata := om.Metadata(desired.Metadata)
	switch step.kind {
	case stepCreate:
		key := string(desired.CustomerKey)
		var body om.SubscriptionCreate
		if err := body.FromPlanSubscriptionCreate(om.PlanSubscriptionCreate{
			CustomerKey: &key,
			// Version omitted: OpenMeter subscribes to the version active
			// now, which avoids racing a concurrent publish.
			Plan:     om.PlanReferenceInput{Key: desired.PlanKey},
			Metadata: &metadata,
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
		if err := timing.FromSubscriptionTimingEnum(om.SubscriptionTimingEnumImmediate); err != nil {
			return &PermanentError{Err: err}
		}
		var body om.SubscriptionChange
		if err := body.FromPlanSubscriptionChange(om.PlanSubscriptionChange{
			Plan:     om.PlanReferenceInput{Key: desired.PlanKey},
			Timing:   timing,
			Metadata: &metadata,
		}); err != nil {
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
		target := activeVersion
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
	err := classify(resp, body, nil)
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
