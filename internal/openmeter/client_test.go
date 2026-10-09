// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	om "github.com/openmeterio/openmeter/api/client/go"
)

// fakeServer is a minimal in-memory stand-in for the OpenMeter API. Each
// resource type owns its own routing (see serveMeters in meter_test.go,
// serveCustomers in customer_test.go): ServeHTTP just dispatches to them in
// turn, so adding a new resource type's tests never requires touching an
// existing resource's test file — only this file (one dispatch line, plus
// the new resource's storage map below) and the new resource's own _test.go.
type fakeServer struct {
	meters    map[string]om.Meter
	customers map[string]om.Customer
	// invoices is keyed by customer key, mirroring how ListInvoices is
	// always called.
	invoices map[string][]om.Invoice
	// stripeAppData is keyed by customer key.
	stripeAppData map[string]om.StripeCustomerAppData
	// billingProfiles is keyed by profile id.
	billingProfiles map[string]om.BillingProfile
	// billingProfileOverrides is keyed by customer key.
	billingProfileOverrides map[string]om.BillingProfileCustomerOverrideCreate
	// features is keyed by feature KEY; featureByID maps id -> key so the
	// /features/{idOrKey} routes resolve either identifier, mirroring
	// OpenMeter's idOrKey path parameter. Keeping list iterables keyed by
	// key alone avoids double-counting features in list responses.
	features    map[string]om.Feature
	featureByID map[string]string
	// plans is keyed by plan id. ListPlans filters to non-deleted plans,
	// mirroring the real API's default.
	plans map[string]om.Plan
	// planSeq generates plan ids; planWrites counts every plan mutation
	// (create, update, publish, archive, delete).
	planSeq    int
	planWrites int
	// featureCreates counts feature POSTs.
	featureCreates int

	// statusOverride, when non-zero, short-circuits every request with that
	// status code. Tests use it to inject wire-level failures (429s, 5xxs)
	// that aren't specific to any one resource's routing.
	statusOverride int
	// emptyBodyOnWrite reproduces what was observed against a real OpenMeter
	// instance right after a fresh install: a write applies (the resource is
	// stored) but the response comes back 2xx with no body, so the SDK can't
	// populate the typed response field. Shared across resources since every
	// resource's create/update implements the same fall-back-to-GET fix.
	emptyBodyOnWrite bool

	// Write counters. Every Ensure* is supposed to converge: once the remote
	// state matches, re-running it must issue NO further writes. Nothing
	// enforced that before, which let two separate never-converging drift
	// checks ship (see TestEnsureBillingProfile_ConvergesWhenServerNormalizes
	// AlignmentD and TestEnsureCustomer_ConvergesAfterClearingEmail), each
	// silently issuing a PUT on every single reconcile forever.
	customerUpdates       int
	billingProfileUpdates int
	stripeAppDataWrites   int

	// ingestedEvents accumulates every CloudEvent ever POSTed to the
	// ingest route, in submission order, across all calls.
	ingestedEvents []cloudevents.Event
}

