// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// featureListPageSize is the page size used when walking feature listings.
const featureListPageSize = 100

// DesiredFeature models an OpenMeter feature.
type DesiredFeature struct {
	Key       string
	Name      string
	MeterSlug string
	// AdvancedMeterGroupByFilters maps meter group-by dimensions to the
	// filter a usage event must satisfy to count towards the feature. Only
	// $eq and $nin are produced by this provider.
	AdvancedMeterGroupByFilters map[string]om.FilterString
	// Metadata is attached to the feature on create.
	Metadata map[string]string
}

// EnsureFeature creates the feature if no active feature exists under its
// key. Features are immutable in OpenMeter, so an existing feature is only
// accepted when its meter and filters match desired; anything else is a
// PermanentError instead of a plan silently billing the wrong usage.
func (c *client) EnsureFeature(ctx context.Context, desired DesiredFeature) (om.Feature, error) {
	if desired.Key == "" {
		return om.Feature{}, &PermanentError{Err: errors.New("DesiredFeature.Key is required")}
	}
	if desired.MeterSlug == "" {
		return om.Feature{}, &PermanentError{Err: errors.New("DesiredFeature.MeterSlug is required")}
	}

	// GET /features/{idOrKey} only resolves active features: an archived
	// feature under the same key returns 404 and is replaced by a new one.
	getResp, err := c.api.GetFeatureWithResponse(ctx, desired.Key)
	if err != nil {
		return om.Feature{}, classify(nil, nil, err)
	}
	if getResp.StatusCode() == http.StatusOK && getResp.JSON200 != nil {
		existing := *getResp.JSON200
		if err := checkFeatureMatches(existing, desired); err != nil {
			return om.Feature{}, err
		}
		return existing, nil
	}
	if getResp.StatusCode() != http.StatusNotFound {
		if err := classify(getResp.HTTPResponse, getResp.Body, nil); err != nil {
			return om.Feature{}, err
		}
		return om.Feature{}, &TransientError{Err: errors.New("unexpected empty 200 response from GetFeature")}
	}

	createReq := om.FeatureCreateInputs{
		Key:       desired.Key,
		Name:      desired.Name,
		MeterSlug: &desired.MeterSlug,
	}
	if len(desired.AdvancedMeterGroupByFilters) > 0 {
		filters := desired.AdvancedMeterGroupByFilters
		createReq.AdvancedMeterGroupByFilters = &filters
	}
	if len(desired.Metadata) > 0 {
		meta := om.Metadata(desired.Metadata)
		createReq.Metadata = &meta
	}

	createResp, err := c.api.CreateFeatureWithResponse(ctx, createReq)
	if err != nil {
		return om.Feature{}, classify(nil, nil, err)
	}
	if err := classify(createResp.HTTPResponse, createResp.Body, nil); err != nil {
		return om.Feature{}, err
	}
	if createResp.JSON201 == nil {
		return om.Feature{}, &TransientError{Err: errors.New("unexpected empty 201 response from CreateFeature")}
	}
	return *createResp.JSON201, nil
}

// checkFeatureMatches reports a PermanentError when existing does not have
// the meter and filters desired asks for.
func checkFeatureMatches(existing om.Feature, desired DesiredFeature) error {
	existingSlug := ""
	if existing.MeterSlug != nil {
		existingSlug = *existing.MeterSlug
	}
	if existingSlug != desired.MeterSlug {
		return &PermanentError{Err: fmt.Errorf(
			"feature %s is bound to meter %q, want %q", desired.Key, existingSlug, desired.MeterSlug)}
	}
	var existingFilters map[string]om.FilterString
	if existing.AdvancedMeterGroupByFilters != nil {
		existingFilters = *existing.AdvancedMeterGroupByFilters
	}
	if !FiltersEqual(existingFilters, desired.AdvancedMeterGroupByFilters) {
		return &PermanentError{Err: fmt.Errorf(
			"feature %s exists with different group-by filters; features are immutable", desired.Key)}
	}
	return nil
}

// FiltersEqual compares two group-by filter maps semantically: $in/$nin
// value order is irrelevant and empty maps equal nil.
func FiltersEqual(a, b map[string]om.FilterString) bool {
	if len(a) != len(b) {
		return false
	}
	for dim, fa := range a {
		fb, ok := b[dim]
		if !ok {
			return false
		}
		ja, errA := canonicalFilterJSON(fa)
		jb, errB := canonicalFilterJSON(fb)
		if errA != nil || errB != nil || !bytes.Equal(ja, jb) {
			return false
		}
	}
	return true
}

