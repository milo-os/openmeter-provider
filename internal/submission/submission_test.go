// SPDX-License-Identifier: AGPL-3.0-only

package submission

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"k8s.io/apimachinery/pkg/types"
	"github.com/go-logr/logr"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	dto "github.com/prometheus/client_model/go"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

// fakeMsg implements jetstream.Msg for testing processMessages without a
// real NATS server.
type fakeMsg struct {
	data         []byte
	subject      string
	numDelivered uint64
	acked        bool
	naked        bool
	nakDelay     time.Duration
	inProgress   int
}

func (m *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) {
	delivered := m.numDelivered
	if delivered == 0 {
		delivered = 1
	}
	return &jetstream.MsgMetadata{Timestamp: time.Now(), NumDelivered: delivered}, nil
}
func (m *fakeMsg) Data() []byte                       { return m.data }
func (m *fakeMsg) Headers() natsgo.Header             { return nil }
func (m *fakeMsg) Subject() string                    { return m.subject }
func (m *fakeMsg) Reply() string                      { return "" }
func (m *fakeMsg) Ack() error                         { m.acked = true; return nil }
func (m *fakeMsg) DoubleAck(_ context.Context) error  { m.acked = true; return nil }
func (m *fakeMsg) Nak() error                         { m.naked = true; return nil }
func (m *fakeMsg) NakWithDelay(d time.Duration) error { m.naked = true; m.nakDelay = d; return nil }
func (m *fakeMsg) InProgress() error                  { m.inProgress++; return nil }
func (m *fakeMsg) Term() error                        { return nil }
func (m *fakeMsg) TermWithReason(_ string) error      { return nil }

// fakeIngestClient implements openmeter.Client for testing, but only
// SubmitUsageBatch is exercised by the submission consumer; every other
// method panics if called so a test fails loudly if the consumer starts
// depending on something outside its documented scope.
type fakeIngestClient struct {
	openmeter.Client
	submitFunc func(ctx context.Context, events []cloudevents.Event) error
	received   []cloudevents.Event
	calls      int
}

func (f *fakeIngestClient) SubmitUsageBatch(ctx context.Context, events []cloudevents.Event) error {
	f.calls++
	if f.submitFunc != nil {
		if err := f.submitFunc(ctx, events); err != nil {
			return err
		}
	}
	f.received = append(f.received, events...)
	return nil
}

// Errors shaped exactly like openmeter.SubmitUsageBatch returns them.
func rejectedErr() error {
	return &openmeter.PermanentError{
		Err:        fmt.Errorf("%w: openmeter rejected request: invalid event", openmeter.ErrEventRejected),
		StatusCode: http.StatusBadRequest,
	}
}

func configErr(status int) error {
	return &openmeter.PermanentError{Err: errors.New("openmeter rejected request"), StatusCode: status}
}

func transientErr() error {
	return &openmeter.TransientError{Err: errors.New("503 service unavailable"), StatusCode: http.StatusServiceUnavailable}
}

// buildCloudEventJSON builds a minimal valid CloudEvent JSON payload.
func buildCloudEventJSON(t *testing.T, id, ceType, subject string, value int64) []byte {
	t.Helper()
	ce := cloudevents.NewEvent()
	ce.SetID(id)
	ce.SetType(ceType)
	ce.SetSource("test-source")
	ce.SetSubject(subject)
	// Every event on the .valid stream carries billing's attribution.
	ce.SetExtension(billingAccountRefExtension, testAccountName)
	ce.SetTime(time.Now().UTC())
	if err := ce.SetData(cloudevents.ApplicationJSON, map[string]any{"value": fmt.Sprintf("%d", value)}); err != nil {
		t.Fatalf("SetData: %v", err)
	}
	b, err := ce.MarshalJSON()
	if err != nil {
		t.Fatalf("buildCloudEventJSON: %v", err)
	}
	return b
}

func newMsg(t *testing.T, id string) *fakeMsg {
	t.Helper()
	return &fakeMsg{
		data:    buildCloudEventJSON(t, id, "compute.miloapis.com/cpu", "projects/proj-1", 100),
		subject: "billing.usage.proj-1.valid",
	}
}

func asMsgs(msgs ...*fakeMsg) []jetstream.Msg {
	out := make([]jetstream.Msg, len(msgs))
	for i, m := range msgs {
		out[i] = m
	}
	return out
}

const (
	testAccountName = "acme"
	testAccountUID  = "uid-acme"
)

