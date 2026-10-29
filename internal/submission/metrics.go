// SPDX-License-Identifier: AGPL-3.0-only

package submission

import (
	"github.com/prometheus/client_golang/prometheus"

	"go.miloapis.com/instrumentation/metrics"
)

// Prometheus instruments for the submission consumer.
//
// Registered against controller-runtime's default registry (the same one
// /metrics is served from) via go.miloapis.com/instrumentation/metrics,
// so operators see billing_* series alongside controller_runtime_* without
// extra wiring.
//
// Metric names match amberflo-provider's submission consumer exactly
// (not openmeter_provider_*): these represent the same conceptual pipeline
// stage — validated usage CloudEvent in, ingest API call out — regardless
// of which provider is doing the ingesting, so existing dashboards/alerts
// built against these names keep working if a deployment ever swaps
// providers.

var (
	// submissionRequestsTotal counts OpenMeter ingest API submission
	// attempts by outcome (see the outcome* constants in submission.go).
	// Labels:
	//   status="success"      — 2xx response, event acked.
	//   status="transient"    — 5xx/429/network, event nacked for retry.
	//   status="permanent"    — event malformed, missing its subject, or
	//                           rejected by OpenMeter as invalid (400/422);
	//                           acked and dropped.
	//   status="config_error" — OpenMeter refused the request for a reason
	//                           that is not the event's (401/403/404/...,
	//                           i.e. a bad secret or URL); nacked for retry.
	//                           Any sustained rate needs operator action.
	//   status="max_deliveries_exceeded" — JetStream stopped redelivering a
	//                           message after maxDeliver attempts; its usage
	//                           was NOT submitted and must be replayed.
	submissionRequestsTotal = metrics.NewCounterVec(
		prometheus.CounterOpts{
			Name: "billing_submission_requests_total",
			Help: "Total OpenMeter ingest API submission attempts, by outcome.",
		},
		[]string{"status"},
	)

	// pipelineOldestUnsubmittedSeconds tracks the age of the oldest
	// fetched-but-unacked event in the current submission batch.
	// Resets to 0 when no messages are in-flight (after each batch).
	// A sustained non-zero value indicates the consumer is stalled.
	pipelineOldestUnsubmittedSeconds = metrics.NewGauge(
		prometheus.GaugeOpts{
			Name: "billing_pipeline_oldest_unsubmitted_seconds",
			Help: "Age in seconds of the oldest fetched-but-unacked event in the current submission batch. Resets to 0 when no messages are in flight.",
		},
	)
)

// recordSubmission increments the submission counter with the given status
// label (one of the outcome* constants).
func recordSubmission(status string) {
	submissionRequestsTotal.WithLabelValues(status).Inc()
}

// setOldestUnsubmittedAge sets the age gauge to the given number of seconds.
// Pass 0 after a batch drains to signal no in-flight messages.
func setOldestUnsubmittedAge(seconds float64) {
	pipelineOldestUnsubmittedSeconds.Set(seconds)
}
