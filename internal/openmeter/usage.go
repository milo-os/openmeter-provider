// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// MeterQuery reads a meter's usage. It is sent as OpenMeter's POST meter
// query, so long customer and filter lists never hit URL length limits.
type MeterQuery struct {
	// Slug identifies the meter (see MeterSlug).
	Slug string
	// From is the inclusive start and To the exclusive end of the range.
	From time.Time
	To   time.Time
	// WindowSize splits the result into time buckets; nil returns one
	// aggregate for the whole range.
	WindowSize *om.WindowSize
	// TimeZone is the IANA zone windows are aligned to. Empty means UTC.
	TimeZone string
	// CustomerIDs limits the query to these customers. OpenMeter then
	// returns one row per customer (it adds customer_id to the group-by).
	CustomerIDs []CustomerID
	// GroupBy lists OpenMeter group-by keys (see GroupByKey).
	GroupBy []string
	// FilterGroupBy keeps only rows whose group-by key has one of the listed
	// values, keyed by OpenMeter group-by key.
	FilterGroupBy map[string][]string
}

// MeterQueryRow is one row of a meter query: the meter's aggregate for one
// window, customer and combination of group-by values.
type MeterQueryRow struct {
	WindowStart time.Time
	WindowEnd   time.Time
	// CustomerID is set when the query filtered by customers.
	CustomerID CustomerID
	// GroupBy maps each requested group-by key to its value; "" when the
	// events did not set it.
	GroupBy map[string]string
	Value   float64
}

// QueryMeter runs q. Returns ErrMeterNotFound when OpenMeter has no meter
// with q.Slug.
func (c *client) QueryMeter(ctx context.Context, q MeterQuery) ([]MeterQueryRow, error) {
	if q.Slug == "" {
		return nil, &PermanentError{Err: errors.New("slug is required")}
	}

	body := om.MeterQueryRequest{
		From:       &q.From,
		To:         &q.To,
		WindowSize: q.WindowSize,
	}
	if q.TimeZone != "" {
		body.WindowTimeZone = &q.TimeZone
	}
	if len(q.CustomerIDs) > 0 {
		ids := make([]string, len(q.CustomerIDs))
		for i, id := range q.CustomerIDs {
			ids[i] = string(id)
		}
		body.FilterCustomerId = &ids
	}
	if len(q.GroupBy) > 0 {
		groupBy := append([]string(nil), q.GroupBy...)
		body.GroupBy = &groupBy
	}
	if len(q.FilterGroupBy) > 0 {
		filters := make(map[string]om.FilterString, len(q.FilterGroupBy))
		for key, values := range q.FilterGroupBy {
			in := append([]string(nil), values...)
			filters[key] = om.FilterString{In: &in}
		}
		body.AdvancedMeterGroupByFilters = &filters
	}

	resp, err := c.api.QueryMeterPostWithResponse(ctx, q.Slug, body)
	if err != nil {
		return nil, classify(nil, nil, err)
	}
	if resp.StatusCode() == http.StatusNotFound {
		if isOpenMeterNotFound(resp.HTTPResponse, resp.Body) {
			return nil, fmt.Errorf("%w: %s", ErrMeterNotFound, q.Slug)
		}
		return nil, unrecognizedNotFound("query", q.Slug, resp.Body)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, &PermanentError{Err: fmt.Errorf("query meter %q: empty response body", q.Slug)}
	}

	rows := make([]MeterQueryRow, 0, len(resp.JSON200.Data))
	for _, r := range resp.JSON200.Data {
		row := MeterQueryRow{
			WindowStart: r.WindowStart,
			WindowEnd:   r.WindowEnd,
			GroupBy:     make(map[string]string, len(q.GroupBy)),
			Value:       r.Value,
		}
		if r.CustomerId != nil {
			row.CustomerID = CustomerID(*r.CustomerId)
		}
		// Keep only the requested keys: OpenMeter may echo customer_id or
		// subject here as well.
		for _, key := range q.GroupBy {
			if v := r.GroupBy[key]; v != nil {
				row.GroupBy[key] = *v
			} else {
				row.GroupBy[key] = ""
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}
