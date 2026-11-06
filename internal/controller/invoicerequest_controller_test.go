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

const (
	testInvoiceRequest = "acct-preview"
	contractOfferName  = "acme-bare-metal"
)

var (
	invoiceRequestNow = time.Date(2026, time.November, 2, 10, 0, 0, 0, time.UTC)
	contractStart     = time.Date(2026, time.November, 18, 0, 0, 0, 0, time.UTC)
)

// fakeInvoicingClient serves the OpenMeter calls the InvoiceRequest
// controller makes.
type fakeInvoicingClient struct {
	openmeter.Client // unimplemented methods panic if used

	customerErr error
	draft       *om.Invoice
	upcoming    []om.InvoiceLine
	upcomingErr error

	calls int
}

func (f *fakeInvoicingClient) GetCustomer(_ context.Context, key openmeter.CustomerKey) (om.Customer, error) {
	f.calls++
	if f.customerErr != nil {
		return om.Customer{}, f.customerErr
	}
	return om.Customer{Id: "01JCUSTOMER", Key: (*string)(&key)}, nil
}

func (f *fakeInvoicingClient) DraftInvoice(context.Context, openmeter.CustomerID) (*om.Invoice, error) {
	return f.draft, nil
}

func (f *fakeInvoicingClient) UpcomingFixedCharges(context.Context, openmeter.CustomerID) ([]om.InvoiceLine, error) {
	return f.upcoming, f.upcomingErr
}

func flatLine(name, total string, from, to time.Time) om.InvoiceLine {
	return om.InvoiceLine{Id: name, Name: name, Currency: "USD", Period: om.Period{From: from, To: to},
		Totals: om.InvoiceTotals{Total: total, TaxesExclusiveTotal: "0"}}
}

// contractOffer is a bare metal contract: a setup fee, and a monthly fee
// with a 10% discount, plus usage that a first invoice doesn't carry.
func contractOffer() *billingv1alpha1.Offer {
	return &billingv1alpha1.Offer{
		ObjectMeta: metav1.ObjectMeta{Name: contractOfferName},
		Spec: billingv1alpha1.OfferSpec{
			LaunchStage: billingv1alpha1.OfferLaunchStageGA,
			ServicePricings: []billingv1alpha1.ServicePricingSnapshot{
				{Name: "setup-fee", Spec: billingv1alpha1.ServicePricingSpec{
					DisplayName: "Server installation", ChargeType: billingv1alpha1.ChargeTypeOneTime, Amount: "1500.00"}},
				{Name: "monthly-fee", Discount: &billingv1alpha1.Discount{Percentage: 10}, Spec: billingv1alpha1.ServicePricingSpec{
					DisplayName: "Dedicated server", ChargeType: billingv1alpha1.ChargeTypeRecurring, Amount: "1000.00"}},
				{Name: "cpu", Spec: billingv1alpha1.ServicePricingSpec{ChargeType: billingv1alpha1.ChargeTypeUsage}},
			},
		},
	}
}

func contractEntitlementFor(startsAt time.Time) *billingv1alpha1.BillingEntitlement {
	be := testEntitlement(testBEName, contractOfferName)
	be.Spec.Contract = &billingv1alpha1.BillingContract{StartsAt: metav1.NewTime(startsAt)}
	return be
}

func usdAccount() *billingv1alpha1.BillingAccount {
	account := testAccount()
	account.Spec.CurrencyCode = "USD"
	return account
}

func invoiceRequestObj(mode billingv1alpha1.InvoiceRequestMode) *billingv1alpha1.InvoiceRequest {
	return &billingv1alpha1.InvoiceRequest{
		ObjectMeta: metav1.ObjectMeta{Name: testInvoiceRequest, Namespace: testNamespace, Generation: 1},
		Spec: billingv1alpha1.InvoiceRequestSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: testAccountName},
			Mode:              mode,
		},
	}
}

