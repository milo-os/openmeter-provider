// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	om "github.com/openmeterio/openmeter/api/client/go"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
)

// TestMeterAggregation_UnmappedValueIsPermanent guards against silently
// mapping an unrecognized MeterAggregation onto SUM: an unmapped value can
// only reach this function if the mapping falls behind
// billingv1alpha1.MeterAggregation's CRD-validated enum, which is a code bug
// that must be surfaced loudly rather than miscategorizing usage.
func TestMeterAggregation_UnmappedValueIsPermanent(t *testing.T) {
	_, err := MeterAggregation(billingv1alpha1.MeterAggregation("Bogus"))
	if err == nil {
		t.Fatal("expected an error for an unmapped aggregation, got nil")
	}
	if !IsPermanent(err) {
		t.Errorf("expected a PermanentError, got %T: %v", err, err)
	}
}

// TestValueNeedsProperty guards the createMeter behavior against OpenMeter's
// own validateMeterAggregation rule: COUNT is the only aggregation that must
// NOT carry a valueProperty ("meter value property is not allowed when the
// aggregation is count"). Every other aggregation — UNIQUE_COUNT included —
// requires one ("meter value property is required when the aggregation is
// not count").
func TestValueNeedsProperty(t *testing.T) {
	tests := []struct {
		name        string
		aggregation om.MeterAggregation
		want        bool
	}{
		{"count", om.MeterAggregationCount, false},
		{"unique_count", om.MeterAggregationUniqueCount, true},
		{"sum", om.MeterAggregationSum, true},
		{"max", om.MeterAggregationMax, true},
		{"min", om.MeterAggregationMin, true},
		{"avg", om.MeterAggregationAvg, true},
		{"latest", om.MeterAggregationLatest, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := valueNeedsProperty(tt.aggregation); got != tt.want {
				t.Errorf("valueNeedsProperty(%q) = %v, want %v", tt.aggregation, got, tt.want)
			}
		})
	}
}

// validateMeterAggregation mirrors OpenMeter's own server-side rule
// (openmeter/meter.validateMeterAggregation): COUNT must not carry a
// valueProperty; every other aggregation must. The fake enforces it so a
// client regression (e.g. treating UNIQUE_COUNT like COUNT) fails these tests
// the same way it would fail against the real API, instead of the fake
// silently accepting whatever the client sends.
func validateMeterAggregation(aggregation om.MeterAggregation, valueProperty *string) error {
	if aggregation == om.MeterAggregationCount {
		if valueProperty != nil {
			return errors.New("meter value property is not allowed when the aggregation is count")
		}
		return nil
	}
	if valueProperty == nil || *valueProperty == "" {
		return errors.New("meter value property is required when the aggregation is not count")
	}
	return nil
}

