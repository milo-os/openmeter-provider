// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"errors"
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
	if f.ingestStatus != 0 {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(f.ingestStatus)
		_, _ = w.Write([]byte(f.ingestBody))
		return true
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

// TestSubmitUsageBatch_Classification pins which ingest failures count as
// "this event is invalid" (the only class a caller may discard) versus
// configuration problems that must never cause usage to be dropped.
func TestSubmitUsageBatch_Classification(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		wantRejected  bool
		wantTooLarge  bool
		wantTransient bool
	}{
		{"invalid event", http.StatusBadRequest, `{"title":"Bad Request","status":400,"detail":"invalid event: missing id"}`, true, false, false},
		{"schema violation", http.StatusBadRequest, `{"title":"Bad Request","status":400,"detail":"request body has an error"}`, true, false, false},
		{"unprocessable", http.StatusUnprocessableEntity, `{"status":422}`, true, false, false},
		{"invalid content type is a client bug", http.StatusBadRequest, `{"status":400,"detail":"invalid content type: text/plain"}`, false, false, false},
		{"unauthorized", http.StatusUnauthorized, `{"status":401}`, false, false, false},
		{"forbidden", http.StatusForbidden, `{"status":403}`, false, false, false},
		{"not found (wrong URL)", http.StatusNotFound, `{"status":404}`, false, false, false},
		{"method not allowed", http.StatusMethodNotAllowed, `{"status":405}`, false, false, false},
		{"unsupported media type", http.StatusUnsupportedMediaType, `{"status":415}`, false, false, false},
		{"too large", http.StatusRequestEntityTooLarge, `{"status":413}`, false, true, false},
		{"rate limited", http.StatusTooManyRequests, `{"status":429}`, false, false, true},
		{"server error", http.StatusBadGateway, `{"status":502}`, false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, f := newTestClient(t)
			f.ingestStatus = tt.status
			f.ingestBody = tt.body

			err := c.SubmitUsageBatch(context.Background(), []cloudevents.Event{baseUsageEvent("id", "type", "subj")})
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := IsEventRejected(err); got != tt.wantRejected {
				t.Errorf("IsEventRejected = %v, want %v (err: %v)", got, tt.wantRejected, err)
			}
			if got := errors.Is(err, ErrPayloadTooLarge); got != tt.wantTooLarge {
				t.Errorf("errors.Is(ErrPayloadTooLarge) = %v, want %v", got, tt.wantTooLarge)
			}
			if got := IsTransient(err); got != tt.wantTransient {
				t.Errorf("IsTransient = %v, want %v", got, tt.wantTransient)
			}
			if !tt.wantTransient && !IsPermanent(err) {
				t.Errorf("expected a PermanentError, got %T", err)
			}
		})
	}
}
