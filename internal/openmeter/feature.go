// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"fmt"
	"net/http"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// DesiredFeature models an OpenMeter feature.
type DesiredFeature struct {
	Key       string
	Name      string
	MeterSlug string
	// AdvancedMeterGroupByFilters maps dimensions to their expected values.
	AdvancedMeterGroupByFilters map[string]om.FilterString
}

// EnsureFeature creates a feature if it doesn't exist, or returns the existing one.
func (c *client) EnsureFeature(ctx context.Context, desired DesiredFeature) (om.Feature, error) {
	// 1. Try to get the feature by Key
	// OpenMeter's GetFeatureWithResponse supports lookup by ID or Key.
	getResp, err := c.api.GetFeatureWithResponse(ctx, desired.Key)
	if err != nil {
		return om.Feature{}, fmt.Errorf("get feature: %w", err)
	}

	if getResp.StatusCode() == http.StatusOK && getResp.JSON200 != nil {
		// Feature exists, OpenMeter features are mostly immutable or managed.
		// For simplicity, we just return it.
		return *getResp.JSON200, nil
	}

	if getResp.StatusCode() != http.StatusNotFound {
		return om.Feature{}, c.parseErrorResponse(getResp.StatusCode(), getResp.Body)
	}

	// 2. Feature does not exist, create it.
	createReq := om.FeatureCreateInputs{
		Key:       desired.Key,
		Name:      desired.Name,
		MeterSlug: &desired.MeterSlug,
	}

	if len(desired.AdvancedMeterGroupByFilters) > 0 {
		createReq.AdvancedMeterGroupByFilters = &desired.AdvancedMeterGroupByFilters
	}

	createResp, err := c.api.CreateFeatureWithResponse(ctx, createReq)
	if err != nil {
		return om.Feature{}, fmt.Errorf("create feature: %w", err)
	}

	if createResp.StatusCode() != http.StatusCreated {
		return om.Feature{}, c.parseErrorResponse(createResp.StatusCode(), createResp.Body)
	}

	if createResp.JSON201 == nil {
		return om.Feature{}, fmt.Errorf("unexpected empty 201 response from CreateFeature")
	}

	return *createResp.JSON201, nil
}

// ListFeatures returns every active (non-archived) feature. When meterSlug is
// non-nil the result is filtered to features bound to that meter. Archived
// features are excluded — they still exist in OpenMeter but no longer count as
// "active" for the purposes of meter deletion, which is the only reason the
// callers here enumerate features.
func (c *client) ListFeatures(ctx context.Context, meterSlug *string) ([]om.Feature, error) {
	params := &om.ListFeaturesParams{}
	if meterSlug != nil {
		params.MeterSlug = &[]string{*meterSlug}
	}
	rsp, err := c.api.ListFeaturesWithResponse(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("list features: %w", err)
	}
	if rsp.StatusCode() != http.StatusOK {
		return nil, c.parseErrorResponse(rsp.StatusCode(), rsp.Body)
	}
	if rsp.JSON200 == nil {
		return nil, fmt.Errorf("unexpected empty 200 response from ListFeatures")
	}
	features, err := rsp.JSON200.AsListFeaturesResult0()
	if err != nil {
		return nil, fmt.Errorf("unexpected ListFeatures response shape: %w", err)
	}
	return features, nil
}

// DeleteFeature archives the feature identified by id or key. OpenMeter has no
// hard feature delete in this API generation — DELETE /features/{idOrKey} sets
// archivedAt and hides the feature from default listings. 404s are treated as
// success so cleanup is idempotent.
func (c *client) DeleteFeature(ctx context.Context, idOrKey string) error {
	if idOrKey == "" {
		return &PermanentError{Err: fmt.Errorf("feature id or key is required")}
	}
	rsp, err := c.api.DeleteFeatureWithResponse(ctx, idOrKey)
	if err != nil {
		return classify(nil, nil, err)
	}
	if rsp.StatusCode() == http.StatusNotFound {
		return nil
	}
	if rsp.StatusCode() != http.StatusNoContent {
		return c.parseErrorResponse(rsp.StatusCode(), rsp.Body)
	}
	return nil
}

// referencedFeatureKeys returns the set of feature keys referenced by any live
// (non-deleted) plan in OpenMeter. It is the safety predicate for feature
// garbage collection: a feature whose key appears here must NOT be archived,
// because a plan's usage rate card still points at it. Archiving a referenced
// feature would break the referencing plan on its next reconcile.
func (c *client) referencedFeatureKeys(ctx context.Context) (map[string]struct{}, error) {
	refs := make(map[string]struct{})
	page := 1
	for {
		pp := om.PaginationPage(page)
		rsp, err := c.api.ListPlansWithResponse(ctx, &om.ListPlansParams{
			Page: &pp,
			// PageSize stays at the server default (100). Deleted plans are
			// excluded (the default), which is exactly the set we want: a
			// feature referenced only by a deleted plan is garbage.
		})
		if err != nil {
			return nil, fmt.Errorf("list plans: %w", err)
		}
		if rsp.StatusCode() != http.StatusOK {
			return nil, c.parseErrorResponse(rsp.StatusCode(), rsp.Body)
		}
		if rsp.JSON200 == nil {
			return nil, fmt.Errorf("unexpected empty 200 response from ListPlans")
		}
		for _, plan := range rsp.JSON200.Items {
			for _, phase := range plan.Phases {
				for _, rc := range phase.RateCards {
					usage, err := rc.AsRateCardUsageBased()
					if err != nil {
						// Flat-fee and other rate cards carry no feature
						// reference; skip them.
						continue
					}
					if usage.FeatureKey != nil && *usage.FeatureKey != "" {
						refs[*usage.FeatureKey] = struct{}{}
					}
				}
			}
		}
		if page*100 >= rsp.JSON200.TotalCount {
			break
		}
		page++
	}
	return refs, nil
}

// ArchiveFeaturesIfUnreferenced archives each member of candidateKeys that no
// live plan references. Features still referenced by a plan are left active.
// This is the safe cleanup primitive for both the Offer delete path (the
// plan's features become garbage once the plan is gone) and the MeterDefinition
// delete path (features bound to the meter must be archived before the meter
// can be deleted — OpenMeter rejects deleting a meter with active features).
func (c *client) ArchiveFeaturesIfUnreferenced(ctx context.Context, candidateKeys []string) error {
	refs, err := c.referencedFeatureKeys(ctx)
	if err != nil {
		return err
	}
	for _, key := range candidateKeys {
		if _, used := refs[key]; used {
			continue
		}
		if err := c.DeleteFeature(ctx, key); err != nil {
			return fmt.Errorf("archive feature %q: %w", key, err)
		}
	}
	return nil
}

// ArchiveUnreferencedMeterFeatures archives every active feature bound to
// meterSlug that no live plan references. It is called from the
// MeterDefinition finalizer immediately before DeleteMeter: OpenMeter rejects
// deleting a meter that still has active features (409), so the features must
// be archived first. Features still referenced by a live plan are deliberately
// left alone — that plan's Offer is still dependant on the meter, and the
// finalizer stays in place (with the 409 surfaced) until that Offer is removed,
// which is the correct dependency ordering.
func (c *client) ArchiveUnreferencedMeterFeatures(ctx context.Context, meterSlug string) error {
	features, err := c.ListFeatures(ctx, &meterSlug)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(features))
	for _, f := range features {
		keys = append(keys, f.Key)
	}
	return c.ArchiveFeaturesIfUnreferenced(ctx, keys)
}
