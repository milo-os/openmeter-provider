// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strings"

	om "github.com/openmeterio/openmeter/api/client/go"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
)

// DesiredMeter is the controller-facing representation of a meter the
// reconciler wants to exist in OpenMeter. Mirrors amberflo.DesiredMeter:
// callers assemble this struct; wire encoding lives here.
type DesiredMeter struct {
	// Slug is the OpenMeter meter identifier (see MeterSlug).
	Slug string
	// EventType is the CloudEvent/ingest type this meter aggregates. It is
	// the full reverse-DNS meterName, so ingestion keys on the canonical
	// meter identifier rather than the slug encoding. Immutable once
	// created — OpenMeter rejects changing it.
	EventType string
	// Aggregation is the OpenMeter aggregation value. Immutable once
	// created — OpenMeter rejects changing it.
	Aggregation om.MeterAggregation
	// Name is the human-readable display name (spec.displayName). Mutable.
	// Always sent on create AND update: OpenMeter defaults an omitted name
	// to the slug, so leaving it out of an update would reset it.
	Name string
	// Description is the human-readable description (from
	// spec.displayName / spec.description). Mutable.
	Description string
	// GroupBy maps dimension keys to their JSONPath in the event data.
	// Mutable.
	GroupBy map[string]string
	// ValueProperty is the JSONPath to the value in the event data (see
	// valueNeedsProperty for which aggregations require it). Immutable once
	// created.
	ValueProperty string
}

// MeterAggregation maps a Milo MeterAggregation onto the OpenMeter wire value.
// OpenMeter supports all seven Milo aggregations, so unlike Amberflo there is
// no unsupported set and no need to skip syncs.
//
//   - Sum         → SUM
//   - Count       → COUNT
//   - UniqueCount → UNIQUE_COUNT
//   - Max / Min   → MAX / MIN
//   - Latest      → LATEST
//   - Average     → AVG
//
// The default case is unreachable through the API — spec.measurement.aggregation
// is CRD-enum-validated to exactly these seven values — so it can only be hit by
// this mapping falling behind a new value added to billingv1alpha1.MeterAggregation.
// That is a code bug, not a data problem: return a PermanentError rather than
// silently defaulting to Sum, which would create a meter with the wrong semantics.
func MeterAggregation(a billingv1alpha1.MeterAggregation) (om.MeterAggregation, error) {
	switch a {
	case billingv1alpha1.MeterAggregationSum:
		return om.MeterAggregationSum, nil
	case billingv1alpha1.MeterAggregationCount:
		return om.MeterAggregationCount, nil
	case billingv1alpha1.MeterAggregationUniqueCount:
		return om.MeterAggregationUniqueCount, nil
	case billingv1alpha1.MeterAggregationMax:
		return om.MeterAggregationMax, nil
	case billingv1alpha1.MeterAggregationMin:
		return om.MeterAggregationMin, nil
	case billingv1alpha1.MeterAggregationLatest:
		return om.MeterAggregationLatest, nil
	case billingv1alpha1.MeterAggregationAverage:
		return om.MeterAggregationAvg, nil
	default:
		return "", &PermanentError{Err: fmt.Errorf("unmapped MeterAggregation %q", a)}
	}
}

const (
	// ValuePropertyValue is the JSONPath of a billing usage event's quantity
	// (billing internal/event.EventData.Value, a string-encoded int64 that
	// OpenMeter parses as a number). Used by every numeric aggregation.
	ValuePropertyValue = "$.value"
	// ValuePropertyResourceUID is the JSONPath of the UID of the Kubernetes
	// object that generated a billing usage event
	// (billing internal/event.EventData.Resource.UID). Used by UNIQUE_COUNT,
	// which counts distinct values of its valueProperty.
	ValuePropertyResourceUID = "$.resource.uid"
)

