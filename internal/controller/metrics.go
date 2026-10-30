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
// controller. result is one of: success, error, requeue. It must be deferred
// directly (defer observeReconcile(..., &res, &err)) so that recover() sees a
// panicking reconcile: the panic is recorded as an error and re-raised for
// controller-runtime to handle.
func observeReconcile(controller string, start time.Time, res *ctrl.Result, err *error) {
	result := reconcileResult(*res, *err)
	r := recover()
	if r != nil {
		result = "error"
	}
	reconcileDuration.
		WithLabelValues(controller, result).
		Observe(time.Since(start).Seconds())
	if r != nil {
		panic(r)
	}
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
