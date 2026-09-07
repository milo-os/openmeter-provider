// SPDX-License-Identifier: AGPL-3.0-only

// Package openmeter is a typed client for the OpenMeter metering backend.
// It wraps the generated OpenMeter Go SDK (github.com/openmeterio/openmeter/api/client/go)
// with the Milo-friendly shapes used by the MeterDefinition reconciler:
// EnsureMeter / GetMeter / DeleteMeter plus permanent-vs-transient error
// classification.
package openmeter

import (
	"context"
	"errors"
	"fmt"
	"strings"

	om "github.com/openmeterio/openmeter/api/client/go"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
)

// DesiredMeter is the desired state of an OpenMeter meter, derived from a
// MeterDefinition. It is the OpenMeter-side analogue of
// amberflo.DesiredMeter.
type DesiredMeter struct {
	// Slug is the OpenMeter meter identifier (see meterSlug, D2).
	Slug string
	// EventType is the CloudEvent/ingest type this meter aggregates. It is
	// the full reverse-DNS meterName, so ingestion keys on the canonical
	// meter identifier rather than the slug encoding.
	EventType string
	// Aggregation is the OpenMeter aggregation value.
	Aggregation om.MeterAggregation
	// Description is the human-readable description (from spec.displayName).
	Description string
	// GroupBy maps dimension keys to their JSONPath in the event data.
	GroupBy map[string]string
	// ValueProperty is the JSONPath to the numeric value in the event data.
	ValueProperty string
}

// MeterAggregation maps a Milo MeterAggregation onto the OpenMeter wire value.
// OpenMeter supports all seven Milo aggregations, so unlike Amberflo there is
// no unsupported set and no need to skip syncs.
//
//   - Sum         → SUM
//   - Count       → COUNT
//   - UniqueCount → UNIQUE_COUNT
//   - Max / Min   → MAX / MIN
//   - Latest      → LATEST
//   - Average     → AVG
func MeterAggregation(a billingv1alpha1.MeterAggregation) om.MeterAggregation {
	switch a {
	case billingv1alpha1.MeterAggregationSum:
		return om.MeterAggregationSum
	case billingv1alpha1.MeterAggregationCount:
		return om.MeterAggregationCount
	case billingv1alpha1.MeterAggregationUniqueCount:
		return om.MeterAggregationUniqueCount
	case billingv1alpha1.MeterAggregationMax:
		return om.MeterAggregationMax
	case billingv1alpha1.MeterAggregationMin:
		return om.MeterAggregationMin
	case billingv1alpha1.MeterAggregationLatest:
		return om.MeterAggregationLatest
	case billingv1alpha1.MeterAggregationAverage:
		return om.MeterAggregationAvg
	default:
		return om.MeterAggregationSum
	}
}

// Client is the typed interface over the OpenMeter meters API. It is the
// seam the MeterDefinition reconciler depends on.
type Client interface {
	// EnsureMeter creates the meter if absent, or returns the existing one.
	// NotFound is handled internally (create on first reconcile).
	EnsureMeter(ctx context.Context, desired DesiredMeter) (om.Meter, error)
	// GetMeter fetches a meter by slug. Returns ErrMeterNotFound when absent.
	GetMeter(ctx context.Context, slug string) (om.Meter, error)
	// DeleteMeter removes a meter by slug. NotFound is treated as success.
	DeleteMeter(ctx context.Context, slug string) error
}

type client struct {
	api *om.ClientWithResponses
}

// NewClient builds an OpenMeter client against the given server URL,
// authenticating with a bearer token when apiSecret is non-empty.
func NewClient(server string, apiSecret string) (Client, error) {
	if strings.TrimSpace(server) == "" {
		return nil, fmt.Errorf("openmeter: empty server URL")
	}

	var api *om.ClientWithResponses
	var err error
	if apiSecret != "" {
		api, err = om.NewAuthClientWithResponses(server, apiSecret)
	} else {
		api, err = om.NewClientWithResponses(server)
	}
	if err != nil {
		return nil, fmt.Errorf("openmeter: new client: %w", err)
	}
	return &client{api: api}, nil
}

// EnsureMeter creates the meter if it does not exist. If it already exists
// (200 on GET), the existing meter is returned unchanged — OpenMeter meter
// definitions are effectively immutable for our purposes (slug and
// aggregation cannot change), and re-creating would fail. Callers that need
// to surface drift should add an explicit Get+compare path later.
func (c *client) EnsureMeter(ctx context.Context, desired DesiredMeter) (om.Meter, error) {
	existing, err := c.GetMeter(ctx, desired.Slug)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrMeterNotFound) {
		return om.Meter{}, err
	}

	return c.createMeter(ctx, desired)
}

func (c *client) createMeter(ctx context.Context, desired DesiredMeter) (om.Meter, error) {
	description := desired.Description
	body := om.MeterCreate{
		Slug:          desired.Slug,
		EventType:     desired.EventType,
		Aggregation:   desired.Aggregation,
		ValueProperty: &desired.ValueProperty,
	}
	if description != "" {
		body.Description = &description
	}
	if len(desired.GroupBy) > 0 {
		body.GroupBy = &desired.GroupBy
	}

	resp, err := c.api.CreateMeterWithResponse(ctx, body)
	if err != nil {
		return om.Meter{}, classify(0, "", err)
	}
	if err := classify(resp.StatusCode(), string(resp.Body), nil); err != nil {
		return om.Meter{}, err
	}
	if resp.JSON201 == nil {
		return om.Meter{}, classify(resp.StatusCode(), string(resp.Body), fmt.Errorf("openmeter: create meter %q: empty response body", desired.Slug))
	}
	return *resp.JSON201, nil
}

func (c *client) GetMeter(ctx context.Context, slug string) (om.Meter, error) {
	resp, err := c.api.GetMeterWithResponse(ctx, slug)
	if err != nil {
		return om.Meter{}, classify(0, "", err)
	}
	switch resp.StatusCode() {
	case 200:
		if resp.JSON200 == nil {
			return om.Meter{}, classify(200, string(resp.Body), fmt.Errorf("openmeter: get meter %q: empty JSON body", slug))
		}
		return *resp.JSON200, nil
	case 404:
		return om.Meter{}, fmt.Errorf("%w: %s", ErrMeterNotFound, slug)
	default:
		return om.Meter{}, classify(resp.StatusCode(), string(resp.Body), nil)
	}
}

func (c *client) DeleteMeter(ctx context.Context, slug string) error {
	resp, err := c.api.DeleteMeterWithResponse(ctx, slug)
	if err != nil {
		return classify(0, "", err)
	}
	switch resp.StatusCode() {
	case 204, 200, 202:
		return nil
	case 404:
		// Already gone — treat as success.
		return nil
	default:
		return classify(resp.StatusCode(), string(resp.Body), nil)
	}
}