// MeterValueProperty returns the valueProperty for a Milo aggregation.
//
// UniqueCount counts DISTINCT resources: billing v0.3.10 defines no
// semantics for it beyond the enum value, and its usage event carries only
// a quantity, free-form dimensions, and an optional resource reference.
// Counting distinct quantities ("$.value") would be meaningless, so a
// UniqueCount meter counts the distinct resource.uid values that emitted
// usage in the window (e.g. "active instances"). Emitters of UniqueCount
// meters must therefore set UsageEvent.Resource: for events without one,
// OpenMeter's ClickHouse query (uniqExact over JSON_VALUE) sees an empty
// string, so all of them together add at most one to the count, and
// OpenMeter's event listing flags them as invalid. Every other aggregation
// reads the event quantity; Count ignores the valueProperty entirely (see
// valueNeedsProperty).
func MeterValueProperty(a billingv1alpha1.MeterAggregation) string {
	if a == billingv1alpha1.MeterAggregationUniqueCount {
		return ValuePropertyResourceUID
	}
	return ValuePropertyValue
}

// valueNeedsProperty reports whether an OpenMeter aggregation requires a
// valueProperty. Per OpenMeter's own validation (validateMeterAggregation),
// COUNT is the only aggregation that forbids one — it counts events, full
// stop. Every other aggregation, UNIQUE_COUNT included, requires a
// valueProperty: SUM/MIN/MAX/AVG aggregate the numeric value it points to,
// LATEST returns it, and UNIQUE_COUNT counts distinct values of it.
func valueNeedsProperty(a om.MeterAggregation) bool {
	return a != om.MeterAggregationCount
}

const (
	// meterSlugMaxLen is OpenMeter's slug length limit (api/openapi.yaml,
	// enforced by the server's OpenAPI request validator).
	meterSlugMaxLen = 64
	// meterSlugHashLen is the number of hex characters of the meterName's
	// SHA-256 appended to a slug that had to be truncated, so two long
	// meterNames sharing a 55-character prefix still get distinct slugs.
	meterSlugHashLen = 8
)

// MeterSlug derives the OpenMeter meter slug from the canonical reverse-DNS
// MeterDefinition.meterName. OpenMeter slugs must match
// `^[a-z0-9]+(?:_[a-z0-9]+)*$` and be at most 64 characters. Milo meterNames
// are reverse-DNS paths such as "compute.miloapis.com/instance/cpu-seconds"
// of up to 253 characters.
//
// The transformation is deterministic: the name is lowercased, every run of
// characters outside [a-z0-9] becomes a single '_', and leading/trailing
// underscores are dropped. For every meterName the original
// '.'/'/'/'-' → '_' mapping already turned into a valid slug, this yields
// the exact same slug, so existing meters keep their identity. A result
// longer than 64 characters is truncated and suffixed with
// "_" + the first 8 hex characters of SHA-256(meterName).
//
// It is NOT injective — "a-b" and "a_b" (or "a.b") map to the same slug.
// Two MeterDefinitions colliding that way surface as a PermanentError from
// EnsureMeter (their eventTypes disagree) rather than silently sharing a
// meter, and the MeterDefinition finalizer refuses to delete a meter whose
// eventType is not its own. The original meterName is always carried
// verbatim in the meter's eventType, so event routing never depends on the
// slug encoding.
func MeterSlug(meterName string) string {
	var b strings.Builder
	pendingUnderscore := false
	for _, r := range strings.ToLower(meterName) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pendingUnderscore && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingUnderscore = false
			b.WriteRune(r)
			continue
		}
		pendingUnderscore = true
	}
	slug := b.String()
	if slug != "" && len(slug) <= meterSlugMaxLen {
		return slug
	}

	sum := sha256.Sum256([]byte(meterName))
	suffix := hex.EncodeToString(sum[:])[:meterSlugHashLen]
	if slug == "" {
		// Only reachable for a meterName with no ASCII alphanumerics at all.
		return "meter_" + suffix
	}
	prefix := strings.TrimRight(slug[:meterSlugMaxLen-meterSlugHashLen-1], "_")
	return prefix + "_" + suffix
}

// groupByKeyRegExp mirrors OpenMeter's own group-by key rule
// (openmeter/meter/meter.go groupByKeyRegExp).
var groupByKeyRegExp = regexp.MustCompile(`^[a-zA-Z_][0-9a-zA-Z_]*$`)

// dimensionKeyPrefix is prepended to a group-by key that would otherwise
// start with a digit or shadow one of OpenMeter's reserved query group-bys.
const dimensionKeyPrefix = "dim_"

// reservedGroupByKeys are the group-by names OpenMeter's meter query treats
// specially (openmeter/meter/httphandler/mapping.go): "subject" and
// "customer_id" always resolve to the event's top-level columns, so a
// dimension keyed by either name could never be queried or filtered on.
var reservedGroupByKeys = map[string]struct{}{
	"subject":     {},
	"customer_id": {},
}

