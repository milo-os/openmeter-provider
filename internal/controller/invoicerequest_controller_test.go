/*
Copyright 2026 Datum Technology Inc.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, version 3.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.
*/

package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

const testInvoiceRequest = "acct-first-invoice"

var invoiceRequestNow = time.Date(2026, time.March, 18, 10, 0, 0, 0, time.UTC)

// fakeInvoicingClient serves the OpenMeter calls the InvoiceRequest
// controller makes.
type fakeInvoicingClient struct {
	openmeter.Client // unimplemented methods panic if used

	customerErr error
	charges     []openmeter.FixedCharge
	chargesErr  error
	invoices    []om.Invoice // ListInvoices, newest first
	invoiceErr  error

	invoicedLines [][]string
	previewed     [][]openmeter.FixedCharge
}

func (f *fakeInvoicingClient) GetCustomer(_ context.Context, key openmeter.CustomerKey) (om.Customer, error) {
	if f.customerErr != nil {
		return om.Customer{}, f.customerErr
	}
	return om.Customer{Id: "01JCUSTOMER", Key: (*string)(&key)}, nil
}

func (f *fakeInvoicingClient) PendingFixedCharges(context.Context, openmeter.CustomerID, time.Time) ([]openmeter.FixedCharge, error) {
	return f.charges, f.chargesErr
}

func (f *fakeInvoicingClient) PreviewInvoice(_ context.Context, _ openmeter.CustomerID, charges []openmeter.FixedCharge) (om.Invoice, error) {
	f.previewed = append(f.previewed, charges)
	return testInvoice("", invoiceRequestNow), nil
}

func (f *fakeInvoicingClient) InvoiceFixedCharges(_ context.Context, _ openmeter.CustomerID, lineIDs []string, _ time.Time) ([]om.Invoice, error) {
	f.invoicedLines = append(f.invoicedLines, lineIDs)
	if f.invoiceErr != nil {
		return nil, f.invoiceErr
	}
	return []om.Invoice{{Id: "01JINVOICE"}}, nil
}

func (f *fakeInvoicingClient) GetInvoiceWithLines(_ context.Context, id string) (om.Invoice, error) {
	return testInvoice(id, invoiceRequestNow), nil
}

func (f *fakeInvoicingClient) ListInvoices(context.Context, openmeter.CustomerID) ([]om.Invoice, error) {
	return f.invoices, nil
}

// testInvoice is a contract's first invoice: a setup fee and a first month
// with a 10% discount, no tax.
func testInvoice(id string, createdAt time.Time) om.Invoice {
	from := time.Date(2026, time.March, 18, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	lines := []om.InvoiceLine{
		{Id: "l1", Name: "Server installation", Period: om.Period{From: from, To: from},
			Totals: om.InvoiceTotals{Total: "1500.00", TaxesExclusiveTotal: "0"}},
		{Id: "l2", Name: "Dedicated server", Period: om.Period{From: from, To: to},
			Totals: om.InvoiceTotals{Total: "900.00", TaxesExclusiveTotal: "0"}},
	}
	return om.Invoice{
		Id:        id,
		Status:    om.InvoiceStatusDraft,
		Currency:  "USD",
		CreatedAt: createdAt,
		Period:    &om.Period{From: from, To: to},
		Lines:     &lines,
		Totals:    om.InvoiceTotals{Total: "2400.00", TaxesExclusiveTotal: "0"},
	}
}

func testCharges() []openmeter.FixedCharge {
	return []openmeter.FixedCharge{{LineID: "line-setup"}, {LineID: "line-monthly"}}
}

func testInvoiceRequestObj(mode billingv1alpha1.InvoiceRequestMode) *billingv1alpha1.InvoiceRequest {
	return &billingv1alpha1.InvoiceRequest{
		ObjectMeta: metav1.ObjectMeta{Name: testInvoiceRequest, Namespace: testNamespace, Generation: 1},
		Spec: billingv1alpha1.InvoiceRequestSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: testAccountName},
			Mode:              mode,
		},
	}
}

func newInvoiceRequestReconciler(t *testing.T, omc *fakeInvoicingClient, objs ...client.Object) (*InvoiceRequestReconciler, *record.FakeRecorder) {
	t.Helper()
	k8s := fake.NewClientBuilder().
		WithScheme(newOfferScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&billingv1alpha1.InvoiceRequest{}).
		Build()
	rec := record.NewFakeRecorder(20)
	return &InvoiceRequestReconciler{
		Client:          k8s,
		OpenMeterClient: omc,
		Recorder:        rec,
		Now:             func() time.Time { return invoiceRequestNow },
	}, rec
}

func reconcileInvoiceRequest(r *InvoiceRequestReconciler) (*billingv1alpha1.InvoiceRequest, error) {
	key := types.NamespacedName{Namespace: testNamespace, Name: testInvoiceRequest}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	var ir billingv1alpha1.InvoiceRequest
	if getErr := r.Get(context.Background(), key, &ir); getErr != nil {
		return nil, getErr
	}
	return &ir, err
}

