// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	ctrl "sigs.k8s.io/controller-runtime"
)

// reconcileCount reads the sample count of one controller/result series of
// openmeter_provider_reconcile_duration_seconds.
func reconcileCount(t *testing.T, controller, result string) uint64 {
	t.Helper()
	var m dto.Metric
	if err := reconcileDuration.WithLabelValues(controller, result).(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("read histogram: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

func TestReconcileResult(t *testing.T) {
	tests := []struct {
		name string
		res  ctrl.Result
		err  error
		want string
	}{
		{name: "success", want: "success"},
		{name: "requeue after", res: ctrl.Result{RequeueAfter: time.Minute}, want: "requeue"},
		{name: "error wins over requeue", res: ctrl.Result{RequeueAfter: time.Minute}, err: errors.New("boom"), want: "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reconcileResult(tt.res, tt.err); got != tt.want {
				t.Errorf("reconcileResult() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestObserveReconcile_RecordsNamedResults(t *testing.T) {
	const controller = "test-named-results"
	before := reconcileCount(t, controller, "error")

	reconcile := func() (res ctrl.Result, err error) {
		defer observeReconcile(controller, time.Now(), &res, &err)
		return ctrl.Result{}, errors.New("boom")
	}
	_, _ = reconcile()

	if got := reconcileCount(t, controller, "error") - before; got != 1 {
		t.Errorf("error samples += %d, want 1", got)
	}
}

func TestObserveReconcile_RecordsPanicAsError(t *testing.T) {
	const controller = "test-panic"
	beforeErr := reconcileCount(t, controller, "error")
	beforeOK := reconcileCount(t, controller, "success")

	reconcile := func() (res ctrl.Result, err error) {
		defer observeReconcile(controller, time.Now(), &res, &err)
		panic("boom")
	}

	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Errorf("recovered %v, want the original panic to be re-raised", r)
			}
		}()
		_, _ = reconcile()
	}()

	if got := reconcileCount(t, controller, "error") - beforeErr; got != 1 {
		t.Errorf("error samples += %d, want 1", got)
	}
	if got := reconcileCount(t, controller, "success") - beforeOK; got != 0 {
		t.Errorf("success samples += %d, want 0", got)
	}
}
