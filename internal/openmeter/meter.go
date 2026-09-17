// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	om "github.com/openmeterio/openmeter/api/client/go"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
)

// DesiredMeter is the controller-facing representation of a meter the
// reconciler wants to exist in OpenMeter. Mirrors amberflo.DesiredMeter:
// callers assemble this struct; wire encoding lives here.
type DesiredMeter struct {
	// Slug is the OpenMeter meter identifier (see MeterSlug).
	Slug string
	// EventType is the CloudEvent/ingest type this meter aggregates. It is
	// the full reverse-DNS meterName, so ingestion keys on the canonical
	// meter identifier rather than the slug encoding. Immutable once
	// created — OpenMeter rejects changing it.
	EventType string
	// Aggregation is the OpenMeter aggregation value. Immutable once
	// created — OpenMeter rejects changing it.
	Aggregation om.MeterAggregation
	// Description is the human-readable description (from
	// spec.displayName / spec.description). Mutable.
	Description string
	// GroupBy maps dimension keys to their JSONPath in the event data.
	// Mutable.
	GroupBy map[string]string
	// ValueProperty is the JSONPath to the value in the event data (see
	// valueNeedsProperty for which aggregations require it). Immutable once
	// created.
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
//
// The default case is unreachable through the API — spec.measurement.aggregation
// is CRD-enum-validated to exactly these seven values — so it can only be hit by
// this mapping falling behind a new value added to billingv1alpha1.MeterAggregation.
// That is a code bug, not a data problem: return a PermanentError rather than
// silently defaulting to Sum, which would create a meter with the wrong semantics.
func MeterAggregation(a billingv1alpha1.MeterAggregation) (om.MeterAggregation, error) {
	switch a {
	case billingv1alpha1.MeterAggregationSum:
		return om.MeterAggregationSum, nil
	case billingv1alpha1.MeterAggregationCount:
		return om.MeterAggregationCount, nil
	case billingv1alpha1.MeterAggregationUniqueCount:
		return om.MeterAggregationUniqueCount, nil
	case billingv1alpha1.MeterAggregationMax:
		return om.MeterAggregationMax, nil
	case billingv1alpha1.MeterAggregationMin:
		return om.MeterAggregationMin, nil
	case billingv1alpha1.MeterAggregationLatest:
		return om.MeterAggregationLatest, nil
	case billingv1alpha1.MeterAggregationAverage:
		return om.MeterAggregationAvg, nil
	default:
		return "", &PermanentError{Err: fmt.Errorf("unmapped MeterAggregation %q", a)}
	}
}

// valueNeedsProperty reports whether an OpenMeter aggregation requires a
// valueProperty. Per OpenMeter's own validation (validateMeterAggregation),
// COUNT is the only aggregation that forbids one — it counts events, full
// stop. Every other aggregation, UNIQUE_COUNT included, requires a
// valueProperty: SUM/MIN/MAX/AVG aggregate the numeric value it points to,
// LATEST returns it, and UNIQUE_COUNT counts distinct values of it.
func valueNeedsProperty(a om.MeterAggregation) bool {
	return a != om.MeterAggregationCount
}

// MeterSlug derives the OpenMeter meter slug from the canonical reverse-DNS
// MeterDefinition.meterName. OpenMeter slugs must match
// `^[a-z0-9]+(?:_[a-z0-9]+)*$` — lowercase alphanumeric segments joined by
// underscores, with no dots, slashes, or hyphens. Milo meterNames are
// reverse-DNS paths such as "compute.miloapis.com/instance/cpu-seconds".
//
// The transformation is deterministic and 1:1 for a given input: every '.'
// and '/' is replaced with '_' and the result is lowercased. The original
// meterName is always carried verbatim in the meter's eventType, so event
// routing never depends on the slug encoding.
func MeterSlug(meterName string) string {
	replacer := strings.NewReplacer(".", "_", "/", "_")
	return replacer.Replace(strings.ToLower(meterName))
}

// EnsureMeter creates or updates the meter so OpenMeter matches desired. The
// call is idempotent: if OpenMeter already agrees, no write happens and only
// the GET is issued.
//
// EventType, Aggregation, and ValueProperty cannot be changed once a meter
// exists (OpenMeter has no PUT support for them, matching the immutability
// of MeterDefinition.spec.measurement on the Milo side). If an existing
// meter's immutable fields disagree with desired, that is a PermanentError:
// the caller must ship a new meter under a new slug rather than mutate this
// one.
func (c *client) EnsureMeter(ctx context.Context, desired DesiredMeter) (om.Meter, error) {
	if desired.Slug == "" {
		return om.Meter{}, &PermanentError{Err: errors.New("DesiredMeter.Slug is required")}
	}
	if desired.EventType == "" {
		return om.Meter{}, &PermanentError{Err: errors.New("DesiredMeter.EventType is required")}
	}

	existing, err := c.GetMeter(ctx, desired.Slug)
	switch {
	case errors.Is(err, ErrMeterNotFound):
		return c.createMeter(ctx, desired)
	case err != nil:
		return om.Meter{}, err
	}

	if err := checkImmutableFields(existing, desired); err != nil {
		return om.Meter{}, err
	}
	if !meterNeedsUpdate(existing, desired) {
		return existing, nil
	}
	return c.updateMeter(ctx, existing.Slug, desired)
}

// checkImmutableFields reports a PermanentError when an existing meter's
// eventType, aggregation, or valueProperty disagree with desired. These
// fields cannot be changed via UpdateMeter, and Milo's own immutability
// rules on MeterDefinition.spec.measurement mean this should never happen
// legitimately — surfacing it as a PermanentError makes a drifted/hand-
// edited OpenMeter meter visible rather than silently ignored.
func checkImmutableFields(existing om.Meter, desired DesiredMeter) error {
	if existing.EventType != desired.EventType {
		return &PermanentError{Err: fmt.Errorf(
			"meter %s: eventType %q cannot change to %q", desired.Slug, existing.EventType, desired.EventType)}
	}
	if existing.Aggregation != desired.Aggregation {
		return &PermanentError{Err: fmt.Errorf(
			"meter %s: aggregation %q cannot change to %q", desired.Slug, existing.Aggregation, desired.Aggregation)}
	}
	existingValueProperty := ""
	if existing.ValueProperty != nil {
		existingValueProperty = *existing.ValueProperty
	}
	// Count-based aggregations never carry a valueProperty on the wire (see
	// valueNeedsProperty); normalize desired the same way before comparing,
	// or every reconcile of a Count/UniqueCount meter would spuriously trip
	// this check.
	wantValueProperty := ""
	if valueNeedsProperty(desired.Aggregation) {
		wantValueProperty = desired.ValueProperty
	}
	if existingValueProperty != wantValueProperty {
		return &PermanentError{Err: fmt.Errorf(
			"meter %s: valueProperty %q cannot change to %q", desired.Slug, existingValueProperty, wantValueProperty)}
	}
	return nil
}

// meterNeedsUpdate reports whether existing's mutable fields (description,
// groupBy) disagree with desired. Comparison is limited to fields
// UpdateMeter can actually change; slug/eventType/aggregation/valueProperty
// are checked separately by checkImmutableFields.
func meterNeedsUpdate(existing om.Meter, desired DesiredMeter) bool {
	existingDescription := ""
	if existing.Description != nil {
		existingDescription = *existing.Description
	}
	if existingDescription != desired.Description {
		return true
	}
	existingGroupBy := map[string]string{}
	if existing.GroupBy != nil {
		existingGroupBy = *existing.GroupBy
	}
	return !maps.Equal(existingGroupBy, desired.GroupBy)
}

// createMeter POSTs a new meter. valueProperty is only attached for
// aggregations that require one — OpenMeter returns a permanent 400 ("meter
// value property is not allowed when the aggregation is count") if it is set
// for COUNT.
func (c *client) createMeter(ctx context.Context, desired DesiredMeter) (om.Meter, error) {
	body := om.MeterCreate{
		Slug:        desired.Slug,
		EventType:   desired.EventType,
		Aggregation: desired.Aggregation,
	}
	if valueNeedsProperty(desired.Aggregation) {
		body.ValueProperty = &desired.ValueProperty
	}
	if desired.Description != "" {
		body.Description = &desired.Description
	}
	if len(desired.GroupBy) > 0 {
		body.GroupBy = &desired.GroupBy
	}

	resp, err := c.api.CreateMeterWithResponse(ctx, body)
	if err != nil {
		return om.Meter{}, classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return om.Meter{}, err
	}
	if resp.JSON201 != nil {
		return *resp.JSON201, nil
	}
	// A 2xx status with a body the SDK couldn't parse into Meter — observed
	// in practice as a genuinely empty body right after a fresh OpenMeter
	// install. The create almost certainly landed server-side regardless
	// (confirmed by a follow-up GetMeter finding it); GET is idempotent, so
	// fetch the meter we just asked for instead of reporting a failed
	// create that actually succeeded.
	created, err := c.GetMeter(ctx, desired.Slug)
	if err != nil {
		return om.Meter{}, &PermanentError{Err: fmt.Errorf(
			"create meter %q: got %s with an unparseable body, and the follow-up GetMeter also failed: %w",
			desired.Slug, resp.Status(), err)}
	}
	return created, nil
}

// updateMeter PUTs the mutable fields of an existing meter.
func (c *client) updateMeter(ctx context.Context, slug string, desired DesiredMeter) (om.Meter, error) {
	body := om.MeterUpdate{}
	if desired.Description != "" {
		body.Description = &desired.Description
	}
	if len(desired.GroupBy) > 0 {
		body.GroupBy = &desired.GroupBy
	}

	resp, err := c.api.UpdateMeterWithResponse(ctx, slug, body)
	if err != nil {
		return om.Meter{}, classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return om.Meter{}, err
	}
	if resp.JSON200 != nil {
		return *resp.JSON200, nil
	}
	// See createMeter's matching fallback: a 2xx with an unparseable body
	// means the update almost certainly still landed server-side.
	updated, err := c.GetMeter(ctx, slug)
	if err != nil {
		return om.Meter{}, &PermanentError{Err: fmt.Errorf(
			"update meter %q: got %s with an unparseable body, and the follow-up GetMeter also failed: %w",
			slug, resp.Status(), err)}
	}
	return updated, nil
}

// GetMeter fetches a meter by its slug. Returns ErrMeterNotFound when no
// such meter exists.
func (c *client) GetMeter(ctx context.Context, slug string) (om.Meter, error) {
	if slug == "" {
		return om.Meter{}, &PermanentError{Err: errors.New("slug is required")}
	}

	resp, err := c.api.GetMeterWithResponse(ctx, slug)
	if err != nil {
		return om.Meter{}, classify(nil, nil, err)
	}
	if resp.StatusCode() == 404 {
		return om.Meter{}, fmt.Errorf("%w: %s", ErrMeterNotFound, slug)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return om.Meter{}, err
	}
	if resp.JSON200 == nil {
		return om.Meter{}, &PermanentError{Err: fmt.Errorf("get meter %q: empty response body", slug)}
	}
	return *resp.JSON200, nil
}

// DeleteMeter removes a meter keyed by its slug. NotFound is treated as
// success — the desired end state is "no meter in OpenMeter", and absence
// satisfies that goal.
func (c *client) DeleteMeter(ctx context.Context, slug string) error {
	if slug == "" {
		return &PermanentError{Err: errors.New("slug is required")}
	}

	resp, err := c.api.DeleteMeterWithResponse(ctx, slug)
	if err != nil {
		return classify(nil, nil, err)
	}
	if resp.StatusCode() == 404 {
		return nil
	}
	return classify(resp.HTTPResponse, resp.Body, nil)
}
