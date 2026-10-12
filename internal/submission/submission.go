// SPDX-License-Identifier: AGPL-3.0-only

// Package submission provides the SubmissionConsumer, a manager.Runnable
// that dequeues validated CloudEvents from billing.usage.*.valid via a
// durable NATS JetStream pull consumer and submits them to OpenMeter.
//
// Mirrors amberflo-provider's internal/submission package (same NATS
// mechanics, ack/nack semantics, and Prometheus metric names), simplified
// where OpenMeter's own model makes amberflo-provider's steps unnecessary:
//
//   - Attribution by billing account. OpenMeter attributes a usage event to
//     the customer whose key (or one of whose usageAttribution subject keys)
//     equals the event's subject. Billing emits events with subject
//     "projects/<name>" and stamps the BillingAccount it attributed them to
//     in the billingaccountref extension; the consumer resolves that account
//     (AccountResolver) and rewrites the subject to its UID — the OpenMeter
//     customer key. Customers therefore need no subject keys, which matters
//     because OpenMeter refuses subject-key changes while a customer has a
//     live subscription: attributing by project would freeze an account's
//     project set the moment it subscribes. The project stays available as
//     the project_name dimension billing injects into the event data.
//   - No UsageRecord wire-shape translation. OpenMeter's ingest API accepts
//     CloudEvents directly (cloudevents.Event is a type alias for the same
//     event.Event the OpenMeter SDK's request body uses), so validated
//     events are forwarded as received.
//   - No derived idempotency key. The billing pipeline's CloudEvent id is a
//     stable, pre-generated ULID (see billing/emission/cloudevents.go), and
//     OpenMeter's sink-worker deduplicates by (namespace, source, id) — but
//     ONLY because our OpenMeter install enables `sink.dedupe` with a redis
//     driver and a window longer than billing's 30-day replay window
//     (config/dependencies/openmeter/helmrelease.yaml). OpenMeter's default
//     only drops duplicates within a single ~2s sink flush; an OpenMeter
//     deployed without that setting double-bills every redelivery.
//   - No MeterDefinition gate. Billing has already validated every event on
//     the .valid subject, and OpenMeter stores events regardless of the
//     state of the meter that will aggregate them, so events are forwarded
//     even when their MeterDefinition is not (or no longer) Published: late
//     usage for a Deprecated or Retired meter must still be billed, and
//     holding events back for a meter that never appears would only stall
//     them until the stream's retention silently expired them.
//
// Usage is never dropped silently. An event is discarded (acked without a
// successful submission) only when it is structurally unusable (not a
// CloudEvent, no subject) or when OpenMeter rejects that single event as
// invalid (400/422) — each logged at error level and counted. Every other
// failure — transient (network, 5xx, 429) or configuration (401/403/404/...)
// — is NAKed with a capped exponential delay and retried.
package submission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/go-logr/logr"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

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

	// maxDeliver bounds how often JetStream redelivers one message — a
	// safety net against a poison message cycling forever, sized so it
	// never triggers before the stream's own (>= 30 day) retention would:
	// with NAK delays capped at maxNakDelay, 5000 deliveries span ~34 days.
	// A message that does exhaust it is no longer delivered; the
	// MAX_DELIVERIES advisory is logged at error level and counted (see
	// handleMaxDeliveriesAdvisory) so the event can be replayed.
	maxDeliver = 5000

	// maxNakDelay caps the exponential NAK backoff (RetryAfter doubled per
	// delivery), so a long OpenMeter outage retries every 10 minutes
	// instead of hammering it — and so maxDeliver spans the retention.
	maxNakDelay = 10 * time.Minute

	// maxFetchBackoff caps the delay between failing Fetch calls.
	maxFetchBackoff = 30 * time.Second

	// maxDeliveriesAdvisorySubject is the JetStream advisory published when
	// a message of our consumer exceeds maxDeliver.
	maxDeliveriesAdvisorySubject = "$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES." +
		billingUsageStream + "." + durableConsumerName
)

