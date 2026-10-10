// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	om "github.com/openmeterio/openmeter/api/client/go"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

func TestMapInvoiceStatus(t *testing.T) {
	tests := []struct {
		status      om.InvoiceStatus
		want        billingv1alpha1.InvoicePhase
		wantProject bool
	}{
		{om.InvoiceStatusGathering, "", false},
		{om.InvoiceStatusDraft, "", false},
		{om.InvoiceStatusIssuing, "", false},
		{openmeter.InvoiceStatusDeleting, "", false},
		{om.InvoiceStatusIssued, billingv1alpha1.InvoicePhaseOpen, true},
		{om.InvoiceStatusPaymentProcessing, billingv1alpha1.InvoicePhaseOpen, true},
		{om.InvoiceStatusOverdue, billingv1alpha1.InvoicePhasePastDue, true},
		{om.InvoiceStatusUncollectible, billingv1alpha1.InvoicePhaseVoid, true},
		{om.InvoiceStatusPaid, billingv1alpha1.InvoicePhasePaid, true},
		{om.InvoiceStatusVoided, billingv1alpha1.InvoicePhaseVoid, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			got, project := mapInvoiceStatus(tt.status)
			if got != tt.want || project != tt.wantProject {
				t.Errorf("mapInvoiceStatus(%q) = %q, %v, want %q, %v", tt.status, got, project, tt.want, tt.wantProject)
			}
		})
	}
}

var invoiceNamePattern = regexp.MustCompile(`^acme-account-2026-03-[0-9a-f]{10}$`)

func TestInvoiceName(t *testing.T) {
	march := &om.Period{
		From: time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC),
	}

	t.Run("account, period month, and a stable per-invoice suffix", func(t *testing.T) {
		inv := om.Invoice{Id: "01JABCDEF", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Period: march}
		got := invoiceName("acme-account", inv)
		if !invoiceNamePattern.MatchString(got) {
			t.Errorf("invoiceName() = %q, want acme-account-2026-03-<10 hex>", got)
		}
		if again := invoiceName("acme-account", inv); again != got {
			t.Errorf("invoiceName() not deterministic: %q vs %q", got, again)
		}
	})

	t.Run("several invoices in one month get distinct names", func(t *testing.T) {
		a := invoiceName("acme-account", om.Invoice{Id: "01JAAAA", Period: march})
		b := invoiceName("acme-account", om.Invoice{Id: "01JBBBB", Period: march})
		if a == b {
			t.Errorf("two invoices share the name %q", a)
		}
	})

	t.Run("falls back to createdAt with no period (no line items yet)", func(t *testing.T) {
		inv := om.Invoice{Id: "x", CreatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
		if got := invoiceName("acme-account", inv); !strings.HasPrefix(got, "acme-account-2026-06-") {
			t.Errorf("invoiceName() = %q, want the createdAt month", got)
		}
	})

	t.Run("capped to a valid object name", func(t *testing.T) {
		long := strings.Repeat("a", 240) + "." + strings.Repeat("b", 12)
		for _, account := range []string{long, strings.Repeat("c", 253)} {
			got := invoiceName(account, om.Invoice{Id: "01JABCDEF", Period: march})
			if len(got) > 253 {
				t.Errorf("len(invoiceName()) = %d, want <= 253", len(got))
			}
			if errs := validation.IsDNS1123Subdomain(got); len(errs) > 0 {
				t.Errorf("invoiceName() = %q is not a valid name: %v", got, errs)
			}
		}
	})
}