// fakeAccountResolver resolves (project, account name) pairs from a map.
type fakeAccountResolver struct {
	uids map[string]types.UID // "project/accountName" -> UID
}

func (f *fakeAccountResolver) ResolveAccountUID(_ context.Context, project, accountName string) (types.UID, error) {
	if uid, ok := f.uids[project+"/"+accountName]; ok {
		return uid, nil
	}
	return "", fmt.Errorf("%w: %s/%s", ErrAccountUnresolved, project, accountName)
}

// newTestConsumer returns a SubmissionConsumer for processMessages tests.
// Events of project proj-1 attributed to account acme resolve to uid-acme.
func newTestConsumer(t *testing.T, ingestClient openmeter.Client) *SubmissionConsumer {
	t.Helper()
	return &SubmissionConsumer{
		IngestClient: ingestClient,
		Accounts:     &fakeAccountResolver{uids: map[string]types.UID{"proj-1/" + testAccountName: testAccountUID}},
		Logger:       logr.Discard(),
		FetchBatch:   1,
		RetryAfter:   5 * time.Second,
		AckWait:      2 * time.Minute,
		FetchTimeout: 5 * time.Second,
	}
}

// counterValue reads the current value of one status label of
// billing_submission_requests_total.
func counterValue(t *testing.T, status string) float64 {
	t.Helper()
	var m dto.Metric
	if err := submissionRequestsTotal.WithLabelValues(status).Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

func TestProcessMessages_HappyPath(t *testing.T) {
	meterName := "compute.miloapis.com/cpu"

	ingest := &fakeIngestClient{}
	consumer := newTestConsumer(t, ingest)

	msg := &fakeMsg{
		data:    buildCloudEventJSON(t, "evt-001", meterName, "projects/proj-1", 100),
		subject: "billing.usage.proj-1.valid",
	}

	consumer.processMessages(context.Background(), asMsgs(msg))

	if !msg.acked {
		t.Error("expected message to be acked")
	}
	if msg.naked {
		t.Error("expected message NOT to be nacked")
	}
	if len(ingest.received) != 1 {
		t.Fatalf("expected 1 event submitted, got %d", len(ingest.received))
	}
	got := ingest.received[0]
	if got.ID() != "evt-001" {
		t.Errorf("ID: got %q want %q", got.ID(), "evt-001")
	}
	if got.Type() != meterName {
		t.Errorf("Type: got %q want %q", got.Type(), meterName)
	}
	// The subject is the account UID, the OpenMeter customer key.
	if got.Subject() != testAccountUID {
		t.Errorf("Subject: got %q want %q", got.Subject(), testAccountUID)
	}
}

// TestProcessMessages_UnattributedEventsAreRetried: an event whose account
// can't be resolved (not cached yet, or billing never attributed it) is
// NAKed — never submitted under the wrong subject, never dropped — while the
// rest of the batch goes through.
func TestProcessMessages_UnattributedEventsAreRetried(t *testing.T) {
	ingest := &fakeIngestClient{}
	consumer := newTestConsumer(t, ingest)

	good := newMsg(t, "evt-good")
	unknownProject := &fakeMsg{
		data:    buildCloudEventJSON(t, "evt-unknown", "compute.miloapis.com/cpu", "projects/proj-unknown", 1),
		subject: "billing.usage.proj-unknown.valid",
	}
	ce := cloudevents.NewEvent()
	ce.SetID("evt-noref")
	ce.SetType("compute.miloapis.com/cpu")
	ce.SetSource("test-source")
	ce.SetSubject("projects/proj-1")
	noRef, err := ce.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	missingRef := &fakeMsg{data: noRef, subject: "billing.usage.proj-1.valid"}
	badSubject := &fakeMsg{
		data:    buildCloudEventJSON(t, "evt-badsubject", "compute.miloapis.com/cpu", "proj-1", 1),
		subject: "billing.usage.proj-1.valid",
	}

	before := counterValue(t, outcomeUnattributed)
	consumer.processMessages(context.Background(), asMsgs(good, unknownProject, missingRef, badSubject))

	if !good.acked || len(ingest.received) != 1 || ingest.received[0].Subject() != testAccountUID {
		t.Fatalf("good event: acked=%v submitted=%v", good.acked, ingest.received)
	}
	for name, m := range map[string]*fakeMsg{"unknown project": unknownProject, "missing billingaccountref": missingRef, "subject without projects/": badSubject} {
		if m.acked || !m.naked {
			t.Errorf("%s: acked=%v naked=%v, want NAKed for retry", name, m.acked, m.naked)
		}
	}
	if got := counterValue(t, outcomeUnattributed) - before; got != 3 {
		t.Errorf("unattributed counter += %v, want 3", got)
	}
}

func TestProcessMessages_MalformedCloudEvent(t *testing.T) {
	ingest := &fakeIngestClient{}
	consumer := newTestConsumer(t, ingest)

	msg := &fakeMsg{
		data:    []byte("this is not json"),
		subject: "billing.usage.proj-1.valid",
	}

	before := counterValue(t, outcomePermanent)
	consumer.processMessages(context.Background(), asMsgs(msg))

	if !msg.acked {
		t.Error("expected malformed event to be acked (discarded)")
	}
	if ingest.calls != 0 {
		t.Error("expected no ingest call for malformed event")
	}
	if got := counterValue(t, outcomePermanent) - before; got != 1 {
		t.Errorf("permanent counter delta = %v, want 1", got)
	}
}

func TestProcessMessages_MissingSubject(t *testing.T) {
	ingest := &fakeIngestClient{}
	consumer := newTestConsumer(t, ingest)

	msg := &fakeMsg{
		data:    buildCloudEventJSON(t, "evt-nosubj", "compute.miloapis.com/cpu", "", 10),
		subject: "billing.usage.proj-1.valid",
	}

	consumer.processMessages(context.Background(), asMsgs(msg))

	if !msg.acked {
		t.Error("expected message without subject to be acked (discarded)")
	}
	if ingest.calls != 0 {
		t.Error("expected no ingest call for event missing subject")
	}
}

// TestProcessMessages_ForwardsEventsForAnyMeter guards the decision not to
// gate submission on the MeterDefinition: billing already validated the
// event, OpenMeter stores events regardless of meter state, and late usage
// for a Retired (or not yet visible) meter must still be billed rather than
// NAKed until the stream's retention silently drops it.
func TestProcessMessages_ForwardsEventsForAnyMeter(t *testing.T) {
	ingest := &fakeIngestClient{}
	consumer := newTestConsumer(t, ingest)

	msg := &fakeMsg{
		data:    buildCloudEventJSON(t, "evt-002", "retired.miloapis.com/old-meter", "projects/proj-1", 5),
		subject: "billing.usage.proj-1.valid",
	}

	consumer.processMessages(context.Background(), asMsgs(msg))

	if !msg.acked || msg.naked {
		t.Errorf("acked=%v naked=%v, want the event submitted and acked", msg.acked, msg.naked)
	}
	if len(ingest.received) != 1 {
		t.Fatalf("expected the event to be submitted, got %d submissions", len(ingest.received))
	}
}

func TestProcessMessages_TransientIngestError(t *testing.T) {
	ingest := &fakeIngestClient{
		submitFunc: func(_ context.Context, _ []cloudevents.Event) error { return transientErr() },
	}
	consumer := newTestConsumer(t, ingest)
	msg := newMsg(t, "evt-003")

	consumer.processMessages(context.Background(), asMsgs(msg))

	if !msg.naked || msg.acked {
		t.Errorf("acked=%v naked=%v, want nacked only", msg.acked, msg.naked)
	}
	if msg.nakDelay != consumer.RetryAfter {
		t.Errorf("nakDelay on first delivery = %v, want RetryAfter %v", msg.nakDelay, consumer.RetryAfter)
	}
}

// TestProcessMessages_SingleRejectedEventDiscarded: OpenMeter rejecting an
// event as invalid (400/422) is the only ingest failure that may discard it.
func TestProcessMessages_SingleRejectedEventDiscarded(t *testing.T) {
	ingest := &fakeIngestClient{
		submitFunc: func(_ context.Context, _ []cloudevents.Event) error { return rejectedErr() },
	}
	consumer := newTestConsumer(t, ingest)
	msg := newMsg(t, "evt-004")

	consumer.processMessages(context.Background(), asMsgs(msg))

	if !msg.acked || msg.naked {
		t.Errorf("acked=%v naked=%v, want the invalid event acked (discarded)", msg.acked, msg.naked)
	}
}

// TestProcessMessages_ConfigErrorsNeverDiscard is the regression test for
// usage being silently dropped on a bad secret or URL: auth and other
// non-validation 4xx failures must NAK every event (and count them under a
// distinct label), never ack them — neither at batch level nor after the
// per-event fallback.
func TestProcessMessages_ConfigErrorsNeverDiscard(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusMethodNotAllowed,
		http.StatusUnsupportedMediaType,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			ingest := &fakeIngestClient{
				submitFunc: func(_ context.Context, _ []cloudevents.Event) error { return configErr(status) },
			}
			consumer := newTestConsumer(t, ingest)
			msg1, msg2 := newMsg(t, "evt-a"), newMsg(t, "evt-b")

			before := counterValue(t, outcomeConfigError)
			consumer.processMessages(context.Background(), asMsgs(msg1, msg2))

			for _, m := range []*fakeMsg{msg1, msg2} {
				if m.acked || !m.naked {
					t.Errorf("acked=%v naked=%v, want nacked and never acked", m.acked, m.naked)
				}
			}
			if ingest.calls != 1 {
				t.Errorf("SubmitUsageBatch calls = %d, want 1 (no per-event fallback for a config error)", ingest.calls)
			}
			if got := counterValue(t, outcomeConfigError) - before; got != 2 {
				t.Errorf("config_error counter delta = %v, want 2", got)
			}
		})
	}
}