// Submission outcomes, used as the status label of
// billing_submission_requests_total.
const (
	// outcomeSuccess: submitted to OpenMeter, acked.
	outcomeSuccess = "success"
	// outcomeTransient: network/5xx/429, NAKed for retry.
	outcomeTransient = "transient"
	// outcomePermanent: the event itself is unusable (malformed, missing
	// subject, or rejected by OpenMeter as invalid) — acked and dropped.
	outcomePermanent = "permanent"
	// outcomeConfigError: OpenMeter refused the request for a reason that
	// is not the event's fault (401/403/404/405/415, ...), i.e. a bad
	// secret or URL. NAKed for retry; never dropped.
	outcomeConfigError = "config_error"
	// outcomeUnattributed: the event's BillingAccount could not be resolved
	// (not in the cache yet, or the event lacks billing's attribution).
	// NAKed for retry; never dropped.
	outcomeUnattributed = "unattributed"
	// outcomeMaxDeliveries: JetStream gave up redelivering a message after
	// maxDeliver attempts. The usage was NOT submitted.
	outcomeMaxDeliveries = "max_deliveries_exceeded"
)

// SubmissionConsumer is a manager.Runnable that dequeues validated CloudEvents
// from billing.usage.*.valid and submits them to OpenMeter.
type SubmissionConsumer struct {
	// NC is the shared NATS connection.
	NC *natsgo.Conn

	// IngestClient submits validated events to OpenMeter.
	IngestClient openmeter.Client

	// Accounts resolves the BillingAccount each event is billed to; its UID
	// becomes the event's subject. Required.
	Accounts AccountResolver

	// Logger is the structured logger.
	Logger logr.Logger

	// FetchBatch is the number of messages to fetch per pull. Populated
	// from OpenMeterProviderOperator.SubmissionBatchSize (defaults to 10);
	// this field's own fallback of 1 only applies if a caller leaves it
	// unset.
	FetchBatch int

	// RetryAfter is the base delay before a failed message is redelivered;
	// it doubles per delivery up to maxNakDelay.
	RetryAfter time.Duration

	// AckWait is the JetStream durable consumer's ack wait duration. Raised
	// to minAckWait if configured lower (see Start).
	AckWait time.Duration

	// FetchTimeout is the pull consumer Fetch call timeout.
	FetchTimeout time.Duration
}

// minAckWait is the shortest AckWait under which no message can be
// redelivered (possibly to another replica) while this consumer is still
// working on it. A message's ack timer starts when it is delivered into a
// Fetch, which can wait up to FetchTimeout before returning; the batch
// submission then takes up to one OpenMeter request timeout. From then on
// processMessages extends the deadline of every still-pending message
// (msg.InProgress) before each further OpenMeter call, so the longest
// stretch without an extension is FetchTimeout + one request — doubled
// here for margin.
func (c *SubmissionConsumer) minAckWait() time.Duration {
	return 2 * (c.FetchTimeout + openmeter.RequestTimeout)
}

