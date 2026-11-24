// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
)

const testChargeDraft = "acme-charges"

// chargeDraftObj is a draft of two lines: a custom item with its own
// dates, discount and no tax, and a catalog item left to its defaults.
func chargeDraftObj() *billingv1alpha1.ChargeDraft {
	untaxed := false
	return &billingv1alpha1.ChargeDraft{
		ObjectMeta: metav1.ObjectMeta{Name: testChargeDraft, Namespace: testNamespace},
		Spec: billingv1alpha1.ChargeDraftSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: testAccountName},
			Invoice:           billingv1alpha1.ChargeInvoiceNow,
			Lines: []billingv1alpha1.ChargeDraftLine{
				{
					Description: "Remote hands", Quantity: "3", UnitAmount: "150.00", DiscountPercent: "12.5",
					Tax: &billingv1alpha1.PricingTax{Taxable: &untaxed}, Note: "Reseller, exempt",
					ServicePeriod: &billingv1alpha1.ServicePeriod{Start: "2026-10-05", End: "2026-10-09"},
				},
				{ServicePricingRef: &billingv1alpha1.ChargeServicePricingRef{Name: "cross-connect-install-v1"}, Quantity: "1"},
			},
		},
	}
}

func installPricing() *billingv1alpha1.ServicePricing {
	return &billingv1alpha1.ServicePricing{
		ObjectMeta: metav1.ObjectMeta{Name: "cross-connect-install-v1", Namespace: billingv1alpha1.DefaultServicePricingNamespace},
		Spec: billingv1alpha1.ServicePricingSpec{
			ChargeType: billingv1alpha1.ChargeTypeOneTime, Currency: "USD", DisplayName: "Cross-connect install",
			Amount: "300", Tax: &billingv1alpha1.PricingTax{Code: "txcd_20030000"},
		},
	}
}

func chargeDraftRequest(draft bool) *billingv1alpha1.InvoiceRequest {
	ir := stripePreviewRequest(draft)
	ir.Spec.ChargeDraftRef = &billingv1alpha1.ChargeDraftReference{Name: testChargeDraft}
	return ir
}

func TestInvoiceRequest_PreviewsAChargeDraftThroughStripe(t *testing.T) {
	r := newStripePreviewReconciler(t, usdAccount(), readyStripeCustomer(), installPricing(), chargeDraftObj(), chargeDraftRequest(true))
	r.CustomChargePricing = "custom-charge"

	if _, err := reconcileInvoiceRequest(r); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	inv := getStripeInvoice(t, r)
	if inv.Spec.Purpose != stripev1alpha1.StripeInvoicePurposePreviewDraft {
		t.Errorf("purpose = %s, want PreviewDraft", inv.Spec.Purpose)
	}
	if len(inv.Spec.Lines) != 2 {
		t.Fatalf("lines = %+v", inv.Spec.Lines)
	}
	custom, catalog := inv.Spec.Lines[0], inv.Spec.Lines[1]
	if custom.ServicePricingRef.Name != "custom-charge" || custom.Description != "Remote hands" ||
		custom.Quantity != "3" || custom.UnitAmount != "150" || custom.Amount != "450.00" ||
		custom.DiscountPercent != "12.5" || custom.Note != "Reseller, exempt" {
		t.Errorf("custom line = %+v", custom)
	}
	if custom.Tax == nil || custom.Tax.Taxable == nil || *custom.Tax.Taxable || custom.Tax.Code != "" {
		t.Errorf("custom line tax = %+v, want not taxable", custom.Tax)
	}
	if got := custom.Period.Start.UTC(); !got.Equal(time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("custom line starts %s, want its own dates", got)
	}
	if catalog.ServicePricingRef.Name != "cross-connect-install-v1" || catalog.Description != "Cross-connect install" ||
		catalog.UnitAmount != "300" || catalog.Amount != "300.00" || catalog.Tax == nil || catalog.Tax.Code != "txcd_20030000" {
		t.Errorf("catalog line = %+v, want the pricing's name, price and tax", catalog)
	}
	// Left out, the dates are the current billing period, as billing fills
	// them in: the account bills on the 1st, and it's 2 November.
	if s, e := catalog.Period.Start.UTC(), catalog.Period.End.UTC(); !s.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) ||
		e.Format(time.DateOnly) != "2026-11-30" {
		t.Errorf("catalog line period = %s to %s, want November", s, e)
	}

	inv.Status = stripev1alpha1.StripeInvoiceStatus{
		InvoiceID: "in_1", DashboardURL: "https://dashboard.stripe.com/test/invoices/in_1",
		Lines: []stripev1alpha1.StripeInvoiceLineStatus{
			{Key: "charge-0", Amount: "450.00", Discount: "56.25", Tax: "0.00"},
			{Key: "charge-1", Amount: "300.00", Discount: "0.00", Tax: "24.00"},
		},
		Subtotal: "750.00", Discount: "56.25", Tax: "24.00", Total: "717.75", StartingBalance: "0.00", AmountDue: "717.75",
	}
	apimeta.SetStatusCondition(&inv.Status.Conditions, metav1.Condition{
		Type: stripev1alpha1.StripeInvoiceConditionReady, Status: metav1.ConditionTrue, Reason: "Previewed", ObservedGeneration: inv.Generation,
	})
	if err := r.Status().Update(context.Background(), inv); err != nil {
		t.Fatal(err)
	}
	ir, err := reconcileInvoiceRequest(r)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	c := assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded)
	if !strings.Contains(c.Message, "the draft's 2 lines") {
		t.Errorf("message = %q", c.Message)
	}
	if ir.Status.Total != "693.75" || ir.Status.Tax != "24.00" || ir.Status.TotalWithTax != "717.75" {
		t.Errorf("totals = %s + %s = %s", ir.Status.Total, ir.Status.Tax, ir.Status.TotalWithTax)
	}
	if len(ir.Status.Lines) != 2 || ir.Status.Lines[0].Amount != "393.75" || ir.Status.Lines[1].Tax != "24.00" {
		t.Errorf("lines = %+v", ir.Status.Lines)
	}
	if ir.Status.Draft == nil || ir.Status.Draft.URL != "https://dashboard.stripe.com/test/invoices/in_1" {
		t.Errorf("draft = %+v", ir.Status.Draft)
	}
}