func TestProcessMessages_HappyPath_Batch(t *testing.T) {
	calledBatch := false
	ingest := &fakeIngestClient{
		submitFunc: func(_ context.Context, events []cloudevents.Event) error {
			if len(events) == 2 {
				calledBatch = true
			}
			return nil
		},
	}
	consumer := newTestConsumer(t, ingest)
	msg1, msg2 := newMsg(t, "evt-001"), newMsg(t, "evt-002")

	consumer.processMessages(context.Background(), asMsgs(msg1, msg2))

	if !calledBatch {
		t.Error("expected SubmitUsageBatch to be called as a batch of 2")
	}
	if !msg1.acked || !msg2.acked {
		t.Error("expected both messages to be acked")
	}
	if len(ingest.received) != 2 {
		t.Fatalf("expected 2 events received, got %d", len(ingest.received))
	}
}

func TestProcessMessages_MalformedAndValidMixed(t *testing.T) {
	ingest := &fakeIngestClient{}
	consumer := newTestConsumer(t, ingest)

	msgValid := newMsg(t, "evt-valid")
	msgMalformed := &fakeMsg{data: []byte("not json"), subject: "billing.usage.proj-1.valid"}

	consumer.processMessages(context.Background(), asMsgs(msgValid, msgMalformed))

	if !msgValid.acked {
		t.Error("expected valid message to be acked")
	}
	if !msgMalformed.acked {
		t.Error("expected malformed message to be acked (discarded)")
	}
	if len(ingest.received) != 1 || ingest.received[0].ID() != "evt-valid" {
		t.Fatalf("submitted = %v, want only evt-valid", ingest.received)
	}
}

