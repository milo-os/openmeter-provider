// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
)

// serveIngest handles POST /api/v1/events (the CloudEvents batch ingest
// route). Returns false when the request isn't that route, so ServeHTTP
// can fall through to other resources.
func (f *fakeServer) serveIngest(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost || r.URL.Path != "/api/v1/events" {
		return false
	}
	var events []cloudevents.Event
	if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return true
	}
	f.ingestedEvents = append(f.ingestedEvents, events...)
	w.WriteHeader(http.StatusNoContent)
	return true
}

func baseUsageEvent(id, meterType, subject string) cloudevents.Event {
	ce := cloudevents.NewEvent()
	ce.SetID(id)
	ce.SetType(meterType)
	ce.SetSource("test-source")
	ce.SetSubject(subject)
	ce.SetTime(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	_ = ce.SetData("application/json", map[string]string{"value": "1"})
	return ce
}

func TestSubmitUsageBatch_ForwardsEventsUnmodified(t *testing.T) {
	c, f := newTestClient(t)
	events := []cloudevents.Event{
		baseUsageEvent("01M23AAA", "compute.cpu.seconds", "projects/project-alpha"),
		baseUsageEvent("01M23BBB", "compute.cpu.seconds", "projects/project-beta"),
	}

	if err := c.SubmitUsageBatch(context.Background(), events); err != nil {
		t.Fatalf("SubmitUsageBatch: %v", err)
	}
	if len(f.ingestedEvents) != 2 {
		t.Fatalf("ingested %d events, want 2", len(f.ingestedEvents))
	}
	if f.ingestedEvents[0].ID() != "01M23AAA" || f.ingestedEvents[0].Subject() != "projects/project-alpha" {
		t.Errorf("first event = %+v, want id/subject preserved unmodified", f.ingestedEvents[0])
	}
}

func TestSubmitUsageBatch_EmptyIsNoop(t *testing.T) {
	c, f := newTestClient(t)
	if err := c.SubmitUsageBatch(context.Background(), nil); err != nil {
		t.Fatalf("SubmitUsageBatch(nil): %v", err)
	}
	if len(f.ingestedEvents) != 0 {
		t.Errorf("ingested %d events for an empty batch, want 0", len(f.ingestedEvents))
	}
}

func TestSubmitUsageBatch_ServerErrorIsClassified(t *testing.T) {
	c, f := newTestClient(t)
	f.statusOverride = http.StatusServiceUnavailable

	err := c.SubmitUsageBatch(context.Background(), []cloudevents.Event{baseUsageEvent("id", "type", "subj")})
	if !IsTransient(err) {
		t.Errorf("expected a TransientError for 503, got %T: %v", err, err)
	}
}