// serveMeters handles /api/v1/meters[/{slug}] requests. Returns false when
// the request path isn't a meters route, so ServeHTTP can fall through to
// other resources.
func (f *fakeServer) serveMeters(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/v1/meters") {
		return false
	}
	slug := strings.TrimPrefix(r.URL.Path, "/api/v1/meters/")
	slug = strings.TrimSuffix(slug, "/")

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/meters":
		var body om.MeterCreate
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		if _, exists := f.meters[body.Slug]; exists {
			w.WriteHeader(http.StatusConflict)
			return true
		}
		if err := validateMeterAggregation(body.Aggregation, body.ValueProperty); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return true
		}
		m := om.Meter{
			Id:            "id-" + body.Slug,
			Slug:          body.Slug,
			EventType:     body.EventType,
			Aggregation:   body.Aggregation,
			Description:   body.Description,
			GroupBy:       body.GroupBy,
			ValueProperty: body.ValueProperty,
		}
		f.meters[body.Slug] = m
		if !f.emptyBodyOnWrite {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(http.StatusCreated)
		if !f.emptyBodyOnWrite {
			_ = json.NewEncoder(w).Encode(m)
		}

	case r.Method == http.MethodGet && slug != "":
		m, ok := f.meters[slug]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(m)

	case r.Method == http.MethodPut && slug != "":
		m, ok := f.meters[slug]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		var body om.MeterUpdate
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		m.Description = body.Description
		m.GroupBy = body.GroupBy
		f.meters[slug] = m
		if !f.emptyBodyOnWrite {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(http.StatusOK)
		if !f.emptyBodyOnWrite {
			_ = json.NewEncoder(w).Encode(m)
		}

	case r.Method == http.MethodDelete && slug != "":
		if _, ok := f.meters[slug]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		delete(f.meters, slug)
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
	return true
}

func baseDesiredMeter() DesiredMeter {
	return DesiredMeter{
		Slug:          "cpu_seconds",
		EventType:     "compute.miloapis.com/instance/cpu-seconds",
		Aggregation:   om.MeterAggregationSum,
		Description:   "CPU Seconds",
		GroupBy:       map[string]string{"region": "$.dimensions.region"},
		ValueProperty: "$.value",
	}
}

func TestEnsureMeter_CreatesWhenAbsent(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredMeter()

	got, err := c.EnsureMeter(context.Background(), desired)
	if err != nil {
		t.Fatalf("EnsureMeter: %v", err)
	}
	if got.Slug != desired.Slug {
		t.Errorf("Slug = %q, want %q", got.Slug, desired.Slug)
	}
	if got.ValueProperty == nil || *got.ValueProperty != desired.ValueProperty {
		t.Errorf("ValueProperty = %v, want %q", got.ValueProperty, desired.ValueProperty)
	}
	if _, ok := f.meters[desired.Slug]; !ok {
		t.Errorf("meter %q not stored", desired.Slug)
	}
}

// TestEnsureMeter_CreateFallsBackToGetOnEmptyBody guards against a false
// PermanentError observed against a real OpenMeter instance: CreateMeter
// returned 201 with an empty body (the create had still landed server-side,
// confirmed by a follow-up GetMeter finding it), and the client used to
// report that as a permanent failure instead of falling back to a GET.
func TestEnsureMeter_CreateFallsBackToGetOnEmptyBody(t *testing.T) {
	c, f := newTestClient(t)
	f.emptyBodyOnWrite = true
	desired := baseDesiredMeter()

	got, err := c.EnsureMeter(context.Background(), desired)
	if err != nil {
		t.Fatalf("EnsureMeter: %v", err)
	}
	if got.Slug != desired.Slug {
		t.Errorf("Slug = %q, want %q", got.Slug, desired.Slug)
	}
}

// TestEnsureMeter_UpdateFallsBackToGetOnEmptyBody is the UpdateMeter
// analogue of TestEnsureMeter_CreateFallsBackToGetOnEmptyBody.
func TestEnsureMeter_UpdateFallsBackToGetOnEmptyBody(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredMeter()
	if _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("first EnsureMeter: %v", err)
	}

	f.emptyBodyOnWrite = true
	desired.Description = "Updated description"
	got, err := c.EnsureMeter(context.Background(), desired)
	if err != nil {
		t.Fatalf("second EnsureMeter: %v", err)
	}
	if got.Slug != desired.Slug {
		t.Errorf("Slug = %q, want %q", got.Slug, desired.Slug)
	}
	if stored := f.meters[desired.Slug]; stored.Description == nil || *stored.Description != desired.Description {
		t.Errorf("Description not updated: %v", stored.Description)
	}
}

func TestEnsureMeter_CountAggregationOmitsValueProperty(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredMeter()
	desired.Aggregation = om.MeterAggregationCount

	if _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("EnsureMeter: %v", err)
	}
	stored := f.meters[desired.Slug]
	if stored.ValueProperty != nil {
		t.Errorf("ValueProperty = %v, want nil for Count aggregation", *stored.ValueProperty)
	}

	// Second reconcile must be a no-op update, not a spurious immutable-field
	// conflict from comparing "" (server) against the always-populated
	// DesiredMeter.ValueProperty.
	if _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("EnsureMeter (idempotent re-run): %v", err)
	}
}

// TestEnsureMeter_UniqueCountAttachesValueProperty guards against the
// UNIQUE_COUNT regression this client shipped with once: OpenMeter requires a
// valueProperty for UNIQUE_COUNT (only COUNT forbids one), so it must be
// attached on create and idempotently re-checked, not treated like COUNT.
func TestEnsureMeter_UniqueCountAttachesValueProperty(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredMeter()
	desired.Aggregation = om.MeterAggregationUniqueCount

	if _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("EnsureMeter: %v", err)
	}
	stored := f.meters[desired.Slug]
	if stored.ValueProperty == nil || *stored.ValueProperty != desired.ValueProperty {
		t.Errorf("ValueProperty = %v, want %q for UniqueCount aggregation", stored.ValueProperty, desired.ValueProperty)
	}

	if _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("EnsureMeter (idempotent re-run): %v", err)
	}
}

