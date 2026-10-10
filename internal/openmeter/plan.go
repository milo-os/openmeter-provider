// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"sort"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// PlanSpecHashMetadataKey is the plan metadata key carrying the hash of the
// DesiredPlan a version was built from. OpenMeter normalizes plans on write
// (ids, defaults, expanded features), so comparing the stored plan to the
// desired one field by field is brittle; comparing the hash we wrote is not.
const PlanSpecHashMetadataKey = "openmeter.miloapis.com/spec-hash"

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
	// Description is the plan's description.
	Description string
	// Currency defaults to "USD".
	Currency string
	// Metadata is attached to the plan. PlanSpecHashMetadataKey is reserved
	// and always overwritten.
	Metadata map[string]string
	// Phases contain the rate cards (usage-based, flat fee).
	Phases []om.PlanPhase
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
	meta := make(map[string]string, len(d.Metadata))
	for k, v := range d.Metadata {
		if k != PlanSpecHashMetadataKey {
			meta[k] = v
		}
	}
	// encoding/json sorts map keys and the rate card unions serialize the
	// bytes they were built from, so the encoding is deterministic.
	raw, err := json.Marshal(struct {
		Key            string            `json:"key"`
		Name           string            `json:"name"`
		Description    string            `json:"description"`
		Currency       string            `json:"currency"`
		BillingCadence string            `json:"billingCadence"`
		Metadata       map[string]string `json:"metadata"`
		Phases         []om.PlanPhase    `json:"phases"`
	}{d.Key, d.Name, d.Description, d.currency(), planBillingCadence, meta, d.Phases})
	if err != nil {
		return "", fmt.Errorf("hash desired plan: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// metadataWithHash returns d.Metadata plus the spec hash.
func (d DesiredPlan) metadataWithHash(hash string) om.Metadata {
	meta := make(om.Metadata, len(d.Metadata)+1)
	maps.Copy(meta, d.Metadata)
	meta[PlanSpecHashMetadataKey] = hash
	return meta
}

// planSpecHash returns the spec hash stored on p, or "" when absent.
func planSpecHash(p om.Plan) string {
	if p.Metadata == nil {
		return ""
	}
	return (*p.Metadata)[PlanSpecHashMetadataKey]
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
func (c *client) EnsurePlan(ctx context.Context, desired DesiredPlan) (om.Plan, error) {
	if desired.Key == "" {
		return om.Plan{}, &PermanentError{Err: errors.New("DesiredPlan.Key is required")}
	}
	hash, err := desired.SpecHash()
	if err != nil {
		return om.Plan{}, &PermanentError{Err: err}
	}

	versions, err := c.ListPlanVersions(ctx, desired.Key, false)
	if err != nil {
		return om.Plan{}, err
	}
	if len(versions) == 0 {
		return c.createAndPublishPlan(ctx, desired, hash)
	}

	latest := versions[len(versions)-1]
	switch latest.Status {
	case om.PlanStatusDraft:
		if planSpecHash(latest) != hash {
			if latest, err = c.updateDraftPlan(ctx, latest, desired, hash); err != nil {
				return om.Plan{}, err
			}
		}
		return c.publishPlan(ctx, latest.Id)
	case om.PlanStatusActive, om.PlanStatusScheduled:
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
func (c *client) createAndPublishPlan(ctx context.Context, desired DesiredPlan, hash string) (om.Plan, error) {
	meta := desired.metadataWithHash(hash)
	description := desired.Description
	rsp, err := c.api.CreatePlanWithResponse(ctx, om.PlanCreate{
		Key:            desired.Key,
		Name:           desired.Name,
		Description:    &description,
		Currency:       om.CurrencyCode(desired.currency()),
		BillingCadence: planBillingCadence,
		Metadata:       &meta,
		Phases:         desired.Phases,
	})
	if err != nil {
		return om.Plan{}, classify(nil, nil, err)
	}
	if err := classify(rsp.HTTPResponse, rsp.Body, nil); err != nil {
		return om.Plan{}, err
	}
	if rsp.JSON201 == nil {
		return om.Plan{}, &TransientError{Err: errors.New("unexpected empty 201 response from CreatePlan")}
	}
	return c.publishPlan(ctx, rsp.JSON201.Id)
}

// updateDraftPlan replaces a draft version's content with desired.
func (c *client) updateDraftPlan(ctx context.Context, draft om.Plan, desired DesiredPlan, hash string) (om.Plan, error) {
	meta := desired.metadataWithHash(hash)
	description := desired.Description
	rsp, err := c.api.UpdatePlanWithResponse(ctx, draft.Id, om.PlanReplaceUpdate{
		Name:           desired.Name,
		Description:    &description,
		BillingCadence: planBillingCadence,
		Metadata:       &meta,
		Phases:         desired.Phases,
		Alignment:      draft.Alignment,
	})
	if err != nil {
		return om.Plan{}, classify(nil, nil, err)
	}
	if err := classify(rsp.HTTPResponse, rsp.Body, nil); err != nil {
		return om.Plan{}, err
	}
	if rsp.JSON200 == nil {
		return om.Plan{}, &TransientError{Err: errors.New("unexpected empty 200 response from UpdatePlan")}
	}
	return *rsp.JSON200, nil
}

// publishPlan makes a draft version active effective now.
func (c *client) publishPlan(ctx context.Context, id string) (om.Plan, error) {
	rsp, err := c.api.PublishPlanWithResponse(ctx, id)
	if err != nil {
		return om.Plan{}, classify(nil, nil, err)
	}
	if err := classify(rsp.HTTPResponse, rsp.Body, nil); err != nil {
		return om.Plan{}, err
	}
	if rsp.JSON200 == nil {
		return om.Plan{}, &TransientError{Err: errors.New("unexpected empty 200 response from PublishPlan")}
	}
	return *rsp.JSON200, nil
}

// ListPlanVersions returns every version of key ordered by ascending version.
func (c *client) ListPlanVersions(ctx context.Context, key string, includeDeleted bool) ([]om.Plan, error) {
	if key == "" {
		return nil, &PermanentError{Err: errors.New("plan key is required")}
	}
	params := &om.ListPlansParams{Key: &[]string{key}}
	if includeDeleted {
		params.IncludeDeleted = &includeDeleted
	}
	plans, err := c.listPlans(ctx, params)
	if err != nil {
		return nil, err
	}
	// ListPlans filters by key as a prefix-free exact match, but guard
	// against a server that does not so a foreign plan is never touched.
	out := plans[:0]
	for _, p := range plans {
		if p.Key == key {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// listPlans walks every page of a plan listing.
func (c *client) listPlans(ctx context.Context, params *om.ListPlansParams) ([]om.Plan, error) {
	var all []om.Plan
	pageSize := om.PaginationPageSize(planListPageSize)
	for page := 1; ; page++ {
		pp := om.PaginationPage(page)
		p := *params
		p.Page = &pp
		p.PageSize = &pageSize
		rsp, err := c.api.ListPlansWithResponse(ctx, &p)
		if err != nil {
			return nil, classify(nil, nil, err)
		}
		if err := classify(rsp.HTTPResponse, rsp.Body, nil); err != nil {
			return nil, err
		}
		if rsp.JSON200 == nil {
			return nil, &TransientError{Err: errors.New("unexpected empty 200 response from ListPlans")}
		}
		all = append(all, rsp.JSON200.Items...)
		if len(rsp.JSON200.Items) == 0 || page*planListPageSize >= rsp.JSON200.TotalCount {
			return all, nil
		}
	}
}

// DeletePlan deletes one plan version. OpenMeter only deletes draft,
// scheduled, or archived versions, so an active version is archived first.
func (c *client) DeletePlan(ctx context.Context, plan om.Plan) error {
	if plan.Id == "" {
		return &PermanentError{Err: errors.New("plan id is required")}
	}
	if plan.DeletedAt != nil {
		return nil
	}
	if plan.Status == om.PlanStatusActive {
		rsp, err := c.api.ArchivePlanWithResponse(ctx, plan.Id)
		if err != nil {
			return classify(nil, nil, err)
		}
		if rsp.StatusCode() == http.StatusNotFound {
			return nil
		}
		if err := classify(rsp.HTTPResponse, rsp.Body, nil); err != nil {
			return fmt.Errorf("archive plan %s: %w", plan.Id, err)
		}
	}
	rsp, err := c.api.DeletePlanWithResponse(ctx, plan.Id)
	if err != nil {
		return classify(nil, nil, err)
	}
	if rsp.StatusCode() == http.StatusNotFound {
		return nil
	}
	if err := classify(rsp.HTTPResponse, rsp.Body, nil); err != nil {
		return fmt.Errorf("delete plan %s: %w", plan.Id, err)
	}
	return nil
}

// PlanFeatureKeys returns the feature keys referenced by plan's rate cards,
// in rate card order. Both rate card types may reference a feature (flat
// fees optionally do, for entitlements); garbage collection must honor
// either, so both are reported.
func PlanFeatureKeys(plan om.Plan) []string {
	var keys []string
	for _, phase := range plan.Phases {
		for _, rc := range phase.RateCards {
			if key := rateCardFeatureKey(rc); key != "" {
				keys = append(keys, key)
			}
		}
	}
	return keys
}

// rateCardFeatureKey returns the feature key rc points at, or "". The
// discriminator is checked explicitly: the As* conversions on the union
// decode any JSON object and do not fail on a type mismatch.
func rateCardFeatureKey(rc om.RateCard) string {
	disc, err := rc.Discriminator()
	if err != nil {
		return ""
	}
	var key *string
	switch disc {
	case string(om.RateCardUsageBasedTypeUsageBased):
		if usage, err := rc.AsRateCardUsageBased(); err == nil {
			key = usage.FeatureKey
		}
	case string(om.RateCardFlatFeeTypeFlatFee):
		if flat, err := rc.AsRateCardFlatFee(); err == nil {
			key = flat.FeatureKey
		}
	}
	if key == nil {
		return ""
	}
	return *key
}
