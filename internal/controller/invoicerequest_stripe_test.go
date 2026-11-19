// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
)

// newStripePreviewReconciler is newInvoiceRequestReconciler where
// stripe-provider serves StripeInvoice.
func newStripePreviewReconciler(t *testing.T, objs ...client.Object) *InvoiceRequestReconciler {
	t.Helper()
	s := newOfferScheme(t)
	if err := stripev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add stripe-provider scheme: %v", err)
	}
	k8s := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&billingv1alpha1.InvoiceRequest{}, &stripev1alpha1.StripeInvoice{}, &stripev1alpha1.StripeCustomer{}).
		WithIndex(&billingv1alpha1.BillingEntitlement{}, EntitlementBillingAccountRefField, entitlementBillingAccountRef).
		Build()
	return &InvoiceRequestReconciler{
		Client:          k8s,
		OpenMeterClient: &fakeInvoicingClient{},
		Recorder:        record.NewFakeRecorder(20),
		Now:             func() time.Time { return invoiceRequestNow },
		StripeInvoices:  true,
	}
}

func readyStripeCustomer() *stripev1alpha1.StripeCustomer {
	return &stripev1alpha1.StripeCustomer{
		ObjectMeta: metav1.ObjectMeta{Name: testAccountName, Namespace: testNamespace},
		Status:     stripev1alpha1.StripeCustomerStatus{StripeCustomerID: "cus_acme"},
	}
}

func stripePreviewRequest(draft bool) *billingv1alpha1.InvoiceRequest {
	ir := invoiceRequestObj(billingv1alpha1.InvoiceRequestModePreview)
	ir.Spec.Draft = draft
	ir.CreationTimestamp = metav1.NewTime(invoiceRequestNow)
	return ir
}

func getStripeInvoice(t *testing.T, r *InvoiceRequestReconciler) *stripev1alpha1.StripeInvoice {
	t.Helper()
	var inv stripev1alpha1.StripeInvoice
	key := types.NamespacedName{Namespace: testNamespace, Name: testInvoiceRequest + "-stripe"}
	if err := r.Get(context.Background(), key, &inv); err != nil {
		t.Fatalf("get StripeInvoice: %v", err)
	}
	return &inv
}

// markStripeReady is what stripe-provider does once Stripe has worked the
// preview out: the setup fee untaxed, the monthly fee with 10% off and tax.
func markStripeReady(t *testing.T, r *InvoiceRequestReconciler, dashboardURL string) {
	t.Helper()
	inv := getStripeInvoice(t, r)
	inv.Status = stripev1alpha1.StripeInvoiceStatus{
		InvoiceID:    "in_1",
		DashboardURL: dashboardURL,
		Lines: []stripev1alpha1.StripeInvoiceLineStatus{
			{Key: "charge-0", Amount: "1500.00", Discount: "0.00", Tax: "0.00"},
			{Key: "charge-1", Amount: "1000.00", Discount: "100.00", Tax: "72.00"},
		},
		Subtotal: "2500.00", Discount: "100.00", Tax: "72.00", Total: "2472.00",
		StartingBalance: "0.00", AmountDue: "2472.00",
	}
	apimeta.SetStatusCondition(&inv.Status.Conditions, metav1.Condition{
		Type: stripev1alpha1.StripeInvoiceConditionReady, Status: metav1.ConditionTrue, Reason: "Previewed",
		ObservedGeneration: inv.Generation,
	})
	apimeta.SetStatusCondition(&inv.Status.Conditions, metav1.Condition{
		Type: stripev1alpha1.StripeInvoiceConditionAmountsMatch, Status: metav1.ConditionTrue, Reason: "Match",
	})
	if err := r.Status().Update(context.Background(), inv); err != nil {
		t.Fatalf("update StripeInvoice status: %v", err)
	}
}