func TestProcessMessages_BatchRejectedFallback(t *testing.T) {
	// OpenMeter rejects the batch of 2, but accepts the good event when it
	// is sent on its own.
	ingest := &fakeIngestClient{
		submitFunc: func(_ context.Context, events []cloudevents.Event) error {
			if len(events) == 2 || events[0].ID() == "evt-bad" {
				return rejectedErr()
			}
			return nil
		},
	}
	consumer := newTestConsumer(t, ingest)
	msgGood, msgBad := newMsg(t, "evt-good"), newMsg(t, "evt-bad")

	consumer.processMessages(context.Background(), asMsgs(msgGood, msgBad))

	// Total calls: 1 (batch) + 2 (fallback) = 3 calls.
	if ingest.calls != 3 {
		t.Errorf("expected 3 SubmitUsageBatch calls, got %d", ingest.calls)
	}
	if !msgGood.acked {
		t.Error("expected good message to be acked")
	}
	if !msgBad.acked {
		t.Error("expected bad message to be acked (discarded) after it was rejected on its own")
	}
	if len(ingest.received) != 1 || ingest.received[0].ID() != "evt-good" {
		t.Errorf("submitted = %v, want only evt-good", ingest.received)
	}
}

// TestProcessMessages_FallbackExtendsAckDeadlines: the per-event fallback
// can take longer than AckWait, so every still-pending message's deadline
// must be extended before each OpenMeter call.
func TestProcessMessages_FallbackExtendsAckDeadlines(t *testing.T) {
	ingest := &fakeIngestClient{
		submitFunc: func(_ context.Context, events []cloudevents.Event) error {
			if len(events) > 1 {
				return rejectedErr()
			}
			return nil
		},
	}
	consumer := newTestConsumer(t, ingest)
	m1, m2, m3 := newMsg(t, "e1"), newMsg(t, "e2"), newMsg(t, "e3")

	consumer.processMessages(context.Background(), asMsgs(m1, m2, m3))

	// m1 is pending before call 1; m2 before calls 1-2; m3 before 1-3.
	for i, want := range []int{1, 2, 3} {
		if got := []*fakeMsg{m1, m2, m3}[i].inProgress; got != want {
			t.Errorf("msg %d InProgress calls = %d, want %d", i+1, got, want)
		}
	}
	if !m1.acked || !m2.acked || !m3.acked {
		t.Error("expected all messages to be acked")
	}
}

