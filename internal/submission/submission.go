// SPDX-License-Identifier: AGPL-3.0-only

// Package submission provides the SubmissionConsumer, a manager.Runnable
// that dequeues validated CloudEvents from billing.usage.*.valid via a
// durable NATS JetStream pull consumer and submits them to OpenMeter.
//
// Mirrors amberflo-provider's internal/submission package (same NATS
// mechanics, ack/nack semantics, and Prometheus metric names), simplified
// where OpenMeter's own model makes amberflo-provider's steps unnecessary:
//
//   - No BillingAccountCache / customerID resolution. OpenMeter attributes
//     usage to a customer server-side by matching a CloudEvent's Subject
//     against Customer.UsageAttribution.SubjectKeys (confirmed in
//     OpenMeter's own source — no client-side lookup is involved).
//   - No UsageRecord wire-shape translation. OpenMeter's ingest API accepts
//     CloudEvents directly (cloudevents.Event is a type alias for the same
//     event.Event the OpenMeter SDK's request body uses), so validated
//     events are forwarded as received.
//   - No derived idempotency key. OpenMeter deduplicates ingestion natively
//     by (source, id) (confirmed in OpenMeter's own openmeter/dedupe
//     package), and the billing pipeline's CloudEvent id is already a
//     stable, pre-generated ULID (see billing/emission/cloudevents.go).
//
// A MeterDefinitionCache (Published/Deprecated meter names) is still kept:
// it lets an event for an unknown or unpublished meter be discarded before
// ever calling OpenMeter, and gives fast permanent-vs-transient
// classification without a round trip.
package submission

