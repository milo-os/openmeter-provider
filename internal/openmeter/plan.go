// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"fmt"
	"net/http"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// DesiredPlan models an OpenMeter plan we wish to ensure exists.
type DesiredPlan struct {
	// Key is a stable identifier for the Plan (often Offer.UID).
	Key string
	// Name is the display name of the Plan.
	Name string
	// Description is the plan's description.
	Description string
	// Currency defaults to "USD".
	Currency string
	// Phases contain the rate cards (Usage, Flat, etc).
	Phases []om.PlanPhase
}

// EnsurePlan creates or updates a plan by Key. OpenMeter's API doesn't have an upsert by Key,
// so we typically list/get to find if it exists, then Create or ReplaceUpdate.
// The SDK defines UpdatePlanJSONRequestBody as PlanReplaceUpdate.
func (c *client) EnsurePlan(ctx context.Context, desired DesiredPlan) (om.Plan, error) {
	currency := desired.Currency
	if currency == "" {
		currency = "USD"
	}

	createReq := om.PlanCreate{
		Key:            desired.Key,
		Name:           desired.Name,
		Description:    &desired.Description,
		Currency:       om.CurrencyCode(currency),
		BillingCadence: "P1M", // Monthly
		Phases:         desired.Phases,
	}

	// Try to find the existing plan
	existing, err := c.GetPlanByKey(ctx, desired.Key)
	if err == nil {
		// ReplaceUpdate
		updateReq := om.PlanReplaceUpdate{
			Name:           desired.Name,
			Description:    &desired.Description,
			BillingCadence: "P1M",
			Phases:         desired.Phases,
			Alignment:      existing.Alignment,
		}
		rsp, err := c.api.UpdatePlanWithResponse(ctx, existing.Id, updateReq)
		if err != nil {
			return om.Plan{}, fmt.Errorf("update plan: %w", err)
		}
		if rsp.StatusCode() != http.StatusOK {
			return om.Plan{}, c.parseErrorResponse(rsp.StatusCode(), rsp.Body)
		}
		if rsp.JSON200 == nil {
			return om.Plan{}, fmt.Errorf("unexpected empty 200 response from UpdatePlan")
		}
		return *rsp.JSON200, nil
	}

	// Create
	rsp, err := c.api.CreatePlanWithResponse(ctx, createReq)
	if err != nil {
		return om.Plan{}, fmt.Errorf("create plan: %w", err)
	}
	if rsp.StatusCode() != http.StatusCreated {
		return om.Plan{}, c.parseErrorResponse(rsp.StatusCode(), rsp.Body)
	}
	if rsp.JSON201 == nil {
		return om.Plan{}, fmt.Errorf("unexpected empty 201 response from CreatePlan")
	}
	return *rsp.JSON201, nil
}

// GetPlanByKey fetches a plan by its key.
func (c *client) GetPlanByKey(ctx context.Context, key string) (om.Plan, error) {
	rsp, err := c.api.ListPlansWithResponse(ctx, &om.ListPlansParams{
		Key: &[]string{key},
	})
	if err != nil {
		return om.Plan{}, fmt.Errorf("list plans: %w", err)
	}
	if rsp.StatusCode() != http.StatusOK {
		return om.Plan{}, c.parseErrorResponse(rsp.StatusCode(), rsp.Body)
	}
	if len(rsp.JSON200.Items) == 0 {
		return om.Plan{}, fmt.Errorf("%w: %s", ErrPlanNotFound, key)
	}
	return rsp.JSON200.Items[0], nil
}

// DeletePlan deletes a plan by its ID.
func (c *client) DeletePlan(ctx context.Context, id string) error {
	rsp, err := c.api.DeletePlanWithResponse(ctx, id)
	if err != nil {
		return fmt.Errorf("delete plan: %w", err)
	}
	if rsp.StatusCode() == http.StatusNotFound {
		return nil
	}
	if rsp.StatusCode() != http.StatusNoContent {
		return c.parseErrorResponse(rsp.StatusCode(), rsp.Body)
	}
	return nil
}

func (c *client) parseErrorResponse(code int, body []byte) error {
	err := fmt.Errorf("http %d: %s", code, string(body))
	if code >= 400 && code < 500 && code != http.StatusTooManyRequests {
		return &PermanentError{Err: err, StatusCode: code, ResponseBody: string(body)}
	}
	return err
}
