// SPDX-License-Identifier: AGPL-3.0-only

package usagequery

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	usagev1alpha1 "go.miloapis.com/billing/api/usage/v1alpha1"
	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

// meterQueries bounds concurrent OpenMeter queries for one UsageQuery.
const meterQueries = 8

// meterPlan is one meter to read, checked against its MeterDefinition.
type meterPlan struct {
	definition billingv1alpha1.MeterDefinition
	slug       string
	// groupByKeys maps each dimension the meter can be grouped by to its
	// OpenMeter group-by key.
	groupByKeys map[string]string
}

// planMeters returns the meters a query reads, in result order: spec.meters
// as given, or every published and deprecated MeterDefinition by name.
// Unknown meters and dimensions a meter does not have are bad requests.
func planMeters(ctx context.Context, milo client.Reader, spec *usagev1alpha1.UsageQuerySpec) ([]meterPlan, error) {
	var definitions []billingv1alpha1.MeterDefinition
	if len(spec.Meters) > 0 {
		for _, name := range spec.Meters {
			var md billingv1alpha1.MeterDefinition
			if err := milo.Get(ctx, client.ObjectKey{Name: name}, &md); err != nil {
				if apierrors.IsNotFound(err) {
					return nil, apierrors.NewBadRequest(fmt.Sprintf("spec.meters: MeterDefinition %q does not exist", name))
				}
				return nil, apierrors.NewInternalError(fmt.Errorf("reading MeterDefinition %q: %w", name, err))
			}
			definitions = append(definitions, md)
		}
	} else {
		var list billingv1alpha1.MeterDefinitionList
		if err := milo.List(ctx, &list); err != nil {
			return nil, apierrors.NewInternalError(fmt.Errorf("listing MeterDefinitions: %w", err))
		}
		for _, md := range list.Items {
			// Deprecated meters still carry usage until they are retired.
			if md.Spec.Phase == billingv1alpha1.PhasePublished || md.Spec.Phase == billingv1alpha1.PhaseDeprecated {
				definitions = append(definitions, md)
			}
		}
		slices.SortFunc(definitions, func(a, b billingv1alpha1.MeterDefinition) int { return strings.Compare(a.Name, b.Name) })
	}

	plans := make([]meterPlan, 0, len(definitions))
	for _, md := range definitions {
		plan := meterPlan{
			definition:  md,
			slug:        openmeter.MeterSlug(md.Spec.MeterName),
			groupByKeys: map[string]string{},
		}
		for _, dim := range openmeter.MeterDimensions(&md) {
			plan.groupByKeys[dim] = openmeter.GroupByKey(dim)
		}

		for i, grouping := range spec.Groupings {
			for _, dim := range grouping.GroupBy {
				if _, ok := plan.groupByKeys[dim]; ok {
					continue
				}
				// When reading every meter, a breakdown by a dimension only
				// some meters have is empty for the others (see
				// runGrouping) rather than failing the whole query.
				if len(spec.Meters) == 0 {
					continue
				}
				return nil, apierrors.NewBadRequest(fmt.Sprintf(
					"spec.groupings[%d].groupBy: MeterDefinition %q has no dimension %q (it has: %s)",
					i, md.Name, dim, strings.Join(slices.Sorted(maps.Keys(plan.groupByKeys)), ", ")))
			}
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// executor runs a query's meters against OpenMeter.
type executor struct {
	openMeter openmeter.Client
	spec      *usagev1alpha1.UsageQuerySpec
	customers []openmeter.CustomerID
	projects  []string
}

// run returns one MeterUsage per plan, in order. A meter OpenMeter does not
// know, or rejects, gets an error of its own; transient failures fail the
// whole query.
func (e *executor) run(ctx context.Context, plans []meterPlan) ([]usagev1alpha1.MeterUsage, error) {
	results := make([]usagev1alpha1.MeterUsage, len(plans))
	for i, plan := range plans {
		results[i] = usagev1alpha1.MeterUsage{
			MeterDefinition: plan.definition.Name,
			MeterName:       plan.definition.Spec.MeterName,
			Aggregation:     string(plan.definition.Spec.Measurement.Aggregation),
			Unit:            plan.definition.Spec.Measurement.Unit,
			Groupings:       make([]usagev1alpha1.UsageGroupingResult, len(e.spec.Groupings)),
		}
	}

	meterErrs := make([][]error, len(plans))
	for i := range plans {
		meterErrs[i] = make([]error, len(e.spec.Groupings))
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(meterQueries)
	for i, plan := range plans {
		for j, grouping := range e.spec.Groupings {
			g.Go(func() error {
				result, err := e.runGrouping(gctx, plan, grouping)
				if err != nil {
					if openmeter.IsTransient(err) || !isMeterError(err) {
						return err
					}
					meterErrs[i][j] = err
					return nil
				}
				results[i].Groupings[j] = result
				return nil
			})
		}
	}
	if err := g.Wait(); err != nil {
		var status apierrors.APIStatus
		if errors.As(err, &status) {
			return nil, err
		}
		return nil, apierrors.NewServiceUnavailable(fmt.Sprintf("querying usage: %v", err))
	}

	for i := range results {
		if err := errors.Join(meterErrs[i]...); err != nil {
			results[i].Groupings = nil
			results[i].Error = meterErrorMessage(err)
		}
	}
	return results, nil
}

// isMeterError reports whether err concerns only this meter: OpenMeter has no
// such meter, or rejected the query (a permanent 4xx).
func isMeterError(err error) bool {
	return errors.Is(err, openmeter.ErrMeterNotFound) || errors.Is(err, errMeterLacksDimension) || openmeter.IsPermanent(err)
}

var errMeterLacksDimension = errors.New("this meter cannot be filtered by project")

func meterErrorMessage(err error) string {
	switch {
	case errors.Is(err, openmeter.ErrMeterNotFound):
		return "this meter is not set up in OpenMeter yet"
	case errors.Is(err, errMeterLacksDimension):
		return errMeterLacksDimension.Error()
	default:
		return fmt.Sprintf("OpenMeter rejected the query: %v", err)
	}
}

// runGrouping reads one meter broken down by one grouping.
func (e *executor) runGrouping(ctx context.Context, plan meterPlan, grouping usagev1alpha1.UsageGrouping) (usagev1alpha1.UsageGroupingResult, error) {
	result := usagev1alpha1.UsageGroupingResult{GroupBy: grouping.GroupBy}

	// No customers means no usage: skip OpenMeter, which would read every
	// customer's usage if given an empty customer filter.
	if len(e.customers) == 0 {
		return result, nil
	}

	var keys []string
	for _, dim := range grouping.GroupBy {
		key, ok := plan.groupByKeys[dim]
		if !ok {
			// Only reachable when reading every meter (see planMeters):
			// this meter has no such breakdown, so it has no series.
			return result, nil
		}
		keys = append(keys, key)
	}

	q := openmeter.MeterQuery{
		Slug:        plan.slug,
		From:        e.spec.StartTime.Time,
		To:          e.spec.EndTime.Time,
		TimeZone:    e.spec.TimeZone,
		CustomerIDs: e.customers,
		GroupBy:     keys,
	}
	if len(e.projects) > 0 {
		key, ok := plan.groupByKeys[billingv1alpha1.SystemDimensionProjectName]
		if !ok {
			return result, errMeterLacksDimension
		}
		q.FilterGroupBy = map[string][]string{key: e.projects}
	}

	agg := plan.definition.Spec.Measurement.Aggregation
	var series map[string]*usagev1alpha1.UsageSeries

	if e.spec.Granularity == "" {
		rows, err := e.openMeter.QueryMeter(ctx, q)
		if err != nil {
			return result, err
		}
		series = buildSeries(rows, grouping.GroupBy, keys, agg, false, nil)
	} else {
		windowed := q
		windowed.WindowSize = windowSize(e.spec.Granularity)
		rows, err := e.openMeter.QueryMeter(ctx, windowed)
		if err != nil {
			return result, err
		}

		// The sum of a Sum or Count meter's buckets is its total; any other
		// aggregation needs its own query over the whole range.
		var totals []openmeter.MeterQueryRow
		if agg != billingv1alpha1.MeterAggregationSum && agg != billingv1alpha1.MeterAggregationCount {
			if totals, err = e.openMeter.QueryMeter(ctx, q); err != nil {
				return result, err
			}
		}
		series = buildSeries(rows, grouping.GroupBy, keys, agg, true, totals)
		clampPoints(series, e.spec.StartTime.Time, e.spec.EndTime.Time)
	}

	for _, s := range series {
		result.Series = append(result.Series, *s)
	}
	slices.SortFunc(result.Series, func(a, b usagev1alpha1.UsageSeries) int {
		return cmp.Compare(groupSortKey(a.Group, grouping.GroupBy), groupSortKey(b.Group, grouping.GroupBy))
	})
	return result, nil
}

func windowSize(g usagev1alpha1.UsageGranularity) *om.WindowSize {
	var w om.WindowSize
	switch g {
	case usagev1alpha1.UsageGranularityHour:
		w = om.WindowSizeHour
	case usagev1alpha1.UsageGranularityMonth:
		w = om.WindowSizeMonth
	default:
		w = om.WindowSizeDay
	}
	return &w
}

// buildSeries merges OpenMeter rows into one series per combination of group
// values. OpenMeter returns one row per customer, so rows for the same group
// and window are combined with the meter's aggregation (see combine).
//
// With windowed set, rows are buckets and become points, and the total comes
// from totals (an un-windowed query) when given, or else from the sum of the
// points. Without it, rows are totals.
func buildSeries(
	rows []openmeter.MeterQueryRow,
	dims, keys []string,
	agg billingv1alpha1.MeterAggregation,
	windowed bool,
	totals []openmeter.MeterQueryRow,
) map[string]*usagev1alpha1.UsageSeries {
	type bucket struct {
		start, end time.Time
		values     []float64
	}
	type group struct {
		values  map[string]string
		buckets map[time.Time]*bucket
		totals  []float64
	}
	groups := map[string]*group{}

	groupOf := func(row openmeter.MeterQueryRow) *group {
		values := make(map[string]string, len(dims))
		for i, dim := range dims {
			values[dim] = row.GroupBy[keys[i]]
		}
		id := groupSortKey(values, dims)
		g, ok := groups[id]
		if !ok {
			g = &group{values: values, buckets: map[time.Time]*bucket{}}
			groups[id] = g
		}
		return g
	}

	for _, row := range rows {
		g := groupOf(row)
		if !windowed {
			g.totals = append(g.totals, row.Value)
			continue
		}
		b, ok := g.buckets[row.WindowStart]
		if !ok {
			b = &bucket{start: row.WindowStart, end: row.WindowEnd}
			g.buckets[row.WindowStart] = b
		}
		b.values = append(b.values, row.Value)
	}
	for _, row := range totals {
		g := groupOf(row)
		g.totals = append(g.totals, row.Value)
	}

	out := make(map[string]*usagev1alpha1.UsageSeries, len(groups))
	for id, g := range groups {
		s := &usagev1alpha1.UsageSeries{}
		if len(dims) > 0 {
			s.Group = g.values
		}

		if windowed {
			buckets := slices.SortedFunc(func(yield func(*bucket) bool) {
				for _, b := range g.buckets {
					if !yield(b) {
						return
					}
				}
			}, func(a, b *bucket) int { return a.start.Compare(b.start) })

			var pointSum float64
			for _, b := range buckets {
				v := combine(agg, b.values)
				pointSum += v
				s.Points = append(s.Points, usagev1alpha1.UsagePoint{
					Start: metav1.NewTime(b.start),
					End:   metav1.NewTime(b.end),
					Value: formatValue(v),
				})
			}
			if totals == nil {
				s.Total = formatValue(pointSum)
			}
		}
		if totals != nil || !windowed {
			s.Total = formatValue(combine(agg, g.totals))
		}
		out[id] = s
	}
	return out
}

// combine merges the values several customers report for the same group and
// window. It is exact for Sum, Count, Max and Min. Across several billing
// accounts, UniqueCount adds each account's count (an upper bound, as the
// same value may be counted in two accounts), Latest adds each account's
// latest value (the current total of a gauge), and Average averages the
// accounts' averages without weighting them. With one billing account, the
// usual case, every aggregation is exact.
func combine(agg billingv1alpha1.MeterAggregation, values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	switch agg {
	case billingv1alpha1.MeterAggregationMax:
		return slices.Max(values)
	case billingv1alpha1.MeterAggregationMin:
		return slices.Min(values)
	case billingv1alpha1.MeterAggregationAverage:
		var sum float64
		for _, v := range values {
			sum += v
		}
		return sum / float64(len(values))
	default:
		var sum float64
		for _, v := range values {
			sum += v
		}
		return sum
	}
}

// clampPoints trims the first and last bucket to the query's range:
// OpenMeter reports whole windows even when the range starts or ends inside
// one.
func clampPoints(series map[string]*usagev1alpha1.UsageSeries, start, end time.Time) {
	for _, s := range series {
		for i := range s.Points {
			p := &s.Points[i]
			if p.Start.Time.Before(start) {
				p.Start = metav1.NewTime(start)
			}
			if p.End.After(end) {
				p.End = metav1.NewTime(end)
			}
		}
	}
}

// groupSortKey orders series by their dimension values, in groupBy order.
func groupSortKey(values map[string]string, dims []string) string {
	parts := make([]string, len(dims))
	for i, dim := range dims {
		parts[i] = values[dim]
	}
	return strings.Join(parts, "\x00")
}

func formatValue(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