// GroupByKey derives the OpenMeter meter group-by key for a Milo dimension
// name. OpenMeter requires group-by keys to match `^[a-zA-Z_][0-9a-zA-Z_]*$`,
// while billing allows any dimension name (its own docs use
// "resource.type").
//
// The mapping is deterministic and the identity for every name that is
// already a valid, non-reserved key, so existing meters and feature filters
// are unaffected. Otherwise every character outside [0-9a-zA-Z_] becomes
// '_', and a name that starts with a digit or equals a reserved query
// group-by ("subject", "customer_id") is prefixed with "dim_". The mapping
// is NOT injective ("a.b" and "a_b" both map to "a_b"); callers building a
// meter must reject two dimensions mapping to the same key — see
// MeterGroupBy.
//
// Every place a dimension becomes an OpenMeter group-by key (the meter's
// groupBy, feature advancedMeterGroupByFilters) MUST go through this
// function, or the keys disagree and OpenMeter rejects the feature.
func GroupByKey(dimension string) string {
	if groupByKeyRegExp.MatchString(dimension) {
		if _, reserved := reservedGroupByKeys[dimension]; !reserved {
			return dimension
		}
		return dimensionKeyPrefix + dimension
	}
	var b strings.Builder
	for _, r := range dimension {
		if r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	key := b.String()
	if key == "" || (key[0] >= '0' && key[0] <= '9') {
		key = dimensionKeyPrefix + key
	}
	return key
}

// quotableDimensionRegExp is the conservative character set for which the
// double-quoted JSONPath member form (`$.dimensions."resource.type"`) is
// known to resolve correctly in both JSONPath engines OpenMeter uses:
//
//   - ClickHouse JSON_VALUE evaluates group-by paths at query time (where
//     the double-quoted member is a quoted identifier), and OpenMeter
//     validates every path through ClickHouse when it creates or updates a
//     meter, so a path ClickHouse cannot parse is rejected loudly with a
//     400 rather than silently mis-grouping usage.
//   - github.com/oliveagle/jsonpath (meter.ParseEvent, used to validate
//     events in OpenMeter's event listing). It does NOT support bracket
//     member access (`['resource.type']` fails to parse), has no escape
//     syntax inside quotes (so '"' cannot appear), parses '[' / ']' as
//     index access even inside quotes, and drops a leading '.'.
var quotableDimensionRegExp = regexp.MustCompile(`^[0-9A-Za-z_\-/:][0-9A-Za-z_.\-/:]*$`)

// DimensionJSONPath returns the JSONPath of a dimension's value inside a
// billing usage event's data ({"value": "...", "dimensions": {...}}).
// Dimensions that are plain identifiers keep the dot form
// ("$.dimensions.region", unchanged for existing meters); anything else
// uses the double-quoted member form ("$.dimensions.\"resource.type\"").
// Returns a PermanentError for a name neither form can express.
func DimensionJSONPath(dimension string) (string, error) {
	if groupByKeyRegExp.MatchString(dimension) {
		return "$.dimensions." + dimension, nil
	}
	if !quotableDimensionRegExp.MatchString(dimension) {
		return "", &PermanentError{Err: fmt.Errorf(
			"dimension %q cannot be expressed as an OpenMeter group-by JSONPath: only letters, digits, '_', '-', '.', '/' and ':' are supported, and it must not start with '.'",
			dimension)}
	}
	return `$.dimensions."` + dimension + `"`, nil
}

// MeterGroupBy builds an OpenMeter meter groupBy map for dimensions:
// GroupByKey(dimension) → DimensionJSONPath(dimension). Repeated dimension
// names are folded together; two DIFFERENT names that map to the same
// group-by key (e.g. "resource.type" and "resource_type") are a
// PermanentError, because OpenMeter could only group by one of them and the
// other's usage would silently be attributed to the wrong dimension.
func MeterGroupBy(dimensions []string) (map[string]string, error) {
	groupBy := make(map[string]string, len(dimensions))
	owner := make(map[string]string, len(dimensions))
	for _, dim := range dimensions {
		if dim == "" {
			return nil, &PermanentError{Err: errors.New("dimension name must not be empty")}
		}
		key := GroupByKey(dim)
		if prev, taken := owner[key]; taken {
			if prev == dim {
				continue
			}
			return nil, &PermanentError{Err: fmt.Errorf(
				"dimensions %q and %q both map to OpenMeter group-by key %q; rename one of them", prev, dim, key)}
		}
		path, err := DimensionJSONPath(dim)
		if err != nil {
			return nil, err
		}
		owner[key] = dim
		groupBy[key] = path
	}
	return groupBy, nil
}

// SystemDimensions returns the dimension keys the billing pipeline injects
// on every valid event of md (status.systemDimensions). Before billing's
// controller has written the status, it falls back to project_name, which
// billing v0.3.10 always injects, so a new meter can segment by project from
// the start.
func SystemDimensions(md *billingv1alpha1.MeterDefinition) []string {
	if len(md.Status.SystemDimensions) > 0 {
		return md.Status.SystemDimensions
	}
	return []string{billingv1alpha1.SystemDimensionProjectName}
}

// MeterDimensions returns every dimension md's OpenMeter meter can be
// grouped and filtered by: its declared dimensions followed by the system
// dimensions (see SystemDimensions). Pass it to MeterGroupBy to build the
// meter, and use it to validate a usage query's group-by.
func MeterDimensions(md *billingv1alpha1.MeterDefinition) []string {
	return append(slices.Clone(md.Spec.Measurement.Dimensions), SystemDimensions(md)...)
}

// EnsureMeter creates or updates the meter so OpenMeter matches desired. The
// call is idempotent: if OpenMeter already agrees, no write happens and only
// the GET is issued. changed reports whether a create or update was issued.
//
// EventType, Aggregation, and ValueProperty cannot be changed once a meter
// exists (OpenMeter has no PUT support for them, matching the immutability
// of MeterDefinition.spec.measurement on the Milo side). If an existing
// meter's immutable fields disagree with desired, that is a PermanentError:
// the caller must ship a new meter under a new slug rather than mutate this
// one.
func (c *client) EnsureMeter(ctx context.Context, desired DesiredMeter) (meter om.Meter, changed bool, err error) {
	if desired.Slug == "" {
		return om.Meter{}, false, &PermanentError{Err: errors.New("DesiredMeter.Slug is required")}
	}
	if desired.EventType == "" {
		return om.Meter{}, false, &PermanentError{Err: errors.New("DesiredMeter.EventType is required")}
	}

	existing, err := c.GetMeter(ctx, desired.Slug)
	switch {
	case errors.Is(err, ErrMeterNotFound):
		created, err := c.createMeter(ctx, desired)
		return created, err == nil, err
	case err != nil:
		return om.Meter{}, false, err
	}

	if err := checkImmutableFields(existing, desired); err != nil {
		return om.Meter{}, false, err
	}
	if !meterNeedsUpdate(existing, desired) {
		return existing, false, nil
	}
	updated, err := c.updateMeter(ctx, existing.Slug, desired)
	return updated, err == nil, err
}

// checkImmutableFields reports a PermanentError when an existing meter's
// eventType, aggregation, or valueProperty disagree with desired. These
// fields cannot be changed via UpdateMeter, and Milo's own immutability
// rules on MeterDefinition.spec.measurement mean this should never happen
// legitimately — surfacing it as a PermanentError makes a drifted/hand-
// edited OpenMeter meter visible rather than silently ignored.
func checkImmutableFields(existing om.Meter, desired DesiredMeter) error {
	if existing.EventType != desired.EventType {
		return &PermanentError{Err: fmt.Errorf(
			"meter %s: eventType %q cannot change to %q", desired.Slug, existing.EventType, desired.EventType)}
	}
	if existing.Aggregation != desired.Aggregation {
		return &PermanentError{Err: fmt.Errorf(
			"meter %s: aggregation %q cannot change to %q", desired.Slug, existing.Aggregation, desired.Aggregation)}
	}
	existingValueProperty := ""
	if existing.ValueProperty != nil {
		existingValueProperty = *existing.ValueProperty
	}
	// Count-based aggregations never carry a valueProperty on the wire (see
	// valueNeedsProperty); normalize desired the same way before comparing,
	// or every reconcile of a Count/UniqueCount meter would spuriously trip
	// this check.
	wantValueProperty := ""
	if valueNeedsProperty(desired.Aggregation) {
		wantValueProperty = desired.ValueProperty
	}
	if existingValueProperty != wantValueProperty {
		return &PermanentError{Err: fmt.Errorf(
			"meter %s: valueProperty %q cannot change to %q", desired.Slug, existingValueProperty, wantValueProperty)}
	}
	return nil
}

// meterNeedsUpdate reports whether existing's mutable fields (name,
// description, groupBy) disagree with desired. Comparison is limited to
// fields UpdateMeter can actually change; slug/eventType/aggregation/
// valueProperty are checked separately by checkImmutableFields. An empty
// desired.Name is not compared: the server fills in the slug for it.
func meterNeedsUpdate(existing om.Meter, desired DesiredMeter) bool {
	if desired.Name != "" && (existing.Name == nil || *existing.Name != desired.Name) {
		return true
	}
	existingDescription := ""
	if existing.Description != nil {
		existingDescription = *existing.Description
	}
	if existingDescription != desired.Description {
		return true
	}
	existingGroupBy := map[string]string{}
	if existing.GroupBy != nil {
		existingGroupBy = *existing.GroupBy
	}
	return !maps.Equal(existingGroupBy, desired.GroupBy)
}

// createMeter POSTs a new meter. valueProperty is only attached for
// aggregations that require one — OpenMeter returns a permanent 400 ("meter
// value property is not allowed when the aggregation is count") if it is set
// for COUNT.
func (c *client) createMeter(ctx context.Context, desired DesiredMeter) (om.Meter, error) {
	body := om.MeterCreate{
		Slug:        desired.Slug,
		EventType:   desired.EventType,
		Aggregation: desired.Aggregation,
	}
	if valueNeedsProperty(desired.Aggregation) {
		body.ValueProperty = &desired.ValueProperty
	}
	if desired.Name != "" {
		body.Name = &desired.Name
	}
	if desired.Description != "" {
		body.Description = &desired.Description
	}
	if len(desired.GroupBy) > 0 {
		body.GroupBy = &desired.GroupBy
	}

	resp, err := c.api.CreateMeterWithResponse(ctx, body)
	if err != nil {
		return om.Meter{}, classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return om.Meter{}, err
	}
	// The real server answers a create with 200 (openmeter/meter/httphandler
	// CreateMeter uses JSONResponseEncoderWithStatus(http.StatusOK)), while
	// the generated SDK only parses a 201 body into JSON201 — so decode the
	// body ourselves for any 2xx.
	if resp.JSON201 != nil {
		return *resp.JSON201, nil
	}
	if m, ok := decodeMeter(resp.Body); ok {
		return m, nil
	}
	// A 2xx with a body that isn't a meter (e.g. empty). The create was
	// accepted, so fetch the meter instead of reporting a failure. Any
	// failure of that follow-up GET is transient: the create itself
	// succeeded, and the next reconcile's GET will find the meter.
	created, err := c.GetMeter(ctx, desired.Slug)
	if err != nil {
		return om.Meter{}, &TransientError{Err: fmt.Errorf(
			"create meter %q: got %s without a meter body, and the follow-up GetMeter failed: %w",
			desired.Slug, resp.Status(), err)}
	}
	return created, nil
}

// updateMeter PUTs the mutable fields of an existing meter. Name is always
// sent when known: OpenMeter resets an omitted name to the slug.
func (c *client) updateMeter(ctx context.Context, slug string, desired DesiredMeter) (om.Meter, error) {
	body := om.MeterUpdate{}
	if desired.Name != "" {
		body.Name = &desired.Name
	}
	if desired.Description != "" {
		body.Description = &desired.Description
	}
	if len(desired.GroupBy) > 0 {
		body.GroupBy = &desired.GroupBy
	}

	resp, err := c.api.UpdateMeterWithResponse(ctx, slug, body)
	if err != nil {
		return om.Meter{}, classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return om.Meter{}, err
	}
	if resp.JSON200 != nil {
		return *resp.JSON200, nil
	}
	if m, ok := decodeMeter(resp.Body); ok {
		return m, nil
	}
	// See createMeter's matching fallback: the update was accepted, so a
	// failing follow-up GET is transient.
	updated, err := c.GetMeter(ctx, slug)
	if err != nil {
		return om.Meter{}, &TransientError{Err: fmt.Errorf(
			"update meter %q: got %s without a meter body, and the follow-up GetMeter failed: %w",
			slug, resp.Status(), err)}
	}
	return updated, nil
}

// decodeMeter parses a meter JSON body. ok is false for an empty body or one
// that does not carry a meter (no slug).
func decodeMeter(body []byte) (om.Meter, bool) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return om.Meter{}, false
	}
	var m om.Meter
	if err := json.Unmarshal(body, &m); err != nil || m.Slug == "" {
		return om.Meter{}, false
	}
	return m, true
}

