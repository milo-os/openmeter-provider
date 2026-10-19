// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	omv3 "github.com/openmeterio/openmeter/api/v3"
)

// Plans are written and read through OpenMeter's v3 API (see v3API): a plan
// can carry unit conversions only there.

// PlanSpecHashPrefix starts the plan description, which carries the hash of
// the DesiredPlan a version was built from: "milo-spec-hash:<hash>".
// OpenMeter normalizes plans on write (ids, defaults), so comparing the
// stored plan to the desired one field by field is brittle; comparing the
// hash we wrote is not. It is not a label because OpenMeter's v3 API
// (v1.0.0-beta.232) stores plan labels but never returns them.
const PlanSpecHashPrefix = "milo-spec-hash:"

// planBillingCadence is the plan-level billing cadence. Milo bills monthly.
const planBillingCadence = "P1M"

// planListPageSize is the page size used when walking plan listings. It is
// set explicitly so the pagination loop never depends on the server default.
const planListPageSize = 100

// DesiredPlan models the OpenMeter plan we wish to have published.
type DesiredPlan struct {
	// Key is the stable identifier shared by every version of the plan
	// (the Offer UID).
	Key string
	// Name is the display name of the plan.
	Name string
	// Currency defaults to "USD".
	Currency string
	// Phases contain the rate cards (usage-based, flat fee).
	Phases []omv3.BillingPlanPhase
}

func (d DesiredPlan) currency() string {
	if d.Currency == "" {
		return "USD"
	}
	return d.Currency
}

