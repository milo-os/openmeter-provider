// SPDX-License-Identifier: AGPL-3.0-only

package submission

import (
	"context"
	"fmt"
	"testing"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/go-logr/logr"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

// fakeMsg implements jetstream.Msg for testing processMessages without a
// real NATS server.
type fakeMsg struct {
	data     []byte
	subject  string
	acked    bool
	naked    bool
	nakDelay time.Duration
}

func (m *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{Timestamp: time.Now()}, nil
}
func (m *fakeMsg) Data() []byte                       { return m.data }
func (m *fakeMsg) Headers() natsgo.Header             { return nil }
func (m *fakeMsg) Subject() string                    { return m.subject }
func (m *fakeMsg) Reply() string                      { return "" }
func (m *fakeMsg) Ack() error                         { m.acked = true; return nil }
func (m *fakeMsg) DoubleAck(_ context.Context) error  { m.acked = true; return nil }
func (m *fakeMsg) Nak() error                         { m.naked = true; return nil }
func (m *fakeMsg) NakWithDelay(d time.Duration) error { m.naked = true; m.nakDelay = d; return nil }
func (m *fakeMsg) InProgress() error                  { return nil }
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
	f.received = append(f.received, events...)
	if f.submitFunc != nil {
		return f.submitFunc(ctx, events)
	}
	return nil
}

// fakeCacheSync implements cacheSync for testing, always returning true.
type fakeCacheSync struct{}

func (f *fakeCacheSync) WaitForCacheSync(_ context.Context) bool { return true }