// canonicalFilterJSON encodes f with its set-valued operators sorted.
func canonicalFilterJSON(f om.FilterString) ([]byte, error) {
	sortedCopy := func(in *[]string) *[]string {
		if in == nil {
			return nil
		}
		out := slices.Clone(*in)
		slices.Sort(out)
		return &out
	}
	f.In = sortedCopy(f.In)
	f.Nin = sortedCopy(f.Nin)
	return json.Marshal(f)
}

// ListFeatures returns every active (non-archived) feature. When meterSlug is
// non-nil the result is filtered to features bound to that meter. Archived
// features are excluded — they still exist in OpenMeter but no longer count as
// "active" for the purposes of meter deletion, which is the only reason the
// callers here enumerate features.
func (c *client) ListFeatures(ctx context.Context, meterSlug *string) ([]om.Feature, error) {
	var all []om.Feature
	pageSize := om.PaginationPageSize(featureListPageSize)
	for page := 1; ; page++ {
		pp := om.PaginationPage(page)
		params := &om.ListFeaturesParams{Page: &pp, PageSize: &pageSize}
		if meterSlug != nil {
			params.MeterSlug = &[]string{*meterSlug}
		}
		rsp, err := c.api.ListFeaturesWithResponse(ctx, params)
		if err != nil {
			return nil, classify(nil, nil, err)
		}
		if err := classify(rsp.HTTPResponse, rsp.Body, nil); err != nil {
			return nil, err
		}
		if rsp.JSON200 == nil {
			return nil, &TransientError{Err: errors.New("unexpected empty 200 response from ListFeatures")}
		}
		// The endpoint answers with a bare array when it ignores paging and
		// a paginated envelope otherwise; accept both.
		raw, err := rsp.JSON200.MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("decode ListFeatures response: %w", err)
		}
		if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '[' {
			items, err := rsp.JSON200.AsListFeaturesResult0()
			if err != nil {
				return nil, fmt.Errorf("decode ListFeatures response: %w", err)
			}
			return append(all, items...), nil
		}
		paged, err := rsp.JSON200.AsFeaturePaginatedResponse()
		if err != nil {
			return nil, fmt.Errorf("decode ListFeatures response: %w", err)
		}
		all = append(all, paged.Items...)
		if len(paged.Items) == 0 || page*featureListPageSize >= paged.TotalCount {
			return all, nil
		}
	}
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
	return classify(rsp.HTTPResponse, rsp.Body, nil)
}

// referencedFeatureKeys returns the set of feature keys referenced by any
// non-deleted plan version in OpenMeter. It is the safety predicate for
// feature garbage collection: a feature whose key appears here must NOT be
// archived, because a plan's usage rate card still points at it. Archived
// plan versions count too — subscriptions created before a new version was
// published keep billing against them.
func (c *client) referencedFeatureKeys(ctx context.Context) (map[string]struct{}, error) {
	plans, err := c.listPlans(ctx, &om.ListPlansParams{})
	if err != nil {
		return nil, err
	}
	refs := make(map[string]struct{})
	for _, plan := range plans {
		for _, key := range PlanFeatureKeys(plan) {
			refs[key] = struct{}{}
		}
	}
	return refs, nil
}

// ArchiveFeaturesIfUnreferenced archives each member of candidateKeys that no
// non-deleted plan references. Features still referenced by a plan are left
// active. It must run AFTER the owning plan versions are deleted: until then
// they reference the candidates themselves and nothing is archived.
func (c *client) ArchiveFeaturesIfUnreferenced(ctx context.Context, candidateKeys []string) error {
	if len(candidateKeys) == 0 {
		return nil
	}
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
// meterSlug that no non-deleted plan references. It is called from the
// MeterDefinition finalizer immediately before DeleteMeter: OpenMeter rejects
// deleting a meter that still has active features (409), so the features must
// be archived first. Features still referenced by a plan are deliberately
// left alone — that plan's Offer still depends on the meter, and the
// finalizer stays in place (with the 409 surfaced) until that Offer is
// removed, which is the correct dependency ordering.
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