func TestInvoiceRequest_PreviewThroughStripe(t *testing.T) {
	r := newStripePreviewReconciler(t, usdAccount(), contractEntitlementFor(contractStart), contractOffer(),
		readyStripeCustomer(), stripePreviewRequest(false))

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testInvoiceRequest}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("should check back while Stripe works it out")
	}

	inv := getStripeInvoice(t, r)
	if inv.Spec.Purpose != stripev1alpha1.StripeInvoicePurposePreview || inv.Spec.BillingAccountRef.Name != testAccountName {
		t.Errorf("spec = %+v", inv.Spec)
	}
	if owner := metav1.GetControllerOf(inv); owner == nil || owner.Kind != "InvoiceRequest" {
		t.Errorf("owner = %+v, want the request", owner)
	}
	if len(inv.Spec.Lines) != 2 {
		t.Fatalf("lines = %+v", inv.Spec.Lines)
	}
	setup, monthly := inv.Spec.Lines[0], inv.Spec.Lines[1]
	if setup.ServicePricingRef.Name != "setup-fee" || setup.Description != "Server installation" ||
		setup.Quantity != "1" || setup.UnitAmount != "1500.00" || setup.Amount != "1500.00" ||
		!setup.Period.Start.Equal(&setup.Period.End) {
		t.Errorf("setup line = %+v", setup)
	}
	if monthly.ServicePricingRef.Name != "monthly-fee" || monthly.DiscountPercent != "10" ||
		monthly.Amount != "1000.00" || !monthly.Period.Start.Time.Equal(contractStart) {
		t.Errorf("monthly line = %+v", monthly)
	}

	ir, err := reconcileInvoiceRequest(r)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if ir.Status.Phase != billingv1alpha1.InvoiceRequestPhasePending {
		t.Fatalf("phase = %s, want Pending until Stripe answers", ir.Status.Phase)
	}

	markStripeReady(t, r, "")
	ir, err = reconcileInvoiceRequest(r)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	c := assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded)
	if !strings.Contains(c.Message, "Worked out by Stripe, tax included") {
		t.Errorf("message = %q", c.Message)
	}
	if ir.Status.Total != "2400.00" || ir.Status.Tax != "72.00" || ir.Status.TotalWithTax != "2472.00" {
		t.Errorf("totals = %s / %s / %s", ir.Status.Total, ir.Status.Tax, ir.Status.TotalWithTax)
	}
	got := ir.Status.Lines[1]
	if got.Description != "Dedicated server" || got.Amount != "900.00" || got.Discount != "100.00" ||
		got.Tax != "72.00" || got.Quantity != "1" || got.UnitAmount != "1000.00" || got.Period == nil {
		t.Errorf("monthly = %+v", got)
	}
	if ir.Status.Draft != nil {
		t.Errorf("draft = %+v, want none", ir.Status.Draft)
	}
	if ir.Status.StartingBalance != "0.00" || ir.Status.AmountDue != "2472.00" || strings.Contains(c.Message, "owes") {
		t.Errorf("balance = %s, due = %s, message = %q", ir.Status.StartingBalance, ir.Status.AmountDue, c.Message)
	}
}

func TestInvoiceRequest_PreviewThroughStripeCarriesTheBalance(t *testing.T) {
	for _, tc := range []struct {
		balance, due, says string
	}{
		{"0.23", "2472.23", "The account already owes 0.23, which Stripe adds to this invoice"},
		{"-100.00", "2372.00", "The account has 100.00 in credit, which Stripe takes off this invoice"},
	} {
		t.Run(tc.balance, func(t *testing.T) {
			r := newStripePreviewReconciler(t, usdAccount(), contractEntitlementFor(contractStart), contractOffer(),
				readyStripeCustomer(), stripePreviewRequest(false))
			if _, err := reconcileInvoiceRequest(r); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			markStripeReady(t, r, "")
			inv := getStripeInvoice(t, r)
			inv.Status.StartingBalance, inv.Status.AmountDue = tc.balance, tc.due
			if err := r.Status().Update(context.Background(), inv); err != nil {
				t.Fatal(err)
			}
			ir, err := reconcileInvoiceRequest(r)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			c := assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded)
			if ir.Status.StartingBalance != tc.balance || ir.Status.AmountDue != tc.due || ir.Status.TotalWithTax != "2472.00" {
				t.Errorf("balance = %s, due = %s, total with tax = %s", ir.Status.StartingBalance, ir.Status.AmountDue, ir.Status.TotalWithTax)
			}
			if !strings.Contains(c.Message, tc.says) {
				t.Errorf("message = %q", c.Message)
			}
		})
	}
}

