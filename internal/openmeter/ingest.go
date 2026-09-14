// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"

	cloudevents "github.com/cloudevents/sdk-go/v2"
)

// SubmitUsageBatch ingests a batch of already-validated CloudEvents into
// OpenMeter via its native application/cloudevents-batch+json endpoint.
//
// Unlike amberflo-provider's ingest API, OpenMeter accepts CloudEvents
// directly — no per-record wire-shape translation is needed; cloudevents.Event
// is a type alias for the same github.com/cloudevents/sdk-go/v2/event.Event
// the OpenMeter SDK's own request body type uses, so events are forwarded
// exactly as received from the billing pipeline.
//
// This also means the submission consumer needs neither a customerID lookup
// nor a derived idempotency key: OpenMeter attributes usage server-side by
// matching each event's Subject against a Customer's UsageAttribution
// (confirmed in OpenMeter's own source), resolves the meter via each event's
// Type (== MeterDefinition.spec.meterName — see meter.go's EnsureMeter,
// which sets EventType to the same value), and deduplicates ingestion
// natively by (source, id) (confirmed in OpenMeter's own
// openmeter/dedupe package) — the billing pipeline's CloudEvent id is
// already a stable, pre-generated ULID, so forwarding it unmodified gives
// exactly-once submission for free.
func (c *client) SubmitUsageBatch(ctx context.Context, events []cloudevents.Event) error {
	if len(events) == 0 {
		return nil
	}
	resp, err := c.api.IngestEventsWithApplicationCloudeventsBatchPlusJSONBodyWithResponse(ctx, events)
	if err != nil {
		return classify(nil, nil, err)
	}
	return classify(resp.HTTPResponse, resp.Body, nil)
}