// TestProcessMessages_FallbackConfigErrorStops: a configuration-class error
// during the per-event fallback applies to every remaining event; the loop
// must stop calling OpenMeter and NAK them all rather than discard any.
func TestProcessMessages_FallbackConfigErrorStops(t *testing.T) {
	ingest := &fakeIngestClient{
		submitFunc: func(_ context.Context, events []cloudevents.Event) error {
			switch {
			case len(events) > 1:
				return rejectedErr()
			case events[0].ID() == "e1":
				return nil
			default:
				return configErr(http.StatusUnauthorized)
			}
		},
	}
	consumer := newTestConsumer(t, ingest)
	m1, m2, m3 := newMsg(t, "e1"), newMsg(t, "e2"), newMsg(t, "e3")

	consumer.processMessages(context.Background(), asMsgs(m1, m2, m3))

	if ingest.calls != 3 {
		t.Errorf("SubmitUsageBatch calls = %d, want 3 (batch, e1, e2 — never e3)", ingest.calls)
	}
	if !m1.acked {
		t.Error("expected e1 to be acked")
	}
	for _, m := range []*fakeMsg{m2, m3} {
		if m.acked || !m.naked {
			t.Errorf("acked=%v naked=%v, want nacked and never acked", m.acked, m.naked)
		}
	}
}

func TestProcessMessages_FallbackTransientNaks(t *testing.T) {
	ingest := &fakeIngestClient{
		submitFunc: func(_ context.Context, events []cloudevents.Event) error {
			switch {
			case len(events) > 1:
				return rejectedErr()
			case events[0].ID() == "e2":
				return transientErr()
			default:
				return nil
			}
		},
	}
	consumer := newTestConsumer(t, ingest)
	m1, m2 := newMsg(t, "e1"), newMsg(t, "e2")

	consumer.processMessages(context.Background(), asMsgs(m1, m2))

	if !m1.acked {
		t.Error("expected e1 to be acked")
	}
	if m2.acked || !m2.naked {
		t.Errorf("e2 acked=%v naked=%v, want nacked", m2.acked, m2.naked)
	}
}

func TestProcessMessages_PayloadTooLargeFallsBack(t *testing.T) {
	ingest := &fakeIngestClient{
		submitFunc: func(_ context.Context, events []cloudevents.Event) error {
			if len(events) > 1 {
				return &openmeter.PermanentError{
					Err:        fmt.Errorf("%w: too large", openmeter.ErrPayloadTooLarge),
					StatusCode: http.StatusRequestEntityTooLarge,
				}
			}
			return nil
		},
	}
	consumer := newTestConsumer(t, ingest)
	m1, m2 := newMsg(t, "e1"), newMsg(t, "e2")

	consumer.processMessages(context.Background(), asMsgs(m1, m2))

	if !m1.acked || !m2.acked {
		t.Error("expected both events submitted individually and acked")
	}
}

func TestProcessMessages_BatchTransientError(t *testing.T) {
	ingest := &fakeIngestClient{
		submitFunc: func(_ context.Context, _ []cloudevents.Event) error { return transientErr() },
	}
	consumer := newTestConsumer(t, ingest)
	msg1, msg2 := newMsg(t, "evt-001"), newMsg(t, "evt-002")

	consumer.processMessages(context.Background(), asMsgs(msg1, msg2))

	if !msg1.naked || !msg2.naked {
		t.Error("expected both messages to be nacked on transient batch error")
	}
	if ingest.calls != 1 {
		t.Errorf("SubmitUsageBatch calls = %d, want 1", ingest.calls)
	}
}