// Start implements manager.Runnable. It blocks until ctx is cancelled,
// continuously pulling and processing messages from JetStream. It returns
// an error — making the manager exit so the pod restarts — when the NATS
// connection is permanently closed or the durable consumer disappears.
func (c *SubmissionConsumer) Start(ctx context.Context) error {
	if c.Accounts == nil {
		return errors.New("submission consumer: Accounts resolver is required")
	}
	if c.FetchBatch <= 0 {
		c.FetchBatch = 1
	}
	if c.RetryAfter <= 0 {
		c.RetryAfter = 5 * time.Second
	}
	if c.FetchTimeout <= 0 {
		c.FetchTimeout = 5 * time.Second
	}
	if floor := c.minAckWait(); c.AckWait < floor {
		if c.AckWait > 0 {
			c.Logger.Info("submissionAckWait is shorter than one fetch plus OpenMeter call allows; raising it",
				"configured", c.AckWait, "effective", floor)
		}
		c.AckWait = floor
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
		MaxDeliver:    maxDeliver,
	})
	if err != nil {
		return fmt.Errorf("submission consumer: create/update durable consumer %q: %w", durableConsumerName, err)
	}

	// A queue subscription, so exactly one replica logs and counts each
	// advisory.
	advisorySub, err := c.NC.QueueSubscribe(maxDeliveriesAdvisorySubject, durableConsumerName, func(m *natsgo.Msg) {
		c.handleMaxDeliveriesAdvisory(m.Data)
	})
	if err != nil {
		return fmt.Errorf("submission consumer: subscribe to %q: %w", maxDeliveriesAdvisorySubject, err)
	}
	defer func() { _ = advisorySub.Unsubscribe() }()

	c.Logger.Info("submission consumer started",
		"stream", billingUsageStream,
		"consumer", durableConsumerName,
		"fetchBatch", c.FetchBatch,
		"ackWait", c.AckWait,
		"maxDeliver", maxDeliver,
		"fetchTimeout", c.FetchTimeout,
		"retryAfter", c.RetryAfter,
	)

	failures := 0
	for ctx.Err() == nil {
		if c.NC.IsClosed() {
			return errors.New("submission consumer: NATS connection is closed")
		}

		msgs, err := cons.Fetch(c.FetchBatch, jetstream.FetchMaxWait(c.FetchTimeout))
		if err != nil {
			if fatalFetchError(err) {
				return fmt.Errorf("submission consumer: fetch: %w", err)
			}
			failures++
			c.Logger.Error(err, "fetch from JetStream failed; backing off", "consecutiveFailures", failures)
			sleepCtx(ctx, fetchBackoff(failures))
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

		// Reset the age gauge after each batch drains.
		setOldestUnsubmittedAge(0)

		batchErr := msgs.Error()
		switch {
		case batchErr == nil || idleFetchError(batchErr):
			failures = 0
		case fatalFetchError(batchErr):
			return fmt.Errorf("submission consumer: fetch: %w", batchErr)
		default:
			failures++
			c.Logger.Error(batchErr, "JetStream fetch batch ended with an error; backing off",
				"consecutiveFailures", failures)
			sleepCtx(ctx, fetchBackoff(failures))
		}
	}

	setOldestUnsubmittedAge(0)
	c.Logger.Info("submission consumer stopped")
	return nil
}

// idleFetchError reports whether a fetch batch error only means that no
// message arrived before the fetch expired — the normal idle case.
func idleFetchError(err error) bool {
	return errors.Is(err, natsgo.ErrTimeout) ||
		errors.Is(err, jetstream.ErrNoMessages) ||
		errors.Is(err, context.DeadlineExceeded)
}

// fatalFetchError reports whether a fetch error cannot recover in-process:
// the connection is permanently closed, or the durable consumer is gone
// (a restart recreates it in Start).
func fatalFetchError(err error) bool {
	return errors.Is(err, natsgo.ErrConnectionClosed) ||
		errors.Is(err, jetstream.ErrConnectionClosed) ||
		errors.Is(err, jetstream.ErrConsumerDeleted) ||
		errors.Is(err, jetstream.ErrConsumerNotFound)
}

// fetchBackoff is the delay after the n-th consecutive fetch failure:
// 1s, 2s, 4s, ... capped at maxFetchBackoff.
func fetchBackoff(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	d := time.Second
	for i := 1; i < n && d < maxFetchBackoff; i++ {
		d *= 2
	}
	return min(d, maxFetchBackoff)
}

// sleepCtx sleeps for d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// maxDeliveriesAdvisory is the subset of JetStream's
// io.nats.jetstream.advisory.v1.max_deliver payload we log.
type maxDeliveriesAdvisory struct {
	Stream     string `json:"stream"`
	Consumer   string `json:"consumer"`
	StreamSeq  uint64 `json:"stream_seq"`
	Deliveries uint64 `json:"deliveries"`
}

// handleMaxDeliveriesAdvisory logs and counts a message JetStream will no
// longer deliver. Its usage was never submitted: an operator must replay it
// (stream sequence in the log) once the cause is fixed.
func (c *SubmissionConsumer) handleMaxDeliveriesAdvisory(data []byte) {
	recordSubmission(outcomeMaxDeliveries)
	var adv maxDeliveriesAdvisory
	if err := json.Unmarshal(data, &adv); err != nil {
		c.Logger.Error(err, "usage event exhausted its JetStream deliveries and was NOT submitted to OpenMeter; unparseable advisory",
			"advisory", string(data))
		return
	}
	c.Logger.Error(nil, "usage event exhausted its JetStream deliveries and was NOT submitted to OpenMeter; replay it once the cause is fixed",
		"stream", adv.Stream,
		"consumer", adv.Consumer,
		"streamSeq", adv.StreamSeq,
		"deliveries", adv.Deliveries,
	)
}