func TestInvoiceRequest_PreviewsAChargeDraftWithoutStripe(t *testing.T) {
	r := newStripePreviewReconciler(t, usdAccount(), installPricing(), chargeDraftObj(), chargeDraftRequest(false))
	r.CustomChargePricing = "custom-charge"
	ir, err := reconcileInvoiceRequest(r)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	c := assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded)
	if !strings.Contains(c.Message, "Tax isn't included") {
		t.Errorf("message = %q", c.Message)
	}
	// 3 × 150.00 less 12.5%, and the install at its list price.
	if ir.Status.Total != "693.75" || len(ir.Status.Lines) != 2 || ir.Status.Lines[0].Amount != "393.75" {
		t.Errorf("total %s, lines %+v", ir.Status.Total, ir.Status.Lines)
	}
}

func TestInvoiceRequest_ChargeDraftFailures(t *testing.T) {
	otherAccount := chargeDraftObj()
	otherAccount.Spec.BillingAccountRef.Name = "someone-else"
	for _, tc := range []struct {
		name    string
		draft   *billingv1alpha1.ChargeDraft
		custom  string
		wantMsg string
	}{
		{"no custom charge pricing", chargeDraftObj(), "", "no custom charge pricing is configured"},
		{"another account's draft", otherAccount, "custom-charge", `is for billing account "someone-else"`},
		{"a missing draft", nil, "custom-charge", "not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{usdAccount(), installPricing(), chargeDraftRequest(false)}
			if tc.draft != nil {
				objs = append(objs, tc.draft)
			}
			r := newStripePreviewReconciler(t, objs...)
			r.CustomChargePricing = tc.custom
			ir, err := reconcileInvoiceRequest(r)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			c := assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseFailed, billingv1alpha1.InvoiceRequestReasonFailed)
			if !strings.Contains(c.Message, tc.wantMsg) {
				t.Errorf("message = %q, want %q", c.Message, tc.wantMsg)
			}
		})
	}
}

func TestChargeDraftDefaultPeriod(t *testing.T) {
	onDay := func(day int32) *billingv1alpha1.BillingAccount {
		a := usdAccount()
		a.Spec.PaymentTerms = &billingv1alpha1.PaymentTerms{InvoiceDayOfMonth: day}
		return a
	}
	for _, tc := range []struct {
		name       string
		account    *billingv1alpha1.BillingAccount
		now        time.Time
		start, end string
	}{
		{"mid-month, billed on the 1st", onDay(1), time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC), "2026-11-01", "2026-11-30"},
		{"on the billing day", onDay(1), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), "2026-11-01", "2026-11-30"},
		{"before the billing day", onDay(15), time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC), "2026-10-15", "2026-11-14"},
		{"a short month", onDay(31), time.Date(2027, 2, 15, 0, 0, 0, 0, time.UTC), "2027-01-31", "2027-02-27"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := chargeDraftDefaultPeriod(tc.account, tc.now)
			if got.Start != tc.start || got.End != tc.end {
				t.Errorf("period = %s to %s, want %s to %s", got.Start, got.End, tc.start, tc.end)
			}
		})
	}
}