func TestInvoicePeriod(t *testing.T) {
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name               string
		inv                om.Invoice
		wantStart, wantEnd time.Time
	}{
		{
			name:      "exclusive end becomes the last second of the period",
			inv:       om.Invoice{Period: &om.Period{From: start, To: start.AddDate(0, 1, 0)}},
			wantStart: start,
			wantEnd:   time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC),
		},
		{
			name:      "fractional end",
			inv:       om.Invoice{Period: &om.Period{From: start, To: start.Add(90*time.Second + 500*time.Millisecond)}},
			wantStart: start,
			wantEnd:   start.Add(90 * time.Second),
		},
		{
			name:      "zero-length period keeps end == start",
			inv:       om.Invoice{Period: &om.Period{From: start, To: start}},
			wantStart: start,
			wantEnd:   start,
		},
		{
			name:      "sub-second period keeps end == start",
			inv:       om.Invoice{Period: &om.Period{From: start, To: start.Add(time.Millisecond)}},
			wantStart: start,
			wantEnd:   start,
		},
		{
			name:      "no period uses createdAt for both ends",
			inv:       om.Invoice{CreatedAt: start.Add(1500 * time.Millisecond)},
			wantStart: start.Add(time.Second),
			wantEnd:   start.Add(time.Second),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := invoicePeriod(tt.inv)
			if !got.Start.Time.Equal(tt.wantStart) || !got.End.Time.Equal(tt.wantEnd) {
				t.Errorf("invoicePeriod() = %s..%s, want %s..%s", got.Start.Time, got.End.Time, tt.wantStart, tt.wantEnd)
			}
			if got.Start.After(got.End.Time) {
				t.Errorf("start %s after end %s (violates the CRD's start <= end rule)", got.Start.Time, got.End.Time)
			}
		})
	}
}

func projectedStatus(t *testing.T, inv om.Invoice) billingv1alpha1.InvoiceStatus {
	t.Helper()
	obj := &billingv1alpha1.Invoice{}
	projectInvoiceStatus(obj, baseBillingAccount(), inv, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	return obj.Status
}

func omInvoice(id string, status om.InvoiceStatus, total string) om.Invoice {
	created := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	return om.Invoice{
		Id:        id,
		Status:    status,
		Currency:  "USD",
		CreatedAt: created,
		UpdatedAt: created.Add(time.Hour),
		Period:    &om.Period{From: created, To: created.AddDate(0, 1, 0)},
		Totals:    om.InvoiceTotals{Total: total},
	}
}

func TestProjectInvoiceStatus_Amounts(t *testing.T) {
	tests := []struct {
		status             om.InvoiceStatus
		wantPhase          billingv1alpha1.InvoicePhase
		wantPaid, wantDue  string
		wantPaidAtRecorded bool
	}{
		{om.InvoiceStatusIssued, billingv1alpha1.InvoicePhaseOpen, "0", "12.50", false},
		{om.InvoiceStatusOverdue, billingv1alpha1.InvoicePhasePastDue, "0", "12.50", false},
		{om.InvoiceStatusPaid, billingv1alpha1.InvoicePhasePaid, "12.50", "0", true},
		{om.InvoiceStatusVoided, billingv1alpha1.InvoicePhaseVoid, "0", "0", false},
		{om.InvoiceStatusUncollectible, billingv1alpha1.InvoicePhaseVoid, "0", "12.50", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			got := projectedStatus(t, omInvoice("inv", tt.status, "12.50"))
			if got.Phase != tt.wantPhase || got.AmountPaid != tt.wantPaid || got.AmountDue != tt.wantDue || got.Total != "12.50" {
				t.Errorf("phase/paid/due/total = %s/%s/%s/%s, want %s/%s/%s/12.50",
					got.Phase, got.AmountPaid, got.AmountDue, got.Total, tt.wantPhase, tt.wantPaid, tt.wantDue)
			}
			if (got.PaidAt != nil) != tt.wantPaidAtRecorded {
				t.Errorf("paidAt = %v, want set=%v", got.PaidAt, tt.wantPaidAtRecorded)
			}
		})
	}

	t.Run("deleted invoice closes out as Void with nothing owed", func(t *testing.T) {
		inv := omInvoice("inv", openmeter.InvoiceStatusDeleted, "12.50")
		got := projectedStatus(t, inv)
		if got.Phase != billingv1alpha1.InvoicePhaseVoid || got.AmountDue != "0" || got.AmountPaid != "0" {
			t.Errorf("phase/paid/due = %s/%s/%s, want Void/0/0", got.Phase, got.AmountPaid, got.AmountDue)
		}
	})
}