// isOpenMeterNotFound reports whether a 404 is OpenMeter's own "this
// resource does not exist" answer rather than a generic 404 (a wrong base
// URL or path prefix, an ingress default backend, an unknown route). Only
// the former may be treated as "already gone": treating any 404 as success
// would let a misconfigured serverURL release finalizers and leak meters.
//
// OpenMeter answers a missing resource with an RFC 7807
// application/problem+json body whose detail carries the domain error
// (e.g. "meter not found: <slug>"); its router-level 404 for an unknown
// route is also problem+json but with an empty detail, and a non-OpenMeter
// 404 is not problem+json at all.
func isOpenMeterNotFound(resp *http.Response, body []byte) bool {
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/problem+json" {
		return false
	}
	var problem struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &problem); err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(problem.Detail), "not found")
}

// unrecognizedNotFound is the error for a 404 that is not OpenMeter's own
// not-found answer (see isOpenMeterNotFound).
func unrecognizedNotFound(op, slug string, body []byte) error {
	snippet := strings.TrimSpace(string(body))
	// Whatever answered is not OpenMeter, so its body may be an arbitrary
	// HTML page; keep only enough of it to identify the responder.
	const maxSnippet = 512
	if len(snippet) > maxSnippet {
		snippet = snippet[:maxSnippet] + "...(truncated)"
	}
	return &PermanentError{
		Err: fmt.Errorf(
			"%s meter %q: got a 404 that is not an OpenMeter not-found response; check the OpenMeter serverUrl: %s",
			op, slug, snippet),
		StatusCode:   http.StatusNotFound,
		ResponseBody: snippet,
	}
}