type permanentValidationError struct {
	err error
}

func (e permanentValidationError) Error() string {
	return e.err.Error()
}

// validateEvent parses and validates a NATS message into a CloudEvent ready
// to forward to OpenMeter as-is. Every failure is a permanentValidationError:
// a payload that is not a CloudEvent, or has no subject (OpenMeter's
// customer attribution key), can never be submitted successfully.
func (c *SubmissionConsumer) validateEvent(msg jetstream.Msg) (*cloudevents.Event, error) {
	var ce cloudevents.Event
	if err := ce.UnmarshalJSON(msg.Data()); err != nil {
		c.Logger.Error(err, "malformed CloudEvent; discarding",
			"subject", msg.Subject(),
		)
		return nil, permanentValidationError{err: err}
	}

	c.Logger.V(1).Info("received usage event",
		"eventID", ce.ID(),
		"subject", ce.Subject(),
		"source", ce.Source(),
		"type", ce.Type(),
	)

	if ce.Subject() == "" {
		c.Logger.Error(nil, "CloudEvent missing subject; discarding",
			"eventID", ce.ID(),
			"type", ce.Type(),
		)
		return nil, permanentValidationError{err: fmt.Errorf("missing subject")}
	}

	return &ce, nil
}

// attribute rewrites ce's subject to the UID of the BillingAccount billing
// attributed it to, the account's OpenMeter customer key. Any failure is
// retryable: the account may simply not be in the cache yet, and dropping
// the event would lose billable usage. An event billing never attributed
// (no billingaccountref) keeps being retried until it exhausts its
// deliveries, which is logged at error level.
func (c *SubmissionConsumer) attribute(ctx context.Context, ce *cloudevents.Event) error {
	project, accountName, err := attributionTarget(ce.Subject(), ce.Extensions())
	if err != nil {
		return err
	}
	uid, err := c.Accounts.ResolveAccountUID(ctx, project, accountName)
	if err != nil {
		return err
	}
	ce.SetSubject(string(uid))
	return nil
}

// processMessages processes a batch of JetStream messages: validates them,
// submits valid ones in a bulk request to OpenMeter, and settles each
// message according to the outcome (see the package doc). When OpenMeter
// rejects the batch as invalid (or too large), the events are resubmitted
// one at a time so only the offending event is discarded.
func (c *SubmissionConsumer) processMessages(ctx context.Context, msgs []jetstream.Msg) {
	var validatedEvents []cloudevents.Event
	var validatedMsgs []jetstream.Msg

	for _, msg := range msgs {
		ce, err := c.validateEvent(msg)
		if err != nil {
			// validateEvent only returns permanentValidationError.
			recordSubmission(outcomePermanent)
			c.ack(msg)
			continue
		}
		if err := c.attribute(ctx, ce); err != nil {
			c.Logger.Error(err, "usage event not attributed to a billing account yet; nacking for retry",
				"eventID", ce.ID(), "type", ce.Type(), "subject", ce.Subject())
			c.nak(msg, outcomeUnattributed)
			continue
		}
		validatedEvents = append(validatedEvents, *ce)
		validatedMsgs = append(validatedMsgs, msg)
	}

	if len(validatedEvents) == 0 {
		return
	}

	submitErr := c.IngestClient.SubmitUsageBatch(ctx, validatedEvents)
	switch {
	case submitErr == nil:
		c.Logger.Info("successfully submitted batch of usage events to OpenMeter", "count", len(validatedEvents))
		for _, msg := range validatedMsgs {
			recordSubmission(outcomeSuccess)
			c.ack(msg)
		}

	case openmeter.IsTransient(submitErr):
		c.Logger.Info("transient OpenMeter ingest error on batch; nacking all for retry",
			"err", submitErr.Error(), "count", len(validatedMsgs))
		c.nakAll(validatedMsgs, outcomeTransient)

	case openmeter.IsEventRejected(submitErr) || errors.Is(submitErr, openmeter.ErrPayloadTooLarge):
		c.Logger.Info("OpenMeter rejected the batch; resubmitting events one at a time to isolate the offending event",
			"err", submitErr.Error(), "count", len(validatedEvents))
		c.submitIndividually(ctx, validatedEvents, validatedMsgs)

	default:
		// Not the events' fault (auth, wrong URL, ...): resubmitting them
		// one by one cannot help, and dropping them would lose usage.
		c.Logger.Error(submitErr, "OpenMeter refused the ingest request for a non-event reason (check the OpenMeter serverUrl and apiSecret); nacking all for retry",
			"count", len(validatedMsgs))
		c.nakAll(validatedMsgs, outcomeConfigError)
	}
}