func newInvoiceRequestReconciler(t *testing.T, omc *fakeInvoicingClient, objs ...client.Object) *InvoiceRequestReconciler {
	t.Helper()
	k8s := fake.NewClientBuilder().
		WithScheme(newOfferScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&billingv1alpha1.InvoiceRequest{}).
		WithIndex(&billingv1alpha1.BillingEntitlement{}, EntitlementBillingAccountRefField, entitlementBillingAccountRef).
		Build()
	return &InvoiceRequestReconciler{
		Client:          k8s,
		OpenMeterClient: omc,
		Recorder:        record.NewFakeRecorder(20),
		Now:             func() time.Time { return invoiceRequestNow },
	}
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

func assertCompleted(t *testing.T, ir *billingv1alpha1.InvoiceRequest, phase billingv1alpha1.InvoiceRequestPhase, reason string) *metav1.Condition {
	t.Helper()
	if ir.Status.Phase != phase {
		t.Errorf("phase = %q, want %q", ir.Status.Phase, phase)
	}
	c := apimeta.FindStatusCondition(ir.Status.Conditions, billingv1alpha1.InvoiceRequestConditionCompleted)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reason {
		t.Fatalf("Completed condition = %+v, want True/%s", c, reason)
	}
	if ir.Status.CompletedAt == nil || !ir.Status.CompletedAt.Time.Equal(invoiceRequestNow) {
		t.Errorf("completedAt = %v, want %s", ir.Status.CompletedAt, invoiceRequestNow)
	}
	return c
}

func TestInvoiceRequest_PreviewBeforeTheContractStarts(t *testing.T) {
	omc := &fakeInvoicingClient{}
	r := newInvoiceRequestReconciler(t, omc, usdAccount(), contractOffer(), contractEntitlementFor(contractStart),
		invoiceRequestObj(billingv1alpha1.InvoiceRequestModePreview))

	ir, err := reconcileInvoiceRequest(r)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	c := assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded)
	if c.Message != "The first invoice, sent when the contract starts on 2026-11-18" {
		t.Errorf("message = %q", c.Message)
	}
	if omc.calls != 0 {
		t.Errorf("called OpenMeter %d times before anything is subscribed", omc.calls)
	}

	// The setup fee, and the first month after its 10% discount; not usage.
	st := ir.Status
	if st.Total != "2400.00" || st.CurrencyCode != "USD" || len(st.Lines) != 2 {
		t.Fatalf("status = %+v, want two USD lines totalling 2400.00", st)
	}
	setup, monthly := st.Lines[0], st.Lines[1]
	if setup.Description != "Server installation" || setup.Amount != "1500.00" || setup.Period != nil {
		t.Errorf("setup line = %+v, want 1500.00 with no period", setup)
	}
	if monthly.Description != "Dedicated server" || monthly.Amount != "900.00" || monthly.Period == nil ||
		!monthly.Period.Start.Time.Equal(contractStart) ||
		!monthly.Period.End.Time.Equal(time.Date(2026, time.December, 17, 23, 59, 59, 0, time.UTC)) {
		t.Errorf("monthly line = %+v, want 900.00 for 18 November to 17 December", monthly)
	}
}

// TestInvoiceRequest_PreviewChargesFeeQuantities: a fee charged more than
// once is previewed as its amount times its quantity, before its discount,
// named as the plan names it.
func TestInvoiceRequest_PreviewChargesFeeQuantities(t *testing.T) {
	offer := contractOffer()
	offer.Spec.ServicePricings[1].Quantity = 3
	offer.Spec.ServicePricings[0].Spec.PricingUnit = "server"
	offer.Spec.ServicePricings[0].Quantity = 2

	omc := &fakeInvoicingClient{}
	r := newInvoiceRequestReconciler(t, omc, usdAccount(), offer, contractEntitlementFor(contractStart),
		invoiceRequestObj(billingv1alpha1.InvoiceRequestModePreview))
	ir, err := reconcileInvoiceRequest(r)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// 2 × 1500.00 setup, and 3 × 1000.00 a month less 10%.
	st := ir.Status
	if st.Total != "5700.00" || len(st.Lines) != 2 {
		t.Fatalf("status = %+v, want two lines totalling 5700.00", st)
	}
	if l := st.Lines[0]; l.Description != "Server installation: 2 server × $1500.00" || l.Amount != "3000.00" {
		t.Errorf("setup line = %+v", l)
	}
	if l := st.Lines[1]; l.Description != "Dedicated server × 3" || l.Amount != "2700.00" {
		t.Errorf("monthly line = %+v", l)
	}
}

