// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
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
		name := body.Slug // the real server defaults an omitted name to the slug
		if body.Name != nil {
			name = *body.Name
		}
		m := om.Meter{
			Id:            "id-" + body.Slug,
			Slug:          body.Slug,
			Name:          &name,
			EventType:     body.EventType,
			Aggregation:   body.Aggregation,
			Description:   body.Description,
			GroupBy:       body.GroupBy,
			ValueProperty: body.ValueProperty,
		}
		f.meters[body.Slug] = m
		f.meterWrites++
		if !f.emptyBodyOnWrite {
			w.Header().Set("Content-Type", "application/json")
		}
		// The real server answers a create with 200, not the 201 the
		// OpenAPI spec (and so the generated SDK) declares.
		w.WriteHeader(http.StatusOK)
		if !f.emptyBodyOnWrite {
			_ = json.NewEncoder(w).Encode(m)
		}

	case r.Method == http.MethodGet && slug != "":
		m, ok := f.meters[slug]
		if !ok {
			writeMeterNotFound(w, slug)
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(m)

	case r.Method == http.MethodPut && slug != "":
		m, ok := f.meters[slug]
		if !ok {
			writeMeterNotFound(w, slug)
			return true
		}
		var body om.MeterUpdate
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		// Mirrors the real update: an omitted name resets to the slug.
		name := m.Slug
		if body.Name != nil {
			name = *body.Name
		}
		m.Name = &name
		m.Description = body.Description
		m.GroupBy = body.GroupBy
		f.meters[slug] = m
		f.meterWrites++
		if !f.emptyBodyOnWrite {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(http.StatusOK)
		if !f.emptyBodyOnWrite {
			_ = json.NewEncoder(w).Encode(m)
		}

	case r.Method == http.MethodDelete && slug != "":
		if _, ok := f.meters[slug]; !ok {
			writeMeterNotFound(w, slug)
			return true
		}
		delete(f.meters, slug)
		w.WriteHeader(http.StatusNoContent)

	default:
		// The real router's 404 for an unknown route: problem+json, but
		// without a detail.
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Not Found","status":404}`))
	}
	return true
}

// writeMeterNotFound writes OpenMeter's real answer for a missing meter: an
// RFC 7807 problem whose detail carries the domain error
// (openmeter/meter.NewMeterNotFoundError).
func writeMeterNotFound(w http.ResponseWriter, slug string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "about:blank",
		"title":  "Not Found",
		"status": http.StatusNotFound,
		"detail": "meter not found: " + slug,
	})
}

func baseDesiredMeter() DesiredMeter {
	return DesiredMeter{
		Slug:          "cpu_seconds",
		EventType:     "compute.miloapis.com/instance/cpu-seconds",
		Aggregation:   om.MeterAggregationSum,
		Name:          "CPU Seconds",
		Description:   "CPU Seconds",
		GroupBy:       map[string]string{"region": "$.dimensions.region"},
		ValueProperty: "$.value",
	}
}

func TestEnsureMeter_CreatesWhenAbsent(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredMeter()

	got, _, err := c.EnsureMeter(context.Background(), desired)
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

// TestEnsureMeter_CreateFallsBackToGetOnEmptyBody: a 2xx create without a
// meter body still landed server-side, so the client must fetch the meter
// rather than report a failed create. (The original observation behind
// this — "an empty body after a fresh install" — was really the real
// server's 200 response, which the SDK does not parse because the spec
// declares 201; TestEnsureMeter_CreateReads200Body covers that.)
func TestEnsureMeter_CreateFallsBackToGetOnEmptyBody(t *testing.T) {
	c, f := newTestClient(t)
	f.emptyBodyOnWrite = true
	desired := baseDesiredMeter()

	got, _, err := c.EnsureMeter(context.Background(), desired)
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
	if _, _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("first EnsureMeter: %v", err)
	}

	f.emptyBodyOnWrite = true
	desired.Description = "Updated description"
	got, _, err := c.EnsureMeter(context.Background(), desired)
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

	if _, _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("EnsureMeter: %v", err)
	}
	stored := f.meters[desired.Slug]
	if stored.ValueProperty != nil {
		t.Errorf("ValueProperty = %v, want nil for Count aggregation", *stored.ValueProperty)
	}

	// Second reconcile must be a no-op update, not a spurious immutable-field
	// conflict from comparing "" (server) against the always-populated
	// DesiredMeter.ValueProperty.
	if _, _, err := c.EnsureMeter(context.Background(), desired); err != nil {
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

	if _, _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("EnsureMeter: %v", err)
	}
	stored := f.meters[desired.Slug]
	if stored.ValueProperty == nil || *stored.ValueProperty != desired.ValueProperty {
		t.Errorf("ValueProperty = %v, want %q for UniqueCount aggregation", stored.ValueProperty, desired.ValueProperty)
	}

	if _, _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("EnsureMeter (idempotent re-run): %v", err)
	}
}