// SpecHash returns a stable hash over every field of d that ends up on the
// plan. Two DesiredPlans with equal hashes produce identical plans.
func (d DesiredPlan) SpecHash() (string, error) {
	// encoding/json sorts map keys and the price unions serialize the bytes
	// they were built from, so the encoding is deterministic.
	raw, err := json.Marshal(struct {
		Key            string                  `json:"key"`
		Name           string                  `json:"name"`
		Currency       string                  `json:"currency"`
		BillingCadence string                  `json:"billingCadence"`
		Phases         []omv3.BillingPlanPhase `json:"phases"`
	}{d.Key, d.Name, d.currency(), planBillingCadence, d.Phases})
	if err != nil {
		return "", fmt.Errorf("hash desired plan: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// planSpecHash returns the spec hash stored on p, or "" when absent.
func planSpecHash(p omv3.BillingPlan) string {
	if p.Description == nil {
		return ""
	}
	hash, _ := strings.CutPrefix(*p.Description, PlanSpecHashPrefix)
	if hash == *p.Description {
		return ""
	}
	return hash
}

// EnsurePlan converges desired.Key to a published version matching desired.
//
// OpenMeter plans are versioned and only draft versions are editable:
//   - no version yet          -> create (draft) + publish
//   - latest is a draft       -> update it if stale + publish (recovers a
//     crash between create and publish)
//   - latest active/scheduled -> no-op when the spec hash matches, else
//     create the next version + publish; publishing archives the previous
//     active version, and existing subscriptions stay on it
//   - latest archived         -> create the next version + publish, since a
//     GA Offer must always have an assignable plan
func (c *client) EnsurePlan(ctx context.Context, desired DesiredPlan) (omv3.BillingPlan, error) {
	if desired.Key == "" {
		return omv3.BillingPlan{}, &PermanentError{Err: errors.New("DesiredPlan.Key is required")}
	}
	hash, err := desired.SpecHash()
	if err != nil {
		return omv3.BillingPlan{}, &PermanentError{Err: err}
	}

	versions, err := c.ListPlanVersions(ctx, desired.Key)
	if err != nil {
		return omv3.BillingPlan{}, err
	}
	if len(versions) == 0 {
		return c.createAndPublishPlan(ctx, desired, hash)
	}

	latest := versions[len(versions)-1]
	switch latest.Status {
	case omv3.BillingPlanStatusDraft:
		if planSpecHash(latest) != hash {
			if latest, err = c.updateDraftPlan(ctx, latest, desired, hash); err != nil {
				return omv3.BillingPlan{}, err
			}
		}
		return c.publishPlan(ctx, latest.Id)
	case omv3.BillingPlanStatusActive, omv3.BillingPlanStatusScheduled:
		if planSpecHash(latest) == hash {
			return latest, nil
		}
		return c.createAndPublishPlan(ctx, desired, hash)
	default: // archived
		return c.createAndPublishPlan(ctx, desired, hash)
	}
}

// createAndPublishPlan creates a new draft version (OpenMeter assigns the
// next version number for an existing key) and publishes it.
func (c *client) createAndPublishPlan(ctx context.Context, desired DesiredPlan, hash string) (omv3.BillingPlan, error) {
	description := PlanSpecHashPrefix + hash
	var plan omv3.BillingPlan
	if _, err := c.v3.do(ctx, http.MethodPost, "/plans", nil, omv3.CreatePlanRequest{
		Key:            desired.Key,
		Name:           desired.Name,
		Description:    &description,
		Currency:       desired.currency(),
		BillingCadence: planBillingCadence,
		Phases:         desired.Phases,
	}, &plan); err != nil {
		return omv3.BillingPlan{}, err
	}
	return c.publishPlan(ctx, plan.Id)
}

// updateDraftPlan replaces a draft version's content with desired.
func (c *client) updateDraftPlan(ctx context.Context, draft omv3.BillingPlan, desired DesiredPlan, hash string) (omv3.BillingPlan, error) {
	description := PlanSpecHashPrefix + hash
	var plan omv3.BillingPlan
	if _, err := c.v3.do(ctx, http.MethodPut, "/plans/"+url.PathEscape(draft.Id), nil, omv3.UpsertPlanRequest{
		Name:        desired.Name,
		Description: &description,
		Phases:      desired.Phases,
	}, &plan); err != nil {
		return omv3.BillingPlan{}, err
	}
	return plan, nil
}

// publishPlan makes a draft version active effective now.
func (c *client) publishPlan(ctx context.Context, id string) (omv3.BillingPlan, error) {
	var plan omv3.BillingPlan
	if _, err := c.v3.do(ctx, http.MethodPost, "/plans/"+url.PathEscape(id)+"/publish", nil, nil, &plan); err != nil {
		return omv3.BillingPlan{}, err
	}
	return plan, nil
}

// ListPlanVersions returns every non-deleted version of key ordered by
// ascending version.
func (c *client) ListPlanVersions(ctx context.Context, key string) ([]omv3.BillingPlan, error) {
	if key == "" {
		return nil, &PermanentError{Err: errors.New("plan key is required")}
	}
	plans, err := c.listPlans(ctx, key)
	if err != nil {
		return nil, err
	}
	// The key filter is not guaranteed to be an exact match; guard so a
	// foreign plan is never touched.
	out := plans[:0]
	for _, p := range plans {
		if p.Key == key {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// GetPlan returns the plan version with the given id, deleted or not.
func (c *client) GetPlan(ctx context.Context, id string) (omv3.BillingPlan, error) {
	var plan omv3.BillingPlan
	if _, err := c.v3.do(ctx, http.MethodGet, "/plans/"+url.PathEscape(id), nil, nil, &plan); err != nil {
		return omv3.BillingPlan{}, err
	}
	return plan, nil
}

// listPlans walks every page of the non-deleted plans, filtered to key when
// it is set.
func (c *client) listPlans(ctx context.Context, key string) ([]omv3.BillingPlan, error) {
	var all []omv3.BillingPlan
	for page := 1; ; page++ {
		q := url.Values{
			"page[size]":   {strconv.Itoa(planListPageSize)},
			"page[number]": {strconv.Itoa(page)},
		}
		if key != "" {
			q.Set("filter[key]", key)
		}
		var rsp omv3.PlanPagePaginatedResponse
		if _, err := c.v3.do(ctx, http.MethodGet, "/plans", q, nil, &rsp); err != nil {
			return nil, err
		}
		all = append(all, rsp.Data...)
		if len(rsp.Data) == 0 || page*planListPageSize >= int(rsp.Meta.Page.Total) {
			return all, nil
		}
	}
}

// DeletePlan deletes one plan version. OpenMeter only deletes draft,
// scheduled, or archived versions, so an active version is archived first.
func (c *client) DeletePlan(ctx context.Context, plan omv3.BillingPlan) error {
	if plan.Id == "" {
		return &PermanentError{Err: errors.New("plan id is required")}
	}
	if plan.DeletedAt != nil {
		return nil
	}
	path := "/plans/" + url.PathEscape(plan.Id)
	if plan.Status == omv3.BillingPlanStatusActive {
		status, err := c.v3.do(ctx, http.MethodPost, path+"/archive", nil, nil, nil)
		if status == http.StatusNotFound {
			return nil
		}
		if err != nil {
			return fmt.Errorf("archive plan %s: %w", plan.Id, err)
		}
	}
	status, err := c.v3.do(ctx, http.MethodDelete, path, nil, nil, nil)
	if status == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete plan %s: %w", plan.Id, err)
	}
	return nil
}

// PlanFeatureIDs returns the ids of the features plan's rate cards reference,
// in rate card order.
func PlanFeatureIDs(plan omv3.BillingPlan) []string {
	var ids []string
	for _, phase := range plan.Phases {
		for _, rc := range phase.RateCards {
			if rc.Feature != nil && rc.Feature.Id != "" {
				ids = append(ids, rc.Feature.Id)
			}
		}
	}
	return ids
}

// planHasUnitConfig reports whether any rate card of plan converts usage.
// The v1 API refuses to subscribe a customer to such a plan by change or
// migration.
func planHasUnitConfig(plan omv3.BillingPlan) bool {
	for _, phase := range plan.Phases {
		for _, rc := range phase.RateCards {
			if rc.UnitConfig != nil {
				return true
			}
		}
	}
	return false
}