func assertCompleted(t *testing.T, ir *billingv1alpha1.InvoiceRequest, phase billingv1alpha1.InvoiceRequestPhase, reason string) {
	t.Helper()
	if ir.Status.Phase != phase {
		t.Errorf("phase = %q, want %q", ir.Status.Phase, phase)
	}
	c := apimeta.FindStatusCondition(ir.Status.Conditions, billingv1alpha1.InvoiceRequestConditionCompleted)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reason {
		t.Errorf("Completed condition = %+v, want True/%s", c, reason)
	}
	if ir.Status.CompletedAt == nil || !ir.Status.CompletedAt.Time.Equal(invoiceRequestNow) {
		t.Errorf("completedAt = %v, want %s", ir.Status.CompletedAt, invoiceRequestNow)
	}
}

func assertFirstInvoiceLines(t *testing.T, ir *billingv1alpha1.InvoiceRequest) {
	t.Helper()
	st := ir.Status
	if st.CurrencyCode != "USD" || st.Total != "2400.00" || len(st.Lines) != 2 {
		t.Fatalf("status = %+v, want two USD lines totalling 2400.00", st)
	}
	setup, monthly := st.Lines[0], st.Lines[1]
	if setup.Description != "Server installation" || setup.Amount != "1500.00" || setup.Period != nil {
		t.Errorf("setup line = %+v, want a one-time charge without a period", setup)
	}
	wantEnd := time.Date(2026, time.April, 17, 23, 59, 59, 0, time.UTC)
	if monthly.Amount != "900.00" || monthly.Period == nil || !monthly.Period.End.Time.Equal(wantEnd) {
		t.Errorf("monthly line = %+v, want 900.00 for the month to %s", monthly, wantEnd)
	}
}

func TestInvoiceRequest_Preview(t *testing.T) {
	t.Run("prices the fixed charges without invoicing", func(t *testing.T) {
		omc := &fakeInvoicingClient{charges: testCharges()}
		r, _ := newInvoiceRequestReconciler(t, omc, testAccount(), testInvoiceRequestObj(billingv1alpha1.InvoiceRequestModePreview))

		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded)
		assertFirstInvoiceLines(t, ir)
		if ir.Status.InvoiceRef != nil {
			t.Errorf("invoiceRef = %+v, want none for a preview", ir.Status.InvoiceRef)
		}
		if len(omc.previewed) != 1 || len(omc.invoicedLines) != 0 {
			t.Errorf("previewed %d times, invoiced %v; want one preview and no invoice", len(omc.previewed), omc.invoicedLines)
		}
	})

	t.Run("nothing waiting is an empty preview", func(t *testing.T) {
		omc := &fakeInvoicingClient{}
		account := testAccount()
		account.Spec.CurrencyCode = "USD"
		r, _ := newInvoiceRequestReconciler(t, omc, account, testInvoiceRequestObj(billingv1alpha1.InvoiceRequestModePreview))

		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded)
		if ir.Status.Total != "0" || len(ir.Status.Lines) != 0 || ir.Status.CurrencyCode != "USD" {
			t.Errorf("status = %+v, want no lines, a zero total in the account's currency", ir.Status)
		}
		if len(omc.previewed) != 0 {
			t.Error("simulated an invoice with no charges")
		}
	})
}