// submitIndividually submits each event on its own so a single invalid
// event can be discarded without dropping the rest of its batch. Before
// each call the ack deadline of every message still pending is extended
// (msg.InProgress): the loop can take several request timeouts, longer than
// AckWait, and an expired deadline would redeliver a message — possibly to
// another replica — while it is still being processed here.
func (c *SubmissionConsumer) submitIndividually(ctx context.Context, events []cloudevents.Event, msgs []jetstream.Msg) {
	for i, ce := range events {
		c.extendDeadlines(msgs[i:])
		msg := msgs[i]

		err := c.IngestClient.SubmitUsageBatch(ctx, []cloudevents.Event{ce})
		switch {
		case err == nil:
			c.Logger.V(1).Info("submitted usage event to OpenMeter on fallback",
				"eventID", ce.ID(), "type", ce.Type())
			recordSubmission(outcomeSuccess)
			c.ack(msg)

		case openmeter.IsEventRejected(err):
			c.Logger.Error(err, "OpenMeter rejected usage event as invalid; discarding it",
				"eventID", ce.ID(),
				"subject", ce.Subject(),
				"type", ce.Type(),
			)
			recordSubmission(outcomePermanent)
			c.ack(msg)

		case openmeter.IsTransient(err):
			c.Logger.Info("transient OpenMeter ingest error on fallback; nacking for retry",
				"eventID", ce.ID(), "err", err.Error())
			c.nak(msg, outcomeTransient)

		default:
			// A configuration-class failure applies to every remaining
			// event too; stop calling OpenMeter and retry them all later.
			c.Logger.Error(err, "OpenMeter refused the ingest request for a non-event reason (check the OpenMeter serverUrl and apiSecret); nacking remaining events for retry",
				"eventID", ce.ID(), "remaining", len(msgs)-i)
			c.nakAll(msgs[i:], outcomeConfigError)
			return
		}
	}
}

// extendDeadlines resets the ack timer of every message in msgs.
func (c *SubmissionConsumer) extendDeadlines(msgs []jetstream.Msg) {
	for _, msg := range msgs {
		if err := msg.InProgress(); err != nil {
			c.Logger.Error(err, "failed to extend ack deadline", "subject", msg.Subject())
		}
	}
}

func (c *SubmissionConsumer) ack(msg jetstream.Msg) {
	if err := msg.Ack(); err != nil {
		c.Logger.Error(err, "failed to ack message", "subject", msg.Subject())
	}
}

func (c *SubmissionConsumer) nakAll(msgs []jetstream.Msg, outcome string) {
	for _, msg := range msgs {
		c.nak(msg, outcome)
	}
}

// nak records outcome and NAKs msg with a delay that grows with its
// delivery count (see nakDelay). On the final allowed delivery it logs that
// JetStream will not redeliver the message.
func (c *SubmissionConsumer) nak(msg jetstream.Msg, outcome string) {
	recordSubmission(outcome)
	var delivered uint64
	if meta, err := msg.Metadata(); err == nil {
		delivered = meta.NumDelivered
	}
	if delivered >= maxDeliver {
		c.Logger.Error(nil, "final JetStream delivery of usage event failed; it will NOT be redelivered or submitted",
			"subject", msg.Subject(), "deliveries", delivered, "outcome", outcome)
	}
	if err := msg.NakWithDelay(c.nakDelay(delivered)); err != nil {
		c.Logger.Error(err, "failed to nack message with delay", "subject", msg.Subject())
	}
}

// nakDelay is the redelivery delay after the delivered-th failed delivery:
// RetryAfter doubled per previous delivery, capped at maxNakDelay.
func (c *SubmissionConsumer) nakDelay(delivered uint64) time.Duration {
	d := c.RetryAfter
	for i := uint64(1); i < delivered && d < maxNakDelay; i++ {
		d *= 2
	}
	return min(d, maxNakDelay)
}