// TestProcessMessages_NakDelayBacksOff: redeliveries back off exponentially
// with the delivery count, capped at maxNakDelay.
func TestProcessMessages_NakDelayBacksOff(t *testing.T) {
	ingest := &fakeIngestClient{
		submitFunc: func(_ context.Context, _ []cloudevents.Event) error { return transientErr() },
	}
	consumer := newTestConsumer(t, ingest)
	msg := newMsg(t, "evt")
	msg.numDelivered = 3

	consumer.processMessages(context.Background(), asMsgs(msg))

	if want := 4 * consumer.RetryAfter; msg.nakDelay != want {
		t.Errorf("nakDelay on 3rd delivery = %v, want %v", msg.nakDelay, want)
	}
}

func TestNakDelay(t *testing.T) {
	c := &SubmissionConsumer{RetryAfter: 5 * time.Second}
	tests := []struct {
		delivered uint64
		want      time.Duration
	}{
		{0, 5 * time.Second},
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{5, 80 * time.Second},
		{8, maxNakDelay},
		{maxDeliver, maxNakDelay},
	}
	for _, tt := range tests {
		if got := c.nakDelay(tt.delivered); got != tt.want {
			t.Errorf("nakDelay(%d) = %v, want %v", tt.delivered, got, tt.want)
		}
	}
}

// TestMaxDeliverSpansRetention guards the sizing argument for maxDeliver:
// with the NAK backoff, a message must keep being redelivered for longer
// than billing's 30-day replay window / stream retention, so MaxDeliver is
// only a safety net and never cuts an OpenMeter outage short.
func TestMaxDeliverSpansRetention(t *testing.T) {
	c := &SubmissionConsumer{RetryAfter: 5 * time.Second}
	var total time.Duration
	for n := uint64(1); n < maxDeliver; n++ {
		total += c.nakDelay(n)
	}
	if total < 30*24*time.Hour {
		t.Errorf("maxDeliver redeliveries span %v, want > 30 days", total)
	}
}

func TestFetchBackoff(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{1, time.Second},
		{2, 2 * time.Second},
		{4, 8 * time.Second},
		{10, maxFetchBackoff},
	}
	for _, tt := range tests {
		if got := fetchBackoff(tt.failures); got != tt.want {
			t.Errorf("fetchBackoff(%d) = %v, want %v", tt.failures, got, tt.want)
		}
	}
}

func TestFetchErrorClassification(t *testing.T) {
	for _, err := range []error{natsgo.ErrTimeout, jetstream.ErrNoMessages, context.DeadlineExceeded} {
		if !idleFetchError(err) {
			t.Errorf("idleFetchError(%v) = false, want true", err)
		}
		if fatalFetchError(err) {
			t.Errorf("fatalFetchError(%v) = true, want false", err)
		}
	}
	for _, err := range []error{natsgo.ErrConnectionClosed, jetstream.ErrConsumerDeleted, jetstream.ErrConsumerNotFound} {
		if !fatalFetchError(fmt.Errorf("wrapped: %w", err)) {
			t.Errorf("fatalFetchError(%v) = false, want true", err)
		}
	}
	if idleFetchError(jetstream.ErrNoHeartbeat) || fatalFetchError(jetstream.ErrNoHeartbeat) {
		t.Error("ErrNoHeartbeat should be neither idle nor fatal (logged and backed off)")
	}
}

// TestMinAckWait: AckWait must exceed one Fetch wait plus one OpenMeter
// request (the longest stretch without an InProgress extension).
func TestMinAckWait(t *testing.T) {
	c := &SubmissionConsumer{FetchTimeout: 5 * time.Second}
	if got, floor := c.minAckWait(), c.FetchTimeout+openmeter.RequestTimeout; got <= floor {
		t.Errorf("minAckWait = %v, want comfortably above %v", got, floor)
	}
}

func TestHandleMaxDeliveriesAdvisory(t *testing.T) {
	c := &SubmissionConsumer{Logger: logr.Discard()}
	before := counterValue(t, outcomeMaxDeliveries)

	c.handleMaxDeliveriesAdvisory([]byte(`{"type":"io.nats.jetstream.advisory.v1.max_deliver","stream":"billing-usage","consumer":"openmeter-usage-submitter","stream_seq":42,"deliveries":5000}`))
	c.handleMaxDeliveriesAdvisory([]byte(`not json`))

	if got := counterValue(t, outcomeMaxDeliveries) - before; got != 2 {
		t.Errorf("max_deliveries_exceeded counter delta = %v, want 2 (every advisory is counted, parseable or not)", got)
	}
}