func TestInvoiceRequest_Issue(t *testing.T) {
	t.Run("invoices the fixed charges and names the Invoice", func(t *testing.T) {
		omc := &fakeInvoicingClient{charges: testCharges()}
		r, rec := newInvoiceRequestReconciler(t, omc, testAccount(), testInvoiceRequestObj(billingv1alpha1.InvoiceRequestModeIssue))

		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded)
		assertFirstInvoiceLines(t, ir)
		if want := [][]string{{"line-setup", "line-monthly"}}; !slices.EqualFunc(omc.invoicedLines, want, slices.Equal[[]string]) {
			t.Errorf("invoiced lines = %v, want %v", omc.invoicedLines, want)
		}
		wantName := invoiceName(testAccountName, testInvoice("01JINVOICE", invoiceRequestNow))
		if ir.Status.InvoiceRef == nil || ir.Status.InvoiceRef.Name != wantName {
			t.Errorf("invoiceRef = %+v, want %s", ir.Status.InvoiceRef, wantName)
		}
		if got := ir.Annotations[IssueAttemptedAtAnnotation]; got != "2026-03-18T10:00:00Z" {
			t.Errorf("attempt annotation = %q, want the attempt time", got)
		}
		if events := drainRecorder(rec); !containsEvent(events, EventReasonInvoiceIssued) {
			t.Errorf("events = %v, want %s", events, EventReasonInvoiceIssued)
		}

		// Done: a later reconcile doesn't invoice again.
		if _, err := reconcileInvoiceRequest(r); err != nil {
			t.Fatal(err)
		}
		if len(omc.invoicedLines) != 1 {
			t.Errorf("invoiced again after completing: %v", omc.invoicedLines)
		}
	})

	t.Run("nothing waiting", func(t *testing.T) {
		omc := &fakeInvoicingClient{}
		r, _ := newInvoiceRequestReconciler(t, omc, testAccount(), testInvoiceRequestObj(billingv1alpha1.InvoiceRequestModeIssue))

		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseFailed, billingv1alpha1.InvoiceRequestReasonNothingToInvoice)
		if len(omc.invoicedLines) != 0 {
			t.Errorf("invoiced %v with no charges", omc.invoicedLines)
		}
	})

	t.Run("charges invoiced elsewhere in the meantime", func(t *testing.T) {
		omc := &fakeInvoicingClient{charges: testCharges(), invoiceErr: openmeter.ErrNothingToInvoice}
		r, _ := newInvoiceRequestReconciler(t, omc, testAccount(), testInvoiceRequestObj(billingv1alpha1.InvoiceRequestModeIssue))

		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseFailed, billingv1alpha1.InvoiceRequestReasonNothingToInvoice)
	})

	t.Run("recovers the invoice an interrupted attempt created", func(t *testing.T) {
		attempt := invoiceRequestNow.Add(-5 * time.Minute)
		ir := testInvoiceRequestObj(billingv1alpha1.InvoiceRequestModeIssue)
		ir.Annotations = map[string]string{IssueAttemptedAtAnnotation: attempt.Format(time.RFC3339)}
		ir.Status.Phase = billingv1alpha1.InvoiceRequestPhasePending
		omc := &fakeInvoicingClient{invoices: []om.Invoice{
			{Id: "01JGATHERING", Status: om.InvoiceStatusGathering, CreatedAt: invoiceRequestNow},
			{Id: "01JINVOICE", Status: om.InvoiceStatusDraft, CreatedAt: attempt.Add(2 * time.Second)},
			{Id: "01JOLDER", Status: om.InvoiceStatusPaid, CreatedAt: attempt.AddDate(0, -1, 0)},
		}}
		r, _ := newInvoiceRequestReconciler(t, omc, testAccount(), ir)

		got, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		assertCompleted(t, got, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded)
		if wantName := invoiceName(testAccountName, testInvoice("01JINVOICE", invoiceRequestNow)); got.Status.InvoiceRef == nil || got.Status.InvoiceRef.Name != wantName {
			t.Errorf("invoiceRef = %+v, want the recovered invoice %s", got.Status.InvoiceRef, wantName)
		}
		if len(omc.invoicedLines) != 0 {
			t.Errorf("invoiced again: %v", omc.invoicedLines)
		}
	})
}

func TestInvoiceRequest_Failures(t *testing.T) {
	t.Run("no OpenMeter customer", func(t *testing.T) {
		omc := &fakeInvoicingClient{customerErr: openmeter.ErrCustomerNotFound}
		r, _ := newInvoiceRequestReconciler(t, omc, testAccount(), testInvoiceRequestObj(billingv1alpha1.InvoiceRequestModePreview))

		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseFailed, billingv1alpha1.InvoiceRequestReasonFailed)
	})

	t.Run("missing billing account", func(t *testing.T) {
		r, _ := newInvoiceRequestReconciler(t, &fakeInvoicingClient{}, testInvoiceRequestObj(billingv1alpha1.InvoiceRequestModePreview))

		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseFailed, billingv1alpha1.InvoiceRequestReasonFailed)
	})

	t.Run("transient OpenMeter error retries, staying pending", func(t *testing.T) {
		omc := &fakeInvoicingClient{chargesErr: &openmeter.TransientError{Err: errors.New("503")}}
		r, _ := newInvoiceRequestReconciler(t, omc, testAccount(), testInvoiceRequestObj(billingv1alpha1.InvoiceRequestModeIssue))

		ir, err := reconcileInvoiceRequest(r)
		if err == nil {
			t.Fatal("Reconcile: want the error returned for a retry")
		}
		if ir.Status.Phase != billingv1alpha1.InvoiceRequestPhasePending {
			t.Errorf("phase = %q, want Pending", ir.Status.Phase)
		}
	})

	t.Run("permanent OpenMeter error fails", func(t *testing.T) {
		omc := &fakeInvoicingClient{chargesErr: &openmeter.PermanentError{Err: errors.New("400")}}
		r, _ := newInvoiceRequestReconciler(t, omc, testAccount(), testInvoiceRequestObj(billingv1alpha1.InvoiceRequestModeIssue))

		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseFailed, billingv1alpha1.InvoiceRequestReasonFailed)
	})
}
