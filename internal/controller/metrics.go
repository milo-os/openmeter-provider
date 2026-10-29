// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrl "sigs.k8s.io/controller-runtime"

	"go.miloapis.com/instrumentation/metrics"
)

// Reconcile-scoped Prometheus metrics. Registered automatically on
// controller-runtime's default registry by go.miloapis.com/instrumentation/metrics.

var (
	reconcileDuration = metrics.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "openmeter_provider_reconcile_duration_seconds",
			Help:    "End-to-end reconcile duration for openmeter-provider controllers.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"controller", "result"},
	)
)

// observeReconcile records a single reconcile outcome against a named
// controller. result is one of: success, error, requeue.
func observeReconcile(controller string, start time.Time, res ctrl.Result, err error) {
	result := reconcileResult(res, err)
	reconcileDuration.
		WithLabelValues(controller, result).
		Observe(time.Since(start).Seconds())
}

// reconcileResult derives the metric label from a Reconcile return pair.
func reconcileResult(res ctrl.Result, err error) string {
	switch {
	case err != nil:
		return "error"
	case res.RequeueAfter > 0:
		return "requeue"
	default:
		return "success"
	}
}
