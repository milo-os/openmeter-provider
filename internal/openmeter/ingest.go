// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	cloudevents "github.com/cloudevents/sdk-go/v2"
)

// RequestTimeout is the upper bound of a single OpenMeter API call made by
// this client (see NewClient). Exported so callers holding external leases
// across calls (the submission consumer's JetStream ack deadline) can size
// them against it.
const RequestTimeout = requestTimeout

// ErrEventRejected marks a SubmitUsageBatch failure where OpenMeter rejected
// the submitted event(s) themselves as invalid (400 / 422 validation
// errors). Only this class of failure may ever lead a caller to discard an
// event: resubmitting the same payload can never succeed. Test with
// errors.Is or IsEventRejected.
var ErrEventRejected = errors.New("openmeter: event rejected as invalid")

// ErrPayloadTooLarge marks a SubmitUsageBatch failure where OpenMeter
// refused the request body as too large (413). Splitting the batch may
// succeed; resubmitting it unchanged will not.
var ErrPayloadTooLarge = errors.New("openmeter: ingest payload too large")

// IsEventRejected reports whether err is a SubmitUsageBatch failure caused
// by OpenMeter rejecting the event payload itself as invalid.
func IsEventRejected(err error) bool {
	return errors.Is(err, ErrEventRejected)
}

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
// (confirmed in OpenMeter's own source), and resolves the meter via each
// event's Type (== MeterDefinition.spec.meterName — see meter.go's
// EnsureMeter, which sets EventType to the same value).
//
// Deduplication is NOT automatic. OpenMeter only deduplicates by
// (namespace, source, id) across flushes when its sink-worker runs with
// `sink.dedupe` enabled on a persistent driver (redis); by default it only
// drops duplicates that land in the same sink flush (a ~2s / 500-message
// window, openmeter/sink dedupeSinkMessages). Our OpenMeter HelmRelease
// (config/dependencies/openmeter/helmrelease.yaml) enables the redis sink
// dedupe with a window longer than billing's 30-day replay window, which is
// what makes forwarding the billing pipeline's stable ULID ids unmodified
// safe against JetStream redeliveries and operator replays.
//
// Error classification on top of classify: a 400/422 that is not a
// content-type complaint wraps ErrEventRejected, a 413 wraps
// ErrPayloadTooLarge. Every other non-2xx keeps its classify shape — in
// particular 401/403/404/405/415 are PermanentErrors that are NOT
// ErrEventRejected: they indicate a configuration problem (bad secret,
// wrong URL), never a bad event, and must not cause usage to be dropped.
func (c *client) SubmitUsageBatch(ctx context.Context, events []cloudevents.Event) error {
	if len(events) == 0 {
		return nil
	}
	resp, err := c.api.IngestEventsWithApplicationCloudeventsBatchPlusJSONBodyWithResponse(ctx, events)
	if err != nil {
		return classify(nil, nil, err)
	}
	return classifyIngest(classify(resp.HTTPResponse, resp.Body, nil))
}

// classifyIngest refines a classify result for the ingest endpoint (see
// SubmitUsageBatch).
//
// OpenMeter's ingest handler answers both an undecodable event and an
// unsupported Content-Type with 400 (openmeter/ingest/httpdriver
// ErrorInvalidEvent / ErrorInvalidContentType), and its OpenAPI request
// validator answers schema violations with 400 too. The content-type case
// is a client bug that applies to every request, so it is deliberately
// left out of ErrEventRejected.
func classifyIngest(err error) error {
	var perm *PermanentError
	if !errors.As(err, &perm) {
		return err
	}
	switch perm.StatusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		if strings.Contains(strings.ToLower(perm.ResponseBody), "invalid content type") {
			return err
		}
		perm.Err = fmt.Errorf("%w: %w", ErrEventRejected, perm.Err)
	case http.StatusRequestEntityTooLarge:
		perm.Err = fmt.Errorf("%w: %w", ErrPayloadTooLarge, perm.Err)
	}
	return err
}