func TestEnsureMeter_NoopWhenUpToDate(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredMeter()

	if _, _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("first EnsureMeter: %v", err)
	}
	before := f.meters[desired.Slug].UpdatedAt

	if _, _, err := c.EnsureMeter(context.Background(), desired); err != nil {
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
	if _, _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("first EnsureMeter: %v", err)
	}

	desired.Description = "Updated description"
	desired.GroupBy = map[string]string{"region": "$.dimensions.region", "tier": "$.dimensions.tier"}
	got, _, err := c.EnsureMeter(context.Background(), desired)
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
	if _, _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("first EnsureMeter: %v", err)
	}

	drifted := desired
	drifted.Aggregation = om.MeterAggregationMax
	_, _, err := c.EnsureMeter(context.Background(), drifted)
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
	if _, _, err := c.EnsureMeter(context.Background(), desired); err != nil {
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

// TestEnsureMeter_CreateReads200Body: the real server answers a create with
// 200 and the meter body; the client must use that body directly instead of
// issuing a follow-up GET (whose transient failure used to turn a
// successful create into a permanent error).
func TestEnsureMeter_CreateReads200Body(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredMeter()

	got, changed, err := c.EnsureMeter(context.Background(), desired)
	if err != nil {
		t.Fatalf("EnsureMeter: %v", err)
	}
	if !changed {
		t.Error("changed = false for a create, want true")
	}
	if got.Id != "id-"+desired.Slug {
		t.Errorf("Id = %q, want the id from the create response body", got.Id)
	}
	if got.Name == nil || *got.Name != desired.Name {
		t.Errorf("Name = %v, want %q", got.Name, desired.Name)
	}
	if f.meterWrites != 1 {
		t.Errorf("meter writes = %d, want 1", f.meterWrites)
	}
}

// TestEnsureMeter_CreateFollowUpGetFailureIsTransient: when a create was
// accepted but its body is unusable, a failing follow-up GET must not be
// reported as a permanent failure — the meter exists, and the next
// reconcile will find it.
func TestEnsureMeter_CreateFollowUpGetFailureIsTransient(t *testing.T) {
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusOK) // accepted, no body
		case http.MethodGet:
			gets++
			if gets == 1 {
				// EnsureMeter's initial lookup: not there yet.
				writeMeterNotFound(w, "cpu_seconds")
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, _, err = c.EnsureMeter(context.Background(), baseDesiredMeter())
	if !IsTransient(err) {
		t.Errorf("expected a TransientError, got %T: %v", err, err)
	}
}

// TestEnsureMeter_ChangedOnlyOnWrites: changed drives the controller's
// Synced event, so it must be false for a no-op and true for an update.
func TestEnsureMeter_ChangedOnlyOnWrites(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredMeter()
	if _, _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("create: %v", err)
	}

	_, changed, err := c.EnsureMeter(context.Background(), desired)
	if err != nil {
		t.Fatalf("no-op EnsureMeter: %v", err)
	}
	if changed {
		t.Error("changed = true for a no-op")
	}

	desired.Name = "CPU Seconds (renamed)"
	_, changed, err = c.EnsureMeter(context.Background(), desired)
	if err != nil {
		t.Fatalf("rename EnsureMeter: %v", err)
	}
	if !changed {
		t.Error("changed = false for an update")
	}
	if f.meterWrites != 2 {
		t.Errorf("meter writes = %d, want 2 (create + rename)", f.meterWrites)
	}
}