func TestEnsureMeter_NoopWhenUpToDate(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredMeter()

	if _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("first EnsureMeter: %v", err)
	}
	before := f.meters[desired.Slug].UpdatedAt

	if _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("second EnsureMeter: %v", err)
	}
	after := f.meters[desired.Slug].UpdatedAt
	if !before.Equal(after) {
		t.Errorf("UpdateMeter was called on a no-drift EnsureMeter")
	}
}

func TestEnsureMeter_UpdatesMutableFields(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredMeter()
	if _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("first EnsureMeter: %v", err)
	}

	desired.Description = "Updated description"
	desired.GroupBy = map[string]string{"region": "$.dimensions.region", "tier": "$.dimensions.tier"}
	got, err := c.EnsureMeter(context.Background(), desired)
	if err != nil {
		t.Fatalf("second EnsureMeter: %v", err)
	}
	if got.Description == nil || *got.Description != desired.Description {
		t.Errorf("Description = %v, want %q", got.Description, desired.Description)
	}
	if stored := f.meters[desired.Slug]; stored.GroupBy == nil || len(*stored.GroupBy) != 2 {
		t.Errorf("GroupBy not updated: %v", stored.GroupBy)
	}
}

func TestEnsureMeter_ImmutableFieldConflictIsPermanent(t *testing.T) {
	c, _ := newTestClient(t)
	desired := baseDesiredMeter()
	if _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("first EnsureMeter: %v", err)
	}

	drifted := desired
	drifted.Aggregation = om.MeterAggregationMax
	_, err := c.EnsureMeter(context.Background(), drifted)
	if err == nil {
		t.Fatal("expected an error for an aggregation change, got nil")
	}
	if !IsPermanent(err) {
		t.Errorf("expected a PermanentError, got %T: %v", err, err)
	}
}

func TestGetMeter_NotFound(t *testing.T) {
	c, _ := newTestClient(t)
	_, err := c.GetMeter(context.Background(), "missing")
	if !errors.Is(err, ErrMeterNotFound) {
		t.Errorf("GetMeter error = %v, want ErrMeterNotFound", err)
	}
}

func TestDeleteMeter_NotFoundIsSuccess(t *testing.T) {
	c, _ := newTestClient(t)
	if err := c.DeleteMeter(context.Background(), "missing"); err != nil {
		t.Errorf("DeleteMeter on missing slug: %v", err)
	}
}

func TestDeleteMeter_RemovesExisting(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredMeter()
	if _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("EnsureMeter: %v", err)
	}

	if err := c.DeleteMeter(context.Background(), desired.Slug); err != nil {
		t.Fatalf("DeleteMeter: %v", err)
	}
	if _, ok := f.meters[desired.Slug]; ok {
		t.Errorf("meter %q still present after delete", desired.Slug)
	}
}

func TestClassify_RateLimitIsTransient(t *testing.T) {
	c, f := newTestClient(t)
	f.statusOverride = http.StatusTooManyRequests

	_, err := c.GetMeter(context.Background(), "any")
	if !IsTransient(err) {
		t.Errorf("expected a TransientError for 429, got %T: %v", err, err)
	}
}

func TestClassify_ServerErrorIsTransient(t *testing.T) {
	c, f := newTestClient(t)
	f.statusOverride = http.StatusInternalServerError

	_, err := c.GetMeter(context.Background(), "any")
	if !IsTransient(err) {
		t.Errorf("expected a TransientError for 500, got %T: %v", err, err)
	}
}

func TestClassify_UnauthorizedIsPermanent(t *testing.T) {
	c, f := newTestClient(t)
	f.statusOverride = http.StatusUnauthorized

	_, err := c.GetMeter(context.Background(), "any")
	if !IsPermanent(err) {
		t.Errorf("expected a PermanentError for 401, got %T: %v", err, err)
	}
}