func newTestClient(t *testing.T) (Client, *fakeServer) {
	t.Helper()
	f := &fakeServer{
		meters:                  map[string]om.Meter{},
		customers:               map[string]om.Customer{},
		invoices:                map[string][]om.Invoice{},
		stripeAppData:           map[string]om.StripeCustomerAppData{},
		billingProfiles:         map[string]om.BillingProfile{},
		billingProfileOverrides: map[string]om.BillingProfileCustomerOverrideCreate{},
		features:                map[string]om.Feature{},
		featureByID:             map[string]string{},
		plans:                   map[string]om.Plan{},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, f
}

// ServeHTTP dispatches to each resource's own handler in turn; the first one
// that recognizes the request path handles it and returns true.
func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.statusOverride != 0 {
		w.WriteHeader(f.statusOverride)
		return
	}
	if f.serveMeters(w, r) {
		return
	}
	// serveStripeAppData must be checked before serveCustomers: its route
	// (/api/v1/customers/{key}/apps) shares the /api/v1/customers prefix
	// serveCustomers matches on, so the more specific route goes first.
	if f.serveStripeAppData(w, r) {
		return
	}
	if f.serveCustomers(w, r) {
		return
	}
	if f.serveInvoices(w, r) {
		return
	}
	if f.serveBillingProfiles(w, r) {
		return
	}
	if f.serveFeatures(w, r) {
		return
	}
	if f.servePlans(w, r) {
		return
	}
	if f.serveIngest(w, r) {
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

// lookupFeature resolves a feature by id or key. OpenMeter's
// /features/{idOrKey} path parameter accepts either, so an id lookup is
// redirected through featureByID to the feature's key.
func (f *fakeServer) lookupFeature(idOrKey string) (om.Feature, bool) {
	key := idOrKey
	if id, ok := f.featureByID[idOrKey]; ok {
		key = id
	}
	feat, ok := f.features[key]
	return feat, ok
}

// serveFeatures mirrors the real OpenMeter feature routes used by the client:
//
//	POST   /api/v1/features            create (201, 409 on duplicate key)
//	GET    /api/v1/features/{idOrKey}  get by id or key (200, 404)
//	GET    /api/v1/features            list, optional ?meterSlug= filter (200)
//	DELETE /api/v1/features/{idOrKey}  archive by id or key (204, 404)
//
// Delete mirrors OpenMeter's soft-archive semantics: the feature stays in the
// store with ArchivedAt set, and the default list (no includeArchived) hides
// it — exactly the behavior that unblocks meter deletion (active features
// block it, archived ones don't).
func (f *fakeServer) serveFeatures(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/v1/features") {
		return false
	}
	// A path element beyond the collection root means an idOrKey route.
	isCollection := r.URL.Path == "/api/v1/features"
	idOrKey := strings.TrimPrefix(r.URL.Path, "/api/v1/features/")

	switch {
	case r.Method == http.MethodPost && isCollection:
		var body om.FeatureCreateInputs
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		now := time.Now().UTC()
		if existing, exists := f.features[body.Key]; exists && existing.ArchivedAt == nil {
			w.WriteHeader(http.StatusConflict)
			return true
		}
		feat := om.Feature{
			Id:                          "id-" + body.Key,
			Key:                         body.Key,
			Name:                        body.Name,
			MeterSlug:                   body.MeterSlug,
			AdvancedMeterGroupByFilters: body.AdvancedMeterGroupByFilters,
			Metadata:                    body.Metadata,
			CreatedAt:                   now,
			UpdatedAt:                   now,
		}
		f.featureCreates++
		f.features[body.Key] = feat
		f.featureByID[feat.Id] = feat.Key
		if !f.emptyBodyOnWrite {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(http.StatusCreated)
		if !f.emptyBodyOnWrite {
			_ = json.NewEncoder(w).Encode(feat)
		}

	case r.Method == http.MethodGet && !isCollection && strings.Trim(idOrKey, "/") != "":
		feat, ok := f.lookupFeature(strings.TrimSuffix(idOrKey, "/"))
		if !ok || feat.ArchivedAt != nil {
			// The real GET excludes archived features.
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(feat)

	case r.Method == http.MethodGet && isCollection:
		var out []om.Feature
		meterSlug := r.URL.Query().Get("meterSlug")
		for _, feat := range f.features {
			if feat.ArchivedAt != nil {
				// Default list excludes archived features, mirroring
				// ListFeatures' doc contract (active-only).
				continue
			}
			if meterSlug != "" {
				if feat.MeterSlug == nil || *feat.MeterSlug != meterSlug {
					continue
				}
			}
			out = append(out, feat)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// The real API answers with a bare array unless paging is
		// requested, in which case it returns a paginated envelope.
		if r.URL.Query().Get("page") == "" {
			_ = json.NewEncoder(w).Encode(out)
			return true
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
		page, pageSize := pageParams(r)
		start, end := pageBounds(page, pageSize, len(out))
		_ = json.NewEncoder(w).Encode(om.FeaturePaginatedResponse{
			Items:      out[start:end],
			Page:       page,
			PageSize:   pageSize,
			TotalCount: len(out),
		})

	case r.Method == http.MethodDelete && !isCollection && strings.Trim(idOrKey, "/") != "":
		feat, ok := f.lookupFeature(strings.TrimSuffix(idOrKey, "/"))
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		now := time.Now().UTC()
		feat.ArchivedAt = &now
		f.features[feat.Key] = feat
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
	return true
}

// servePlans mirrors OpenMeter's plan lifecycle closely enough to exercise
// the client's versioning logic:
//
//	GET    /api/v1/plans                  list (key, includeDeleted, paging)
//	POST   /api/v1/plans                  create a draft; next version per key;
//	                                      400 while a draft exists for the key
//	PUT    /api/v1/plans/{id}             replace; draft/scheduled only
//	POST   /api/v1/plans/{id}/publish     draft -> active; archives the
//	                                      previous active version of the key
//	POST   /api/v1/plans/{id}/archive     active -> archived
//	DELETE /api/v1/plans/{id}             draft/archived/scheduled only
func (f *fakeServer) servePlans(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/v1/plans") {
		return false
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/plans"), "/")
	parts := strings.Split(rest, "/")
	writeJSON := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	badRequest := func(msg string) {
		writeJSON(http.StatusBadRequest, map[string]string{"title": "Bad Request", "detail": msg})
	}

	switch {
	case rest == "" && r.Method == http.MethodGet:
		q := r.URL.Query()
		keys := q["key"]
		includeDeleted := q.Get("includeDeleted") == "true"
		var all []om.Plan
		for _, plan := range f.plans {
			if plan.DeletedAt != nil && !includeDeleted {
				continue
			}
			if len(keys) > 0 && !slices.Contains(keys, plan.Key) {
				continue
			}
			all = append(all, plan)
		}
		// Stable order so pagination is consistent across page requests.
		sort.Slice(all, func(i, j int) bool { return all[i].Id < all[j].Id })
		page, pageSize := pageParams(r)
		start, end := pageBounds(page, pageSize, len(all))
		writeJSON(http.StatusOK, om.PlanPaginatedResponse{
			Items:      all[start:end],
			Page:       page,
			PageSize:   pageSize,
			TotalCount: len(all),
		})

	case rest == "" && r.Method == http.MethodPost:
		var body om.PlanCreate
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(err.Error())
			return true
		}
		version := 1
		for _, p := range f.plans {
			if p.Key != body.Key {
				continue
			}
			if p.DeletedAt == nil && p.Status == om.PlanStatusDraft {
				badRequest("only a single draft version is allowed for Plan")
				return true
			}
			if p.Version >= version {
				version = p.Version + 1
			}
		}
		f.planSeq++
		f.planWrites++
		now := time.Now().UTC()
		plan := om.Plan{
			Id:             "plan-" + strconv.Itoa(f.planSeq),
			Key:            body.Key,
			Name:           body.Name,
			Description:    body.Description,
			Currency:       body.Currency,
			BillingCadence: body.BillingCadence,
			Metadata:       body.Metadata,
			Phases:         body.Phases,
			Status:         om.PlanStatusDraft,
			Version:        version,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		f.plans[plan.Id] = plan
		writeJSON(http.StatusCreated, plan)

	case len(parts) == 1 && r.Method == http.MethodPut:
		plan, ok := f.plans[parts[0]]
		if !ok || plan.DeletedAt != nil {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		if plan.Status != om.PlanStatusDraft && plan.Status != om.PlanStatusScheduled {
			badRequest("only Plans in [draft scheduled] can be updated")
			return true
		}
		var body om.PlanReplaceUpdate
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(err.Error())
			return true
		}
		f.planWrites++
		plan.Name = body.Name
		plan.Description = body.Description
		plan.Metadata = body.Metadata
		plan.Phases = body.Phases
		plan.UpdatedAt = time.Now().UTC()
		f.plans[plan.Id] = plan
		writeJSON(http.StatusOK, plan)

	case len(parts) == 2 && parts[1] == "publish" && r.Method == http.MethodPost:
		plan, ok := f.plans[parts[0]]
		if !ok || plan.DeletedAt != nil {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		if plan.Status != om.PlanStatusDraft && plan.Status != om.PlanStatusScheduled {
			badRequest("only Plans in [draft scheduled] can be published")
			return true
		}
		now := time.Now().UTC()
		for id, p := range f.plans {
			if p.Key == plan.Key && p.DeletedAt == nil && p.Status == om.PlanStatusActive {
				p.Status = om.PlanStatusArchived
				p.EffectiveTo = &now
				f.plans[id] = p
			}
		}
		f.planWrites++
		plan.Status = om.PlanStatusActive
		plan.EffectiveFrom = &now
		f.plans[plan.Id] = plan
		writeJSON(http.StatusOK, plan)

	case len(parts) == 2 && parts[1] == "archive" && r.Method == http.MethodPost:
		plan, ok := f.plans[parts[0]]
		if !ok || plan.DeletedAt != nil {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		if plan.Status != om.PlanStatusActive {
			badRequest("only Plans in [active] can be archived")
			return true
		}
		f.planWrites++
		now := time.Now().UTC()
		plan.Status = om.PlanStatusArchived
		plan.EffectiveTo = &now
		f.plans[plan.Id] = plan
		writeJSON(http.StatusOK, plan)

	case len(parts) == 1 && r.Method == http.MethodDelete:
		plan, ok := f.plans[parts[0]]
		if !ok || plan.DeletedAt != nil {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		if plan.Status == om.PlanStatusActive {
			badRequest("only Plans in [archived scheduled draft] can be deleted, but it has active state")
			return true
		}
		f.planWrites++
		now := time.Now().UTC()
		plan.DeletedAt = &now
		f.plans[plan.Id] = plan
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
	return true
}

// pageParams reads page/pageSize query parameters with the real API's
// defaults (page 1, 100 items).
func pageParams(r *http.Request) (page, pageSize int) {
	page, pageSize = 1, 100
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
		page = p
	}
	if ps, err := strconv.Atoi(r.URL.Query().Get("pageSize")); err == nil && ps > 0 {
		pageSize = ps
	}
	return page, pageSize
}

// pageBounds returns the slice bounds of one page over n items.
func pageBounds(page, pageSize, n int) (start, end int) {
	start = min((page-1)*pageSize, n)
	end = min(start+pageSize, n)
	return start, end
}