import (
	"context"
	"fmt"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/go-logr/logr"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

// cacheSync is the subset of cache.Cache used by SubmissionConsumer.
type cacheSync interface {
	WaitForCacheSync(ctx context.Context) bool
}

const (
	// billingUsageStream is the JetStream stream that carries validated
	// and attributed usage CloudEvents from the billing pipeline.
	billingUsageStream = "billing-usage"

	// durableConsumerName is the durable pull consumer name. Distinct from
	// amberflo-provider's "amberflo-usage-submitter" so the two providers'
	// durable consumers on the same stream track independent ack cursors —
	// each provider needs to see and ack every matching message on its own,
	// not share the other's delivery/ack state. The name is stable across
	// restarts; JetStream persists the ack sequence so the consumer resumes
	// from the last acked message after a pod restart.
	durableConsumerName = "openmeter-usage-submitter"

	// filterSubject matches all validated events across all projects.
	filterSubject = "billing.usage.*.valid"
)

// SubmissionConsumer is a manager.Runnable that dequeues validated CloudEvents
// from billing.usage.*.valid and submits them to OpenMeter.
//
// The consumer is registered in main.go only when NATSConfig is non-nil;
// existing deployments without NATS are unaffected.
type SubmissionConsumer struct {
	// Cache is used to wait for the informer cache to sync before
	// processing starts.
	Cache cacheSync

	// NC is the shared NATS connection.
	NC *natsgo.Conn

	// IngestClient submits validated events to OpenMeter.
	IngestClient openmeter.Client

	// MeterCache validates a CloudEvent's Type against known
	// Published/Deprecated MeterDefinitions before submission.
	MeterCache *MeterDefinitionCache

	// Logger is the structured logger.
	Logger logr.Logger

	// FetchBatch is the number of messages to fetch per pull. Populated
	// from OpenMeterProviderOperator.SubmissionBatchSize (defaults to 10);
	// this field's own fallback of 1 only applies if a caller leaves it
	// unset.
	FetchBatch int

	// RetryAfter is the default duration to wait before retrying on
	// transient errors.
	RetryAfter time.Duration

	// AckWait is the JetStream durable consumer's ack wait duration.
	AckWait time.Duration

	// FetchTimeout is the pull consumer Fetch call timeout.
	FetchTimeout time.Duration
}

// Start implements manager.Runnable. It blocks until ctx is cancelled,
// continuously pulling and processing messages from JetStream.
func (c *SubmissionConsumer) Start(ctx context.Context) error {
	if c.FetchBatch <= 0 {
		c.FetchBatch = 1
	}
	if c.RetryAfter <= 0 {
		c.RetryAfter = 5 * time.Second
	}
	if c.AckWait <= 0 {
		c.AckWait = 30 * time.Second
	}
	if c.FetchTimeout <= 0 {
		c.FetchTimeout = 5 * time.Second
	}

	// Wait for the informer cache to be populated so the MeterDefinition
	// cache is ready before we touch any messages.
	if !c.Cache.WaitForCacheSync(ctx) {
		return fmt.Errorf("submission consumer: cache sync failed or context cancelled")
	}

	js, err := jetstream.New(c.NC)
	if err != nil {
		return fmt.Errorf("submission consumer: create jetstream context: %w", err)
	}

	cons, err := js.CreateOrUpdateConsumer(ctx, billingUsageStream, jetstream.ConsumerConfig{
		Durable:       durableConsumerName,
		FilterSubject: filterSubject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		MaxAckPending: -1,
		AckWait:       c.AckWait,
	})
	if err != nil {
		return fmt.Errorf("submission consumer: create/update durable consumer %q: %w", durableConsumerName, err)
	}

	c.Logger.Info("submission consumer started",
		"stream", billingUsageStream,
		"consumer", durableConsumerName,
		"fetchBatch", c.FetchBatch,
		"ackWait", c.AckWait,
		"fetchTimeout", c.FetchTimeout,
		"retryAfter", c.RetryAfter,
	)

	for ctx.Err() == nil {

		msgs, err := cons.Fetch(c.FetchBatch, jetstream.FetchMaxWait(c.FetchTimeout))
		if err != nil {
			// Fetch timeout is expected when the stream is idle; continue.
			c.Logger.V(1).Info("fetch returned error (may be idle timeout)", "err", err)
			continue
		}

		// Track the oldest message in this batch to report processing lag metrics.
		var oldestTimestamp time.Time
		var batchMsgs []jetstream.Msg
		for msg := range msgs.Messages() {
			batchMsgs = append(batchMsgs, msg)
			meta, metaErr := msg.Metadata()
			if metaErr == nil && !meta.Timestamp.IsZero() {
				// Update the batch-local oldest timestamp.
				if oldestTimestamp.IsZero() || meta.Timestamp.Before(oldestTimestamp) {
					oldestTimestamp = meta.Timestamp
				}
			}
		}

		if !oldestTimestamp.IsZero() {
			setOldestUnsubmittedAge(time.Since(oldestTimestamp).Seconds())
		}

		if len(batchMsgs) > 0 {
			c.processMessages(ctx, batchMsgs)
		}

		if fetchErr := msgs.Error(); fetchErr != nil {
			c.Logger.V(1).Info("message batch error", "err", fetchErr)
		}

		// Reset the age gauge after each batch drains.
		setOldestUnsubmittedAge(0)
	}

	setOldestUnsubmittedAge(0)
	c.Logger.Info("submission consumer stopped")
	return nil
}

type permanentValidationError struct {
	err error
}

func (e permanentValidationError) Error() string {
	return e.err.Error()
}

func isPermanentValidationError(err error) bool {
	_, ok := err.(permanentValidationError)
	return ok
}

// validateEvent parses and validates a NATS message into a CloudEvent ready
// to forward to OpenMeter as-is. It logs any validation errors and returns
// a permanentValidationError for discardable issues (malformed payloads);
// a MeterDefinitionCache miss returns a plain (transient, retryable) error
// instead, since it may just be a startup race with the cache sync rather
// than a genuinely unknown meter.
func (c *SubmissionConsumer) validateEvent(msg jetstream.Msg) (*cloudevents.Event, error) {
	var ce cloudevents.Event
	if err := ce.UnmarshalJSON(msg.Data()); err != nil {
		c.Logger.Error(err, "malformed CloudEvent; discarding",
			"subject", msg.Subject(),
		)
		return nil, permanentValidationError{err: err}
	}

	c.Logger.Info("received usage event",
		"eventID", ce.ID(),
		"subject", ce.Subject(),
		"source", ce.Source(),
		"type", ce.Type(),
	)

	if ce.Subject() == "" {
		c.Logger.Error(nil, "CloudEvent missing subject; discarding",
			"eventID", ce.ID(),
		)
		return nil, permanentValidationError{err: fmt.Errorf("missing subject")}
	}

	if !c.MeterCache.IsValid(ce.Type()) {
		c.Logger.Info("MeterDefinition not found or not published for event type; nacking for retry",
			"meterName", ce.Type(),
			"eventID", ce.ID(),
		)
		return nil, fmt.Errorf("MeterDefinition %q not found or not published", ce.Type())
	}

	return &ce, nil
}

// processMessages processes a batch of JetStream messages: validates them,
// submits valid ones in a bulk request to OpenMeter, and handles transient
// and permanent errors (falling back to individual requests if a batch fails).
func (c *SubmissionConsumer) processMessages(ctx context.Context, msgs []jetstream.Msg) {
	var validatedEvents []cloudevents.Event
	var validatedMsgs []jetstream.Msg

	for _, msg := range msgs {
		ce, err := c.validateEvent(msg)
		if err != nil {
			if isPermanentValidationError(err) {
				recordSubmission("permanent")
				if ackErr := msg.Ack(); ackErr != nil {
					c.Logger.Error(ackErr, "failed to ack message", "subject", msg.Subject())
				}
			} else {
				// Transient validation error (e.g. cache miss)
				if nakErr := msg.NakWithDelay(c.RetryAfter); nakErr != nil {
					c.Logger.Error(nakErr, "failed to nack message with delay", "subject", msg.Subject())
				}
			}
			continue
		}

		validatedEvents = append(validatedEvents, *ce)
		validatedMsgs = append(validatedMsgs, msg)
	}

	if len(validatedEvents) == 0 {
		return
	}

	// Submit the batch of validated events
	submitErr := c.IngestClient.SubmitUsageBatch(ctx, validatedEvents)
	if submitErr == nil {
		c.Logger.Info("successfully submitted batch of usage events to OpenMeter", "count", len(validatedEvents))
		for _, msg := range validatedMsgs {
			recordSubmission("success")
			if ackErr := msg.Ack(); ackErr != nil {
				c.Logger.Error(ackErr, "failed to ack message", "subject", msg.Subject())
			}
		}
		return
	}

	if openmeter.IsPermanent(submitErr) {
		c.Logger.Error(submitErr, "permanent OpenMeter ingest error on batch; falling back to individual submissions to isolate error", "count", len(validatedEvents))
		// Fall back to individual submissions to isolate the bad event so we don't discard the whole batch
		for i, ce := range validatedEvents {
			msg := validatedMsgs[i]
			indivErr := c.IngestClient.SubmitUsageBatch(ctx, []cloudevents.Event{ce})
			if indivErr == nil {
				c.Logger.Info("successfully submitted usage event to OpenMeter on fallback",
					"subject", ce.Subject(),
					"type", ce.Type(),
				)
				recordSubmission("success")
				if ackErr := msg.Ack(); ackErr != nil {
					c.Logger.Error(ackErr, "failed to ack message on fallback", "subject", msg.Subject())
				}
			} else if openmeter.IsPermanent(indivErr) {
				c.Logger.Error(indivErr, "permanent OpenMeter ingest error on fallback; discarding event",
					"subject", ce.Subject(),
					"type", ce.Type(),
				)
				recordSubmission("permanent")
				if ackErr := msg.Ack(); ackErr != nil {
					c.Logger.Error(ackErr, "failed to ack message on fallback", "subject", msg.Subject())
				}
			} else {
				// Transient
				c.Logger.V(1).Info("transient OpenMeter ingest error on fallback; nacking for retry",
					"err", indivErr,
				)
				recordSubmission("transient")
				if nakErr := msg.NakWithDelay(c.RetryAfter); nakErr != nil {
					c.Logger.Error(nakErr, "failed to nack message on fallback with delay", "subject", msg.Subject())
				}
			}
		}
		return
	}

	// Transient error on batch submission (e.g. network/503/429)
	c.Logger.V(1).Info("transient OpenMeter ingest error on batch; nacking all for retry", "err", submitErr, "delay", c.RetryAfter)
	for _, msg := range validatedMsgs {
		recordSubmission("transient")
		if nakErr := msg.NakWithDelay(c.RetryAfter); nakErr != nil {
			c.Logger.Error(nakErr, "failed to nack message", "subject", msg.Subject())
		}
	}
}
