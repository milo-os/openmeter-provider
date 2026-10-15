// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// newQueryServer serves POST /api/v1/meters/{slug}/query with handle, after
// recording the decoded request body in *got.
func newQueryServer(t *testing.T, got *om.MeterQueryRequest, gotSlug *string, handle func(w http.ResponseWriter)) Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/meters/{slug}/query", func(w http.ResponseWriter, r *http.Request) {
		*gotSlug = r.PathValue("slug")
		if err := json.NewDecoder(r.Body).Decode(got); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		handle(w)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestQueryMeter_SendsTheQueryAndParsesRows(t *testing.T) {
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	to := from.Add(48 * time.Hour)
	day := om.WindowSizeDay

	var got om.MeterQueryRequest
	var slug string
	c := newQueryServer(t, &got, &slug, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"from": "2026-10-05T00:00:00Z",
			"to": "2026-10-07T00:00:00Z",
			"windowSize": "DAY",
			"data": [
				{"value": 18.2, "windowStart": "2026-10-05T00:00:00Z", "windowEnd": "2026-10-06T00:00:00Z",
				 "customerId": "01CUSTOMER", "subject": null,
				 "groupBy": {"region": "us-east4", "customer_id": "01CUSTOMER"}},
				{"value": 3, "windowStart": "2026-10-06T00:00:00Z", "windowEnd": "2026-10-07T00:00:00Z",
				 "subject": null, "groupBy": {"region": null}}
			]
		}`))
	})

	rows, err := c.QueryMeter(context.Background(), MeterQuery{
		Slug:          "compute_cpu_hours",
		From:          from,
		To:            to,
		WindowSize:    &day,
		TimeZone:      "Europe/Madrid",
		CustomerIDs:   []CustomerID{"01CUSTOMER", "01OTHER"},
		GroupBy:       []string{"region"},
		FilterGroupBy: map[string][]string{"project_name": {"web", "api"}},
	})
	if err != nil {
		t.Fatalf("QueryMeter: %v", err)
	}

	if slug != "compute_cpu_hours" {
		t.Errorf("slug = %q", slug)
	}
	if got.From == nil || !got.From.Equal(from) || got.To == nil || !got.To.Equal(to) {
		t.Errorf("from/to = %v/%v", got.From, got.To)
	}
	if got.WindowSize == nil || *got.WindowSize != om.WindowSizeDay {
		t.Errorf("windowSize = %v", got.WindowSize)
	}
	if got.WindowTimeZone == nil || *got.WindowTimeZone != "Europe/Madrid" {
		t.Errorf("windowTimeZone = %v", got.WindowTimeZone)
	}
	if got.FilterCustomerId == nil || len(*got.FilterCustomerId) != 2 {
		t.Errorf("filterCustomerId = %v", got.FilterCustomerId)
	}
	if got.GroupBy == nil || len(*got.GroupBy) != 1 || (*got.GroupBy)[0] != "region" {
		t.Errorf("groupBy = %v", got.GroupBy)
	}
	if got.AdvancedMeterGroupByFilters == nil {
		t.Fatal("advancedMeterGroupByFilters not sent")
	}
	if in := (*got.AdvancedMeterGroupByFilters)["project_name"].In; in == nil || len(*in) != 2 {
		t.Errorf("project_name filter = %+v", (*got.AdvancedMeterGroupByFilters)["project_name"])
	}

	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Value != 18.2 || rows[0].CustomerID != "01CUSTOMER" || rows[0].GroupBy["region"] != "us-east4" {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if _, echoed := rows[0].GroupBy["customer_id"]; echoed {
		t.Errorf("row 0 kept an unrequested group-by key: %+v", rows[0].GroupBy)
	}
	if rows[1].GroupBy["region"] != "" || rows[1].CustomerID != "" {
		t.Errorf("row 1 = %+v, want an empty region and no customer", rows[1])
	}
}

func TestQueryMeter_OmitsUnsetFilters(t *testing.T) {
	var got om.MeterQueryRequest
	var slug string
	c := newQueryServer(t, &got, &slug, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": []}`))
	})

	rows, err := c.QueryMeter(context.Background(), MeterQuery{
		Slug: "m",
		From: time.Unix(0, 0),
		To:   time.Unix(3600, 0),
	})
	if err != nil {
		t.Fatalf("QueryMeter: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %+v", rows)
	}
	if got.WindowSize != nil || got.WindowTimeZone != nil || got.FilterCustomerId != nil || got.GroupBy != nil || got.AdvancedMeterGroupByFilters != nil {
		t.Errorf("unset fields were sent: %+v", got)
	}
}

func TestQueryMeter_MeterNotFound(t *testing.T) {
	var got om.MeterQueryRequest
	var slug string
	c := newQueryServer(t, &got, &slug, func(w http.ResponseWriter) {
		writeMeterNotFound(w, "missing")
	})

	_, err := c.QueryMeter(context.Background(), MeterQuery{Slug: "missing", From: time.Unix(0, 0), To: time.Unix(60, 0)})
	if !errors.Is(err, ErrMeterNotFound) {
		t.Fatalf("err = %v, want ErrMeterNotFound", err)
	}
}

func TestQueryMeter_BadRequestIsPermanent(t *testing.T) {
	var got om.MeterQueryRequest
	var slug string
	c := newQueryServer(t, &got, &slug, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Bad Request","status":400,"detail":"invalid group by: region"}`))
	})

	_, err := c.QueryMeter(context.Background(), MeterQuery{Slug: "m", From: time.Unix(0, 0), To: time.Unix(60, 0)})
	if !IsPermanent(err) {
		t.Fatalf("err = %v, want a PermanentError", err)
	}
}

func TestQueryMeter_ServerErrorIsTransient(t *testing.T) {
	var got om.MeterQueryRequest
	var slug string
	c := newQueryServer(t, &got, &slug, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	_, err := c.QueryMeter(context.Background(), MeterQuery{Slug: "m", From: time.Unix(0, 0), To: time.Unix(60, 0)})
	if !IsTransient(err) {
		t.Fatalf("err = %v, want a TransientError", err)
	}
}

func TestQueryMeter_SendsFeatureFilters(t *testing.T) {
	var got om.MeterQueryRequest
	var slug string
	c := newQueryServer(t, &got, &slug, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": []}`))
	})

	nin := []string{"eu-west1"}
	eq := "web"
	if _, err := c.QueryMeter(context.Background(), MeterQuery{
		Slug:          "m",
		From:          time.Unix(0, 0),
		To:            time.Unix(3600, 0),
		FilterGroupBy: map[string][]string{"project_name": {"web", "api"}},
		Filters: map[string]om.FilterString{
			"region":       {Nin: &nin},
			"project_name": {Eq: &eq},
		},
	}); err != nil {
		t.Fatalf("QueryMeter: %v", err)
	}

	filters := *got.AdvancedMeterGroupByFilters
	if r := filters["region"]; r.Nin == nil || len(*r.Nin) != 1 || (*r.Nin)[0] != "eu-west1" {
		t.Errorf("region filter = %+v, want $nin [eu-west1]", r)
	}
	// The same key in both must match both.
	p := filters["project_name"]
	if p.And == nil || len(*p.And) != 2 {
		t.Fatalf("project_name filter = %+v, want $and of the two", p)
	}
	in, feature := (*p.And)[0], (*p.And)[1]
	if in.In == nil || len(*in.In) != 2 || feature.Eq == nil || *feature.Eq != "web" {
		t.Errorf("project_name $and = %+v", *p.And)
	}
}