// buildCloudEventJSON builds a minimal valid CloudEvent JSON payload.
func buildCloudEventJSON(t *testing.T, id, ceType, subject string, value int64) []byte {
	t.Helper()
	ce := cloudevents.NewEvent()
	ce.SetID(id)
	ce.SetType(ceType)
	ce.SetSource("test-source")
	ce.SetSubject(subject)
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

// newTestConsumer returns a SubmissionConsumer backed by the provided cache.
func newTestConsumer(t *testing.T, ingestClient openmeter.Client, meterCache *MeterDefinitionCache) *SubmissionConsumer {
	t.Helper()
	return &SubmissionConsumer{
		Cache:        &fakeCacheSync{},
		IngestClient: ingestClient,
		MeterCache:   meterCache,
		Logger:       logr.Discard(),
		FetchBatch:   1,
		RetryAfter:   5 * time.Second,
		AckWait:      30 * time.Second,
		FetchTimeout: 5 * time.Second,
	}
}

// meterCacheWith returns a MeterDefinitionCache pre-populated with the given
// valid meter names.
func meterCacheWith(names ...string) *MeterDefinitionCache {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return &MeterDefinitionCache{names: set}
}

func TestProcessMessages_HappyPath(t *testing.T) {
	meterName := "compute.miloapis.com/cpu"

	ingest := &fakeIngestClient{}
	consumer := newTestConsumer(t, ingest, meterCacheWith(meterName))

	msg := &fakeMsg{
		data:    buildCloudEventJSON(t, "evt-001", meterName, "projects/proj-1", 100),
		subject: "billing.usage.proj-1.valid",
	}

	consumer.processMessages(context.Background(), []jetstream.Msg{msg})

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
	if got.Subject() != "projects/proj-1" {
		t.Errorf("Subject: got %q want %q", got.Subject(), "projects/proj-1")
	}
}

func TestProcessMessages_MalformedCloudEvent(t *testing.T) {
	ingest := &fakeIngestClient{}
	consumer := newTestConsumer(t, ingest, meterCacheWith())

	msg := &fakeMsg{
		data:    []byte("this is not json"),
		subject: "billing.usage.proj-1.valid",
	}

	consumer.processMessages(context.Background(), []jetstream.Msg{msg})

	if !msg.acked {
		t.Error("expected malformed event to be acked (discarded)")
	}
	if len(ingest.received) != 0 {
		t.Error("expected no ingest call for malformed event")
	}
}

func TestProcessMessages_MissingSubject(t *testing.T) {
	meterName := "compute.miloapis.com/cpu"
	ingest := &fakeIngestClient{}
	consumer := newTestConsumer(t, ingest, meterCacheWith(meterName))

	msg := &fakeMsg{
		data:    buildCloudEventJSON(t, "evt-nosubj", meterName, "", 10),
		subject: "billing.usage.proj-1.valid",
	}

	consumer.processMessages(context.Background(), []jetstream.Msg{msg})

	if !msg.acked {
		t.Error("expected message without subject to be acked (discarded)")
	}
	if len(ingest.received) != 0 {
		t.Error("expected no ingest call for event missing subject")
	}
}

func TestProcessMessages_MeterDefinitionCacheMiss(t *testing.T) {
	ingest := &fakeIngestClient{}
	consumer := newTestConsumer(t, ingest, meterCacheWith()) // empty — meter not indexed

	msg := &fakeMsg{
		data:    buildCloudEventJSON(t, "evt-002", "unknown.meter/type", "projects/proj-1", 5),
		subject: "billing.usage.proj-1.valid",
	}

	consumer.processMessages(context.Background(), []jetstream.Msg{msg})

	if !msg.naked {
		t.Error("expected message to be nacked on meter cache miss")
	}
	if msg.acked {
		t.Error("expected message NOT to be acked on cache miss")
	}
	if len(ingest.received) != 0 {
		t.Error("expected no ingest call on cache miss")
	}
}

func TestProcessMessages_TransientIngestError(t *testing.T) {
	meterName := "compute.miloapis.com/mem"

	ingest := &fakeIngestClient{
		submitFunc: func(_ context.Context, _ []cloudevents.Event) error {
			return &openmeter.TransientError{Err: fmt.Errorf("503 service unavailable"), StatusCode: 503}
		},
	}
	consumer := newTestConsumer(t, ingest, meterCacheWith(meterName))

	msg := &fakeMsg{
		data:    buildCloudEventJSON(t, "evt-003", meterName, "projects/proj-1", 50),
		subject: "billing.usage.proj-1.valid",
	}

	consumer.processMessages(context.Background(), []jetstream.Msg{msg})

	if !msg.naked {
		t.Error("expected message to be nacked on transient ingest error")
	}
	if msg.acked {
		t.Error("expected message NOT to be acked on transient error")
	}
}

func TestProcessMessages_PermanentIngestError(t *testing.T) {
	meterName := "compute.miloapis.com/storage"

	ingest := &fakeIngestClient{
		submitFunc: func(_ context.Context, _ []cloudevents.Event) error {
			return &openmeter.PermanentError{Err: fmt.Errorf("invalid record"), StatusCode: 400}
		},
	}
	consumer := newTestConsumer(t, ingest, meterCacheWith(meterName))

	msg := &fakeMsg{
		data:    buildCloudEventJSON(t, "evt-004", meterName, "projects/proj-1", 200),
		subject: "billing.usage.proj-1.valid",
	}

	consumer.processMessages(context.Background(), []jetstream.Msg{msg})

	if !msg.acked {
		t.Error("expected message to be acked on permanent ingest error (discard)")
	}
	if msg.naked {
		t.Error("expected message NOT to be nacked on permanent error")
	}
}

func TestProcessMessages_HappyPath_Batch(t *testing.T) {
	meterName := "compute.miloapis.com/cpu"

	calledBatch := false
	ingest := &fakeIngestClient{
		submitFunc: func(_ context.Context, events []cloudevents.Event) error {
			if len(events) == 2 {
				calledBatch = true
			}
			return nil
		},
	}
	consumer := newTestConsumer(t, ingest, meterCacheWith(meterName))

	msg1 := &fakeMsg{
		data:    buildCloudEventJSON(t, "evt-001", meterName, "projects/proj-1", 100),
		subject: "billing.usage.proj-1.valid",
	}
	msg2 := &fakeMsg{
		data:    buildCloudEventJSON(t, "evt-002", meterName, "projects/proj-1", 200),
		subject: "billing.usage.proj-1.valid",
	}

	consumer.processMessages(context.Background(), []jetstream.Msg{msg1, msg2})

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

func TestProcessMessages_TransientValidationAndSuccess(t *testing.T) {
	meterName := "compute.miloapis.com/cpu"

	ingest := &fakeIngestClient{}
	consumer := newTestConsumer(t, ingest, meterCacheWith(meterName))

	// Valid message
	msgValid := &fakeMsg{
		data:    buildCloudEventJSON(t, "evt-valid", meterName, "projects/proj-1", 100),
		subject: "billing.usage.proj-1.valid",
	}
	// Malformed (permanent validation error)
	msgMalformed := &fakeMsg{
		data:    []byte("not json"),
		subject: "billing.usage.proj-1.valid",
	}
	// Cache miss (transient validation error)
	msgCacheMiss := &fakeMsg{
		data:    buildCloudEventJSON(t, "evt-miss", "unknown-meter", "projects/proj-1", 100),
		subject: "billing.usage.proj-1.valid",
	}

	consumer.processMessages(context.Background(), []jetstream.Msg{msgValid, msgMalformed, msgCacheMiss})

	if !msgValid.acked {
		t.Error("expected valid message to be acked")
	}
	if !msgMalformed.acked {
		t.Error("expected malformed message to be acked (discarded)")
	}
	if !msgCacheMiss.naked {
		t.Error("expected cache miss message to be nacked (transient)")
	}

	if len(ingest.received) != 1 {
		t.Fatalf("expected 1 event to be submitted, got %d", len(ingest.received))
	}
	if ingest.received[0].ID() != "evt-valid" {
		t.Errorf("expected the valid event to be the one submitted, got id %q", ingest.received[0].ID())
	}
}

func TestProcessMessages_BatchPermanentErrorFallback(t *testing.T) {
	meterName := "compute.miloapis.com/cpu"

	// We want to simulate OpenMeter rejecting the batch of 2, but succeeding
	// when we send them individually except for the bad one.
	calls := 0
	ingest := &fakeIngestClient{
		submitFunc: func(_ context.Context, events []cloudevents.Event) error {
			calls++
			if len(events) == 2 {
				return &openmeter.PermanentError{Err: fmt.Errorf("bad batch"), StatusCode: 400}
			}
			if events[0].ID() == "evt-bad" {
				return &openmeter.PermanentError{Err: fmt.Errorf("bad event"), StatusCode: 400}
			}
			return nil
		},
	}
	consumer := newTestConsumer(t, ingest, meterCacheWith(meterName))

	msgGood := &fakeMsg{
		data:    buildCloudEventJSON(t, "evt-good", meterName, "projects/proj-1", 100),
		subject: "billing.usage.proj-1.valid",
	}
	msgBad := &fakeMsg{
		data:    buildCloudEventJSON(t, "evt-bad", meterName, "projects/proj-1", 999),
		subject: "billing.usage.proj-1.valid",
	}

	consumer.processMessages(context.Background(), []jetstream.Msg{msgGood, msgBad})

	// Total calls: 1 (batch) + 2 (fallback) = 3 calls.
	if calls != 3 {
		t.Errorf("expected 3 SubmitUsageBatch calls, got %d", calls)
	}

	if !msgGood.acked {
		t.Error("expected good message to be acked")
	}
	if !msgBad.acked {
		t.Error("expected bad message to be acked (discarded) after permanent individual error")
	}
}

func TestProcessMessages_BatchTransientError(t *testing.T) {
	meterName := "compute.miloapis.com/cpu"

	ingest := &fakeIngestClient{
		submitFunc: func(_ context.Context, _ []cloudevents.Event) error {
			return &openmeter.TransientError{Err: fmt.Errorf("503 service unavailable"), StatusCode: 503}
		},
	}
	consumer := newTestConsumer(t, ingest, meterCacheWith(meterName))

	msg1 := &fakeMsg{
		data:    buildCloudEventJSON(t, "evt-001", meterName, "projects/proj-1", 100),
		subject: "billing.usage.proj-1.valid",
	}
	msg2 := &fakeMsg{
		data:    buildCloudEventJSON(t, "evt-002", meterName, "projects/proj-1", 200),
		subject: "billing.usage.proj-1.valid",
	}

	consumer.processMessages(context.Background(), []jetstream.Msg{msg1, msg2})

	if !msg1.naked || !msg2.naked {
		t.Error("expected both messages to be nacked on transient batch error")
	}
}