// TestEnsureMeter_UpdateKeepsName guards against OpenMeter resetting the
// display name to the slug on every update that omits it.
func TestEnsureMeter_UpdateKeepsName(t *testing.T) {
	c, f := newTestClient(t)
	desired := baseDesiredMeter()
	if _, _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("create: %v", err)
	}
	desired.Description = "Updated description"
	if _, _, err := c.EnsureMeter(context.Background(), desired); err != nil {
		t.Fatalf("update: %v", err)
	}
	if stored := f.meters[desired.Slug]; stored.Name == nil || *stored.Name != desired.Name {
		t.Errorf("stored Name = %v, want %q kept across the update", stored.Name, desired.Name)
	}
}

// TestMeterNotFound_RequiresOpenMeterProblem: only OpenMeter's own
// not-found problem counts as "meter absent". A 404 from anything else (a
// wrong serverUrl, an ingress default backend, an unknown route) must not
// make DeleteMeter report success — the finalizer would release and leak
// the meter — nor make GetMeter claim the meter is absent.
func TestMeterNotFound_RequiresOpenMeterProblem(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{"plain 404", "text/plain", "404 page not found"},
		{"html 404", "text/html", "<html>default backend - 404</html>"},
		{"router-level problem without detail", "application/problem+json", `{"title":"Not Found","status":404}`},
		{"json but not a problem", "application/json", `{"detail":"meter not found: x"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)
			c, err := NewClient(srv.URL, "")
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}

			if err := c.DeleteMeter(context.Background(), "cpu_seconds"); !IsPermanent(err) {
				t.Errorf("DeleteMeter error = %v, want a PermanentError", err)
			}
			_, err = c.GetMeter(context.Background(), "cpu_seconds")
			if errors.Is(err, ErrMeterNotFound) || !IsPermanent(err) {
				t.Errorf("GetMeter error = %v, want a PermanentError that is not ErrMeterNotFound", err)
			}
		})
	}
}

func TestMeterSlug(t *testing.T) {
	long := "very.long.service.miloapis.com/some/deeply/nested/metric-name-with-many-words-and-more"
	tests := []struct {
		meterName string
		want      string
	}{
		// Names the original '.'/'/'/'-' → '_' mapping already handled
		// must keep their exact slug.
		{"compute.miloapis.com/instance/cpu-seconds", "compute_miloapis_com_instance_cpu_seconds"},
		{"e2e.miloapis.com/submission/requests", "e2e_miloapis_com_submission_requests"},
		{"Compute.MiloAPIs.com/CPU", "compute_miloapis_com_cpu"},
		// Names that used to produce invalid slugs.
		{"compute.miloapis.com//cpu--seconds", "compute_miloapis_com_cpu_seconds"},
		{"/leading/and/trailing/", "leading_and_trailing"},
		{"weird~chars!here", "weird_chars_here"},
		{"a_b", "a_b"},
		{"a__b", "a_b"},
	}
	for _, tt := range tests {
		if got := MeterSlug(tt.meterName); got != tt.want {
			t.Errorf("MeterSlug(%q) = %q, want %q", tt.meterName, got, tt.want)
		}
	}

	slugRe := regexp.MustCompile(`^[a-z0-9]+(?:_[a-z0-9]+)*$`)
	for _, name := range []string{long, long + "-2", "~~~", "Ünïcödé", strings.Repeat("a", 253), strings.Repeat("a.", 120)} {
		got := MeterSlug(name)
		if len(got) > meterSlugMaxLen || !slugRe.MatchString(got) {
			t.Errorf("MeterSlug(%q) = %q (len %d), want a valid slug of at most %d chars", name, got, len(got), meterSlugMaxLen)
		}
		if MeterSlug(name) != got {
			t.Errorf("MeterSlug(%q) is not deterministic", name)
		}
	}
	if MeterSlug(long) == MeterSlug(long+"-2") {
		t.Error("two long meterNames sharing a prefix map to the same truncated slug")
	}
}

func TestGroupByKey(t *testing.T) {
	tests := []struct {
		dimension string
		want      string
	}{
		// Already-valid keys are unchanged.
		{"region", "region"},
		{"project_name", "project_name"},
		{"Tier_2", "Tier_2"},
		{"_private", "_private"},
		// Sanitized.
		{"resource.type", "resource_type"},
		{"k8s.io/name", "k8s_io_name"},
		{"a-b", "a_b"},
		{"9lives", "dim_9lives"},
		{"1.5", "dim_1_5"},
		// Reserved query group-bys.
		{"subject", "dim_subject"},
		{"customer_id", "dim_customer_id"},
	}
	keyRe := regexp.MustCompile(`^[a-zA-Z_][0-9a-zA-Z_]*$`)
	for _, tt := range tests {
		got := GroupByKey(tt.dimension)
		if got != tt.want {
			t.Errorf("GroupByKey(%q) = %q, want %q", tt.dimension, got, tt.want)
		}
		if !keyRe.MatchString(got) {
			t.Errorf("GroupByKey(%q) = %q does not match OpenMeter's group-by key pattern", tt.dimension, got)
		}
	}
}

func TestDimensionJSONPath(t *testing.T) {
	tests := []struct {
		dimension string
		want      string
		wantErr   bool
	}{
		{"region", "$.dimensions.region", false},
		{"resource.type", `$.dimensions."resource.type"`, false},
		{"k8s.io/name", `$.dimensions."k8s.io/name"`, false},
		{"9lives", `$.dimensions."9lives"`, false},
		{"a-b", `$.dimensions."a-b"`, false},
		{`has"quote`, "", true},
		{"has[bracket]", "", true},
		{".leading", "", true},
		{"with space", "", true},
	}
	for _, tt := range tests {
		got, err := DimensionJSONPath(tt.dimension)
		if tt.wantErr {
			if !IsPermanent(err) {
				t.Errorf("DimensionJSONPath(%q) error = %v, want a PermanentError", tt.dimension, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("DimensionJSONPath(%q) = %q, %v; want %q", tt.dimension, got, err, tt.want)
		}
	}
}

func TestMeterGroupBy(t *testing.T) {
	got, err := MeterGroupBy([]string{"region", "resource.type", "region", "project_name"})
	if err != nil {
		t.Fatalf("MeterGroupBy: %v", err)
	}
	want := map[string]string{
		"region":        "$.dimensions.region",
		"resource_type": `$.dimensions."resource.type"`,
		"project_name":  "$.dimensions.project_name",
	}
	if !maps.Equal(got, want) {
		t.Errorf("MeterGroupBy = %v, want %v", got, want)
	}

	for _, dims := range [][]string{
		{"resource.type", "resource_type"}, // both map to resource_type
		{"a-b", "a.b"},
		{""},
		{`bad"name`},
	} {
		if _, err := MeterGroupBy(dims); !IsPermanent(err) {
			t.Errorf("MeterGroupBy(%q) error = %v, want a PermanentError", dims, err)
		}
	}
}

func TestMeterValueProperty(t *testing.T) {
	for _, a := range []billingv1alpha1.MeterAggregation{
		billingv1alpha1.MeterAggregationSum,
		billingv1alpha1.MeterAggregationCount,
		billingv1alpha1.MeterAggregationMax,
		billingv1alpha1.MeterAggregationMin,
		billingv1alpha1.MeterAggregationLatest,
		billingv1alpha1.MeterAggregationAverage,
	} {
		if got := MeterValueProperty(a); got != ValuePropertyValue {
			t.Errorf("MeterValueProperty(%s) = %q, want %q", a, got, ValuePropertyValue)
		}
	}
	if got := MeterValueProperty(billingv1alpha1.MeterAggregationUniqueCount); got != ValuePropertyResourceUID {
		t.Errorf("MeterValueProperty(UniqueCount) = %q, want %q (distinct resources, not distinct quantities)",
			got, ValuePropertyResourceUID)
	}
}