func TestInvoiceRequest_PreviewAfterTheStart(t *testing.T) {
	started := contractEntitlementFor(time.Date(2026, time.November, 2, 0, 0, 0, 0, time.UTC))
	draftLines := []om.InvoiceLine{
		flatLine("Server installation", "1500", invoiceRequestNow, invoiceRequestNow),
		flatLine("Dedicated server", "899.97", invoiceRequestNow, time.Date(2026, time.December, 2, 0, 0, 0, 0, time.UTC)),
	}

	t.Run("the draft waiting to be issued", func(t *testing.T) {
		omc := &fakeInvoicingClient{
			draft:    &om.Invoice{Id: "01JDRAFT", Currency: "USD", Lines: &draftLines},
			upcoming: []om.InvoiceLine{flatLine("Dedicated server", "900", invoiceRequestNow, invoiceRequestNow.AddDate(0, 1, 0))},
		}
		r := newInvoiceRequestReconciler(t, omc, usdAccount(), contractOffer(), started, invoiceRequestObj(billingv1alpha1.InvoiceRequestModePreview))

		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		c := assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded)
		if ir.Status.Total != "2399.97" || len(ir.Status.Lines) != 2 {
			t.Errorf("status = %+v, want the draft's two lines", ir.Status)
		}
		if c.Message != "Draft invoice 01JDRAFT, which OpenMeter issues and sends when its collection period ends" {
			t.Errorf("message = %q", c.Message)
		}
	})

	t.Run("a draft with nothing to pay gives way to the charges for later periods", func(t *testing.T) {
		usage := []om.InvoiceLine{flatLine("Compute vCPU", "0", invoiceRequestNow, invoiceRequestNow)}
		omc := &fakeInvoicingClient{
			draft: &om.Invoice{Id: "01JUSAGE", Currency: "USD", Lines: &usage},
			upcoming: []om.InvoiceLine{
				flatLine("Dedicated server", "900", time.Date(2026, time.December, 2, 0, 0, 0, 0, time.UTC), time.Date(2027, time.January, 2, 0, 0, 0, 0, time.UTC)),
			},
		}
		r := newInvoiceRequestReconciler(t, omc, usdAccount(), contractOffer(), started, invoiceRequestObj(billingv1alpha1.InvoiceRequestModePreview))

		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		c := assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded)
		if ir.Status.Total != "900" || len(ir.Status.Lines) != 1 || c.Message != "One-time and recurring charges waiting for later billing periods" {
			t.Errorf("status = %+v, message %q; want next month's fee, not the empty draft", ir.Status, c.Message)
		}
	})

	t.Run("the charges for later periods", func(t *testing.T) {
		omc := &fakeInvoicingClient{upcoming: []om.InvoiceLine{
			flatLine("Dedicated server", "900", time.Date(2026, time.December, 2, 0, 0, 0, 0, time.UTC), time.Date(2027, time.January, 2, 0, 0, 0, 0, time.UTC)),
		}}
		r := newInvoiceRequestReconciler(t, omc, usdAccount(), contractOffer(), started, invoiceRequestObj(billingv1alpha1.InvoiceRequestModePreview))

		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded)
		if ir.Status.Total != "900" || len(ir.Status.Lines) != 1 || ir.Status.Lines[0].Description != "Dedicated server" {
			t.Errorf("status = %+v, want next month's fee", ir.Status)
		}
	})

	t.Run("nothing waiting, or no contract", func(t *testing.T) {
		omc := &fakeInvoicingClient{}
		r := newInvoiceRequestReconciler(t, omc, usdAccount(), invoiceRequestObj(billingv1alpha1.InvoiceRequestModePreview))

		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded)
		if ir.Status.Total != "0" || len(ir.Status.Lines) != 0 || ir.Status.CurrencyCode != "USD" {
			t.Errorf("status = %+v, want an empty preview in the account's currency", ir.Status)
		}
	})
}

func TestInvoiceRequest_IssueIsUnsupported(t *testing.T) {
	r := newInvoiceRequestReconciler(t, &fakeInvoicingClient{}, usdAccount(), invoiceRequestObj(billingv1alpha1.InvoiceRequestModeIssue))

	ir, err := reconcileInvoiceRequest(r)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseFailed, billingv1alpha1.InvoiceRequestReasonUnsupported)
}

func TestInvoiceRequest_Failures(t *testing.T) {
	t.Run("no OpenMeter customer", func(t *testing.T) {
		r := newInvoiceRequestReconciler(t, &fakeInvoicingClient{customerErr: openmeter.ErrCustomerNotFound},
			usdAccount(), invoiceRequestObj(billingv1alpha1.InvoiceRequestModePreview))

		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseFailed, billingv1alpha1.InvoiceRequestReasonFailed)
	})

	t.Run("missing billing account", func(t *testing.T) {
		r := newInvoiceRequestReconciler(t, &fakeInvoicingClient{}, invoiceRequestObj(billingv1alpha1.InvoiceRequestModePreview))

		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseFailed, billingv1alpha1.InvoiceRequestReasonFailed)
	})

	t.Run("transient OpenMeter error retries, staying pending", func(t *testing.T) {
		omc := &fakeInvoicingClient{upcomingErr: &openmeter.TransientError{Err: errors.New("503")}}
		r := newInvoiceRequestReconciler(t, omc, usdAccount(), invoiceRequestObj(billingv1alpha1.InvoiceRequestModePreview))

		ir, err := reconcileInvoiceRequest(r)
		if err == nil {
			t.Fatal("Reconcile: want the error returned for a retry")
		}
		if ir.Status.Phase != billingv1alpha1.InvoiceRequestPhasePending {
			t.Errorf("phase = %q, want Pending", ir.Status.Phase)
		}
	})

	t.Run("permanent OpenMeter error fails", func(t *testing.T) {
		omc := &fakeInvoicingClient{upcomingErr: &openmeter.PermanentError{Err: errors.New("400")}}
		r := newInvoiceRequestReconciler(t, omc, usdAccount(), invoiceRequestObj(billingv1alpha1.InvoiceRequestModePreview))

		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseFailed, billingv1alpha1.InvoiceRequestReasonFailed)
	})
}