func TestProjectInvoiceStatus_PaidAtIsRecordedOnce(t *testing.T) {
	obj := &billingv1alpha1.Invoice{}
	account := baseBillingAccount()
	inv := omInvoice("inv", om.InvoiceStatusPaid, "5")
	firstSeen := inv.UpdatedAt
	projectInvoiceStatus(obj, account, inv, time.Now())
	if obj.Status.PaidAt == nil || !obj.Status.PaidAt.Time.Equal(firstSeen) {
		t.Fatalf("paidAt = %v, want %s", obj.Status.PaidAt, firstSeen)
	}

	// A later, unrelated update moves updatedAt; paidAt must not follow.
	inv.UpdatedAt = inv.UpdatedAt.Add(72 * time.Hour)
	projectInvoiceStatus(obj, account, inv, time.Now())
	if !obj.Status.PaidAt.Time.Equal(firstSeen) {
		t.Errorf("paidAt moved to %s, want it kept at %s", obj.Status.PaidAt.Time, firstSeen)
	}
}

func TestProjectInvoiceStatus_DueDate(t *testing.T) {
	obj := &billingv1alpha1.Invoice{}
	account := baseBillingAccount()
	inv := omInvoice("inv", om.InvoiceStatusIssued, "5")
	due := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	inv.DueAt = &due
	projectInvoiceStatus(obj, account, inv, time.Now())
	if obj.Status.DueDate == nil || !obj.Status.DueDate.Time.Equal(due) {
		t.Fatalf("dueDate = %v, want %s", obj.Status.DueDate, due)
	}
	inv.DueAt = nil
	projectInvoiceStatus(obj, account, inv, time.Now())
	if obj.Status.DueDate != nil {
		t.Errorf("dueDate = %v, want cleared once OpenMeter reports none", obj.Status.DueDate)
	}
}

func TestProjectInvoiceStatus_CurrencyMismatch(t *testing.T) {
	inv := omInvoice("inv", om.InvoiceStatusIssued, "5")
	inv.Currency = "EUR"
	got := projectedStatus(t, inv)
	cond := apimeta.FindStatusCondition(got.Conditions, billingv1alpha1.InvoiceConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "CurrencyMismatch" {
		t.Errorf("Ready = %+v, want False/CurrencyMismatch", cond)
	}
}

// invoiceListOpenMeterClient serves ListInvoicesIncludingDeleted from a
// fixed list.
type invoiceListOpenMeterClient struct {
	openmeter.Client
	invoices []om.Invoice
}

func (f *invoiceListOpenMeterClient) ListInvoicesIncludingDeleted(_ context.Context, _ openmeter.CustomerID) ([]om.Invoice, error) {
	return f.invoices, nil
}

func newInvoiceSyncReconciler(t *testing.T, invoices []om.Invoice, objs ...client.Object) (*BillingAccountReconciler, client.Client, *billingv1alpha1.BillingAccount) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := billingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	account := baseBillingAccount()
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(append([]client.Object{account}, objs...)...).
		WithStatusSubresource(&billingv1alpha1.Invoice{}).
		Build()
	r := &BillingAccountReconciler{Client: k8s, OpenMeterClient: &invoiceListOpenMeterClient{invoices: invoices}}
	return r, k8s, account
}

func listMiloInvoices(t *testing.T, k8s client.Client) map[string]billingv1alpha1.Invoice {
	t.Helper()
	var list billingv1alpha1.InvoiceList
	if err := k8s.List(context.Background(), &list); err != nil {
		t.Fatalf("list Invoices: %v", err)
	}
	out := map[string]billingv1alpha1.Invoice{}
	for _, inv := range list.Items {
		out[inv.Annotations[InvoiceKeyAnnotation]] = inv
	}
	return out
}