func TestInvoiceRequest_PreviewThroughStripeLeavesADraft(t *testing.T) {
	r := newStripePreviewReconciler(t, usdAccount(), contractEntitlementFor(contractStart), contractOffer(),
		readyStripeCustomer(), stripePreviewRequest(true))
	if _, err := reconcileInvoiceRequest(r); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if p := getStripeInvoice(t, r).Spec.Purpose; p != stripev1alpha1.StripeInvoicePurposePreviewDraft {
		t.Fatalf("purpose = %s", p)
	}
	markStripeReady(t, r, "https://dashboard.stripe.com/test/invoices/in_1")
	ir, err := reconcileInvoiceRequest(r)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if ir.Status.Draft == nil || ir.Status.Draft.URL != "https://dashboard.stripe.com/test/invoices/in_1" {
		t.Errorf("draft = %+v", ir.Status.Draft)
	}
}

func TestInvoiceRequest_PreviewFallsBackWithoutStripe(t *testing.T) {
	t.Run("no Stripe customer", func(t *testing.T) {
		r := newStripePreviewReconciler(t, usdAccount(), contractEntitlementFor(contractStart), contractOffer(),
			stripePreviewRequest(true))
		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		c := assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded)
		if !strings.Contains(c.Message, "Tax isn't included: the account has no Stripe customer yet, so there's no draft to open") {
			t.Errorf("message = %q", c.Message)
		}
		if ir.Status.Total != "2400.00" || ir.Status.Tax != "" || ir.Status.Draft != nil {
			t.Errorf("status = %+v", ir.Status)
		}
	})

	t.Run("account not billed in USD", func(t *testing.T) {
		account := usdAccount()
		account.Spec.CurrencyCode = "EUR"
		r := newStripePreviewReconciler(t, account, contractEntitlementFor(contractStart), contractOffer(),
			readyStripeCustomer(), stripePreviewRequest(false))
		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		c := assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded)
		if !strings.Contains(c.Message, "Tax isn't included: Stripe previews are only in USD and the account is billed in EUR") {
			t.Errorf("message = %q", c.Message)
		}
		if ir.Status.CurrencyCode != "EUR" || ir.Status.Total != "2400.00" || ir.Status.Tax != "" {
			t.Errorf("status = %+v", ir.Status)
		}
		var invoices stripev1alpha1.StripeInvoiceList
		if err := r.List(context.Background(), &invoices); err != nil {
			t.Fatal(err)
		}
		if len(invoices.Items) != 0 {
			t.Errorf("made %d StripeInvoices, want none", len(invoices.Items))
		}
	})

	t.Run("Stripe fails", func(t *testing.T) {
		r := newStripePreviewReconciler(t, usdAccount(), contractEntitlementFor(contractStart), contractOffer(),
			readyStripeCustomer(), stripePreviewRequest(false))
		if _, err := reconcileInvoiceRequest(r); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		inv := getStripeInvoice(t, r)
		apimeta.SetStatusCondition(&inv.Status.Conditions, metav1.Condition{
			Type: stripev1alpha1.StripeInvoiceConditionReady, Status: metav1.ConditionFalse,
			Reason: "StripeError", Message: "No such customer",
		})
		if err := r.Status().Update(context.Background(), inv); err != nil {
			t.Fatal(err)
		}
		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		c := assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded)
		if !strings.Contains(c.Message, "Stripe couldn't work it out: No such customer") || ir.Status.Total != "2400.00" {
			t.Errorf("message = %q, total = %s", c.Message, ir.Status.Total)
		}
	})

	t.Run("Stripe takes too long", func(t *testing.T) {
		r := newStripePreviewReconciler(t, usdAccount(), contractEntitlementFor(contractStart), contractOffer(),
			readyStripeCustomer(), stripePreviewRequest(false))
		if _, err := reconcileInvoiceRequest(r); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		inv := getStripeInvoice(t, r)
		apimeta.SetStatusCondition(&inv.Status.Conditions, metav1.Condition{
			Type: stripev1alpha1.StripeInvoiceConditionReady, Status: metav1.ConditionFalse,
			Reason: "WaitingForProduct", Message: "StripeProduct pricing-monthly-fee isn't ready",
		})
		if err := r.Status().Update(context.Background(), inv); err != nil {
			t.Fatal(err)
		}
		r.Now = func() time.Time { return invoiceRequestNow.Add(stripePreviewWait) }
		ir, err := reconcileInvoiceRequest(r)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		c := apimeta.FindStatusCondition(ir.Status.Conditions, billingv1alpha1.InvoiceRequestConditionCompleted)
		if ir.Status.Phase != billingv1alpha1.InvoiceRequestPhaseSucceeded || c == nil || !strings.Contains(c.Message, "isn't ready") {
			t.Errorf("phase = %s, completed = %+v", ir.Status.Phase, c)
		}
	})
}