// GetMeter fetches a meter by its slug. Returns ErrMeterNotFound when
// OpenMeter reports that no such meter exists; any other 404 is a
// PermanentError (see isOpenMeterNotFound).
func (c *client) GetMeter(ctx context.Context, slug string) (om.Meter, error) {
	if slug == "" {
		return om.Meter{}, &PermanentError{Err: errors.New("slug is required")}
	}

	resp, err := c.api.GetMeterWithResponse(ctx, slug)
	if err != nil {
		return om.Meter{}, classify(nil, nil, err)
	}
	if resp.StatusCode() == http.StatusNotFound {
		if isOpenMeterNotFound(resp.HTTPResponse, resp.Body) {
			return om.Meter{}, fmt.Errorf("%w: %s", ErrMeterNotFound, slug)
		}
		return om.Meter{}, unrecognizedNotFound("get", slug, resp.Body)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return om.Meter{}, err
	}
	if resp.JSON200 == nil {
		return om.Meter{}, &PermanentError{Err: fmt.Errorf("get meter %q: empty response body", slug)}
	}
	return *resp.JSON200, nil
}

// DeleteMeter removes a meter keyed by its slug. OpenMeter's own not-found
// answer is treated as success — the desired end state is "no meter in
// OpenMeter", and absence satisfies that goal. Any other 404 is a
// PermanentError, so a misconfigured serverURL keeps the finalizer in place
// instead of leaking the meter.
func (c *client) DeleteMeter(ctx context.Context, slug string) error {
	if slug == "" {
		return &PermanentError{Err: errors.New("slug is required")}
	}

	resp, err := c.api.DeleteMeterWithResponse(ctx, slug)
	if err != nil {
		return classify(nil, nil, err)
	}
	if resp.StatusCode() == http.StatusNotFound {
		if isOpenMeterNotFound(resp.HTTPResponse, resp.Body) {
			return nil
		}
		return unrecognizedNotFound("delete", slug, resp.Body)
	}
	return classify(resp.HTTPResponse, resp.Body, nil)
}