// TestReconcileInvoices_OneInvoicePerOpenMeterInvoice is the H2
// regression: several OpenMeter invoices in one month (progressive
// billing, a subscription ending mid-period) used to collapse onto one
// `<account>-<YYYY>-<MM>` Invoice showing only the newest, hiding older
// live ones and pairing one invoice's period with another's status.
func TestReconcileInvoices_OneInvoicePerOpenMeterInvoice(t *testing.T) {
	early := omInvoice("01EARLY", om.InvoiceStatusOverdue, "10")
	early.Period = &om.Period{From: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)}
	late := omInvoice("01LATE", om.InvoiceStatusIssued, "20")
	late.CreatedAt = late.CreatedAt.Add(10 * 24 * time.Hour)
	late.Period = &om.Period{From: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)}
	draft := omInvoice("01DRAFT", om.InvoiceStatusDraft, "5")
	gathering := omInvoice("01GATHERING", om.InvoiceStatusGathering, "")
	issuing := omInvoice("01ISSUING", om.InvoiceStatusIssuing, "5")

	r, k8s, account := newInvoiceSyncReconciler(t, []om.Invoice{late, draft, early, gathering, issuing})
	for pass := 1; pass <= 2; pass++ {
		if err := r.reconcileInvoices(context.Background(), account, "cust"); err != nil {
			t.Fatalf("reconcileInvoices pass %d: %v", pass, err)
		}
	}

	got := listMiloInvoices(t, k8s)
	if len(got) != 2 {
		t.Fatalf("Milo Invoices for %v, want exactly the two issued invoices", keysOf(got))
	}
	for _, tc := range []struct {
		inv       om.Invoice
		wantPhase billingv1alpha1.InvoicePhase
		wantEnd   time.Time
	}{
		{early, billingv1alpha1.InvoicePhasePastDue, time.Date(2026, 3, 9, 23, 59, 59, 0, time.UTC)},
		{late, billingv1alpha1.InvoicePhaseOpen, time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC)},
	} {
		obj, ok := got[tc.inv.Id]
		if !ok {
			t.Errorf("no Invoice for %s", tc.inv.Id)
			continue
		}
		if obj.Name != invoiceName(account.Name, tc.inv) {
			t.Errorf("Invoice for %s named %q, want %q", tc.inv.Id, obj.Name, invoiceName(account.Name, tc.inv))
		}
		if obj.Status.Phase != tc.wantPhase || obj.Status.Total != tc.inv.Totals.Total {
			t.Errorf("Invoice for %s: phase %s total %s, want %s %s", tc.inv.Id, obj.Status.Phase, obj.Status.Total, tc.wantPhase, tc.inv.Totals.Total)
		}
		if !obj.Spec.Period.Start.Time.Equal(tc.inv.Period.From) || !obj.Spec.Period.End.Time.Equal(tc.wantEnd) {
			t.Errorf("Invoice for %s: period %s..%s, want its own %s..%s", tc.inv.Id, obj.Spec.Period.Start.Time, obj.Spec.Period.End.Time, tc.inv.Period.From, tc.wantEnd)
		}
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func legacyInvoice(name, openMeterID string, phase billingv1alpha1.InvoicePhase) *billingv1alpha1.Invoice {
	return &billingv1alpha1.Invoice{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "org-a",
			Annotations: map[string]string{InvoiceKeyAnnotation: openMeterID},
		},
		Spec: billingv1alpha1.InvoiceSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: "acme-account"},
			Period: billingv1alpha1.InvoicePeriod{
				Start: metav1.NewTime(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)),
				End:   metav1.NewTime(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)),
			},
		},
		Status: billingv1alpha1.InvoiceStatus{Phase: phase, Total: "7", AmountDue: "7", AmountPaid: "0"},
	}
}

// TestReconcileInvoices_KeepsUpdatingLegacyNamedInvoice: an Invoice created
// under the old `<account>-<YYYY>-<MM>` scheme is matched by its
// annotation and updated in place — neither orphaned nor duplicated.
func TestReconcileInvoices_KeepsUpdatingLegacyNamedInvoice(t *testing.T) {
	paid := omInvoice("01LEGACY", om.InvoiceStatusPaid, "7")
	r, k8s, account := newInvoiceSyncReconciler(t, []om.Invoice{paid},
		legacyInvoice("acme-account-2026-03", "01LEGACY", billingv1alpha1.InvoicePhaseOpen))

	if err := r.reconcileInvoices(context.Background(), account, "cust"); err != nil {
		t.Fatalf("reconcileInvoices: %v", err)
	}
	got := listMiloInvoices(t, k8s)
	if len(got) != 1 {
		t.Fatalf("Invoices = %v, want only the legacy one", keysOf(got))
	}
	obj := got["01LEGACY"]
	if obj.Name != "acme-account-2026-03" || obj.Status.Phase != billingv1alpha1.InvoicePhasePaid || obj.Status.AmountDue != "0" {
		t.Errorf("legacy Invoice = %s phase %s due %s, want acme-account-2026-03 Paid 0", obj.Name, obj.Status.Phase, obj.Status.AmountDue)
	}
}

