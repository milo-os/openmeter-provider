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
