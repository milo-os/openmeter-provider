// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"slices"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
)

func TestMapInvoiceStatus(t *testing.T) {
	tests := []struct {
		status om.InvoiceStatus
		want   billingv1alpha1.InvoicePhase
	}{
		{om.InvoiceStatusDraft, billingv1alpha1.InvoicePhaseOpen},
		{om.InvoiceStatusGathering, billingv1alpha1.InvoicePhaseOpen},
		{om.InvoiceStatusIssuing, billingv1alpha1.InvoicePhaseOpen},
		{om.InvoiceStatusIssued, billingv1alpha1.InvoicePhaseOpen},
		{om.InvoiceStatusPaymentProcessing, billingv1alpha1.InvoicePhaseOpen},
		{om.InvoiceStatusOverdue, billingv1alpha1.InvoicePhasePastDue},
		{om.InvoiceStatusUncollectible, billingv1alpha1.InvoicePhasePastDue},
		{om.InvoiceStatusPaid, billingv1alpha1.InvoicePhasePaid},
		{om.InvoiceStatusVoided, billingv1alpha1.InvoicePhaseVoid},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := mapInvoiceStatus(tt.status); got != tt.want {
				t.Errorf("mapInvoiceStatus(%q) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestInvoiceName(t *testing.T) {
	t.Run("uses period start when present", func(t *testing.T) {
		inv := om.Invoice{
			CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Period: &om.Period{
				From: time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC),
				To:   time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC),
			},
		}
		got := invoiceName("acme-account", inv)
		want := "acme-account-2026-03"
		if got != want {
			t.Errorf("invoiceName() = %q, want %q", got, want)
		}
	})

	t.Run("falls back to createdAt with no period (no line items yet)", func(t *testing.T) {
		inv := om.Invoice{
			CreatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		}
		got := invoiceName("acme-account", inv)
		want := "acme-account-2026-06"
		if got != want {
			t.Errorf("invoiceName() = %q, want %q", got, want)
		}
	})
}

func invoiceAt(id string, created time.Time, status om.InvoiceStatus) om.Invoice {
	return om.Invoice{
		Id:        id,
		CreatedAt: created,
		Status:    status,
		Period:    &om.Period{From: created, To: created.AddDate(0, 1, 0)},
	}
}

// TestPickInvoicePerPeriod is the regression test for a silent data bug:
// Milo Invoice names collapse to <account>-<YYYY>-<MM>, but OpenMeter can
// hold several invoices covering one month (a voided invoice plus its
// reissue, or progressive invoices — the default billing profile ships with
// progressiveBilling enabled). Upserting all of them wrote each onto the
// SAME Invoice object in turn, so the last one processed won; with
// ListInvoices' newest-first ordering that meant the OLDEST silently
// overwrote the newest, leaving e.g. a superseded Void masking a live
// invoice.
func TestPickInvoicePerPeriod(t *testing.T) {
	march := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	marchLater := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	april := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		invoices []om.Invoice
		want     []string
	}{
		{
			name:     "single invoice passes through",
			invoices: []om.Invoice{invoiceAt("a", march, om.InvoiceStatusIssued)},
			want:     []string{"a"},
		},
		{
			name: "a reissue wins over the void it replaced",
			invoices: []om.Invoice{
				invoiceAt("reissued", marchLater, om.InvoiceStatusIssued),
				invoiceAt("voided", march, om.InvoiceStatusVoided),
			},
			want: []string{"reissued"},
		},
		{
			name: "ordering does not decide the winner",
			invoices: []om.Invoice{
				invoiceAt("voided", march, om.InvoiceStatusVoided),
				invoiceAt("reissued", marchLater, om.InvoiceStatusIssued),
			},
			want: []string{"reissued"},
		},
		{
			name: "newest wins among several live progressive invoices",
			invoices: []om.Invoice{
				invoiceAt("first", march, om.InvoiceStatusIssued),
				invoiceAt("second", marchLater, om.InvoiceStatusIssued),
			},
			want: []string{"second"},
		},
		{
			name: "an all-voided month still reports something",
			invoices: []om.Invoice{
				invoiceAt("old-void", march, om.InvoiceStatusVoided),
				invoiceAt("new-void", marchLater, om.InvoiceStatusVoided),
			},
			want: []string{"new-void"},
		},
		{
			name: "distinct periods are all kept",
			invoices: []om.Invoice{
				invoiceAt("march", march, om.InvoiceStatusIssued),
				invoiceAt("april", april, om.InvoiceStatusIssued),
			},
			want: []string{"march", "april"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pickInvoicePerPeriod("acct", tt.invoices)
			gotIDs := make([]string, 0, len(got))
			for _, inv := range got {
				gotIDs = append(gotIDs, inv.Id)
			}
			if len(gotIDs) != len(tt.want) {
				t.Fatalf("picked %v, want %v", gotIDs, tt.want)
			}
			// Order is by Invoice name, so compare as a set of the ids we
			// expect to survive rather than depending on period ordering.
			for _, want := range tt.want {
				if !slices.Contains(gotIDs, want) {
					t.Errorf("picked %v, want it to include %q", gotIDs, want)
				}
			}
		})
	}
}

// TestPickInvoicePerPeriod_IsStableAcrossListOrder guards against the
// Invoice object flip-flopping between reconciles: whatever order
// ListInvoices returns, the same invoice must win every time.
func TestPickInvoicePerPeriod_IsStableAcrossListOrder(t *testing.T) {
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	// Same period, same CreatedAt, both live — only the id can break the tie.
	a := invoiceAt("aaa", at, om.InvoiceStatusIssued)
	b := invoiceAt("bbb", at, om.InvoiceStatusIssued)

	forward := pickInvoicePerPeriod("acct", []om.Invoice{a, b})
	reverse := pickInvoicePerPeriod("acct", []om.Invoice{b, a})
	if len(forward) != 1 || len(reverse) != 1 {
		t.Fatalf("expected exactly one invoice per period, got %d and %d", len(forward), len(reverse))
	}
	if forward[0].Id != reverse[0].Id {
		t.Errorf("winner depends on list order: %q vs %q", forward[0].Id, reverse[0].Id)
	}
}