// TestReconcileInvoices_ClosesOutDeletedInvoice: a projection made from a
// draft (by an earlier version) whose invoice was then deleted becomes
// Void with nothing owed instead of staying Open forever.
func TestReconcileInvoices_ClosesOutDeletedInvoice(t *testing.T) {
	deleted := omInvoice("01DELETED", openmeter.InvoiceStatusDeleted, "7")
	at := time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)
	deleted.DeletedAt = &at
	r, k8s, account := newInvoiceSyncReconciler(t, []om.Invoice{deleted},
		legacyInvoice("acme-account-2026-03", "01DELETED", billingv1alpha1.InvoicePhaseOpen))

	if err := r.reconcileInvoices(context.Background(), account, "cust"); err != nil {
		t.Fatalf("reconcileInvoices: %v", err)
	}
	obj := listMiloInvoices(t, k8s)["01DELETED"]
	if obj.Status.Phase != billingv1alpha1.InvoicePhaseVoid || obj.Status.AmountDue != "0" || obj.Status.AmountPaid != "0" {
		t.Errorf("phase/paid/due = %s/%s/%s, want Void/0/0", obj.Status.Phase, obj.Status.AmountPaid, obj.Status.AmountDue)
	}
}

func TestReconcileInvoices_DeletedInvoiceWithoutProjectionIsNotCreated(t *testing.T) {
	deleted := omInvoice("01DELETED", openmeter.InvoiceStatusDeleted, "7")
	r, k8s, account := newInvoiceSyncReconciler(t, []om.Invoice{deleted})
	if err := r.reconcileInvoices(context.Background(), account, "cust"); err != nil {
		t.Fatalf("reconcileInvoices: %v", err)
	}
	if got := listMiloInvoices(t, k8s); len(got) != 0 {
		t.Errorf("Invoices = %v, want none", keysOf(got))
	}
}

// TestReconcileInvoices_MissingInvoiceIsFlaggedNotVoided: an invoice that is
// not listed even with deleted ones included is not evidence of a void;
// the phase stays and Ready says the projection is stale.
func TestReconcileInvoices_MissingInvoiceIsFlaggedNotVoided(t *testing.T) {
	r, k8s, account := newInvoiceSyncReconciler(t, nil,
		legacyInvoice("acme-account-2026-03", "01GONE", billingv1alpha1.InvoicePhaseOpen))

	if err := r.reconcileInvoices(context.Background(), account, "cust"); err != nil {
		t.Fatalf("reconcileInvoices: %v", err)
	}
	var obj billingv1alpha1.Invoice
	if err := k8s.Get(context.Background(), types.NamespacedName{Name: "acme-account-2026-03", Namespace: "org-a"}, &obj); err != nil {
		t.Fatalf("get: %v", err)
	}
	if obj.Status.Phase != billingv1alpha1.InvoicePhaseOpen {
		t.Errorf("phase = %s, want it left Open", obj.Status.Phase)
	}
	cond := apimeta.FindStatusCondition(obj.Status.Conditions, billingv1alpha1.InvoiceConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != invoiceReasonNotFoundInOpenMeter {
		t.Errorf("Ready = %+v, want False/%s", cond, invoiceReasonNotFoundInOpenMeter)
	}
}

func TestReconcileInvoices_IgnoresOtherAccountsAndUnannotatedInvoices(t *testing.T) {
	other := legacyInvoice("other-account-2026-03", "01OTHER", billingv1alpha1.InvoicePhaseOpen)
	other.Spec.BillingAccountRef.Name = "other-account"
	manual := legacyInvoice("acme-account-manual", "", billingv1alpha1.InvoicePhaseOpen)
	manual.Annotations = nil
	r, k8s, account := newInvoiceSyncReconciler(t, nil, other, manual)

	if err := r.reconcileInvoices(context.Background(), account, "cust"); err != nil {
		t.Fatalf("reconcileInvoices: %v", err)
	}
	for _, name := range []string{"other-account-2026-03", "acme-account-manual"} {
		var obj billingv1alpha1.Invoice
		if err := k8s.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "org-a"}, &obj); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if len(obj.Status.Conditions) != 0 {
			t.Errorf("%s was touched: %+v", name, obj.Status.Conditions)
		}
	}
}
