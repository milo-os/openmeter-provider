// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"github.com/prometheus/client_golang/prometheus"

	"go.miloapis.com/instrumentation/labels"
	"go.miloapis.com/instrumentation/metrics"
)

// invoiceWriterMetricLabels name the billing account: its namespace and
// name.
var invoiceWriterMetricLabels = []string{labels.ResourceNamespace, labels.ResourceName}

// The invoice writer's gauges, per billing account (resource_namespace and
// resource_name), set on every reconcile of an account on the writer. An
// invoice held or stuck means a customer's invoice isn't going out, and
// nothing in OpenMeter times it out, so these are what to alert on: the
// Warning events about them expire.
var (
	invoiceWriterHeldInvoices = metrics.NewGaugeVec(prometheus.GaugeOpts{
		Name: "openmeter_provider_invoice_writer_held_invoices",
		Help: "Invoices the invoice writer won't move on until someone looks at them (see the account's InvoiceWriterHeld events).",
	}, invoiceWriterMetricLabels)
	invoiceWriterStuckInvoices = metrics.NewGaugeVec(prometheus.GaugeOpts{
		Name: "openmeter_provider_invoice_writer_stuck_invoices",
		Help: "Invoices that have waited on a Custom Invoicing hook for over an hour.",
	}, invoiceWriterMetricLabels)
)

// forgetInvoiceWriterMetrics drops an account's gauges, when it's off the
// writer or gone.
func forgetInvoiceWriterMetrics(namespace, name string) {
	invoiceWriterHeldInvoices.DeleteLabelValues(namespace, name)
	invoiceWriterStuckInvoices.DeleteLabelValues(namespace, name)
}
