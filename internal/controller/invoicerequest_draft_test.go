// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
)

const draftOfferName = "contract-acct-draft"

// draftOffer is a contract the staff portal saved but hasn't published: a
// Draft Offer with servicePricingRefs and no snapshot, labelled for acct.
func draftOffer() *billingv1alpha1.Offer {
	return &billingv1alpha1.Offer{
		ObjectMeta: metav1.ObjectMeta{Name: draftOfferName, Labels: map[string]string{
			billingv1alpha1.ContractBillingAccountLabel: testAccountName,
			billingv1alpha1.ContractOrganizationLabel:   testNamespace,
		}},
		Spec: billingv1alpha1.OfferSpec{
			LaunchStage: billingv1alpha1.OfferLaunchStageDraft,
			Discount:    &billingv1alpha1.Discount{Percentage: 20},
			ServicePricingRefs: []billingv1alpha1.ServicePricingRef{
				{Name: "draft-setup"},
				{Name: "draft-rack", Quantity: 2, Discount: &billingv1alpha1.Discount{Percentage: 10}},
				{Name: "draft-support", BillingTiming: billingv1alpha1.BillingTimingInArrears},
			},
		},
	}
}

func draftPricing(name, display string, charge billingv1alpha1.ChargeType, amount string) *billingv1alpha1.ServicePricing {
	return &billingv1alpha1.ServicePricing{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: billingv1alpha1.DefaultServicePricingNamespace},
		Spec: billingv1alpha1.ServicePricingSpec{
			DisplayName: display, ChargeType: charge, Amount: amount, Currency: "USD",
		},
	}
}

func draftObjects() []client.Object {
	return []client.Object{
		usdAccount(), draftOffer(),
		draftPricing("draft-setup", "Setup fee", billingv1alpha1.ChargeTypeOneTime, "500.00"),
		draftPricing("draft-rack", "Rack", billingv1alpha1.ChargeTypeRecurring, "200.00"),
		draftPricing("draft-support", "Support", billingv1alpha1.ChargeTypeRecurring, "100.00"),
	}
}

func draftPreviewRequest(billingDay billingv1alpha1.ContractBillingDay) *billingv1alpha1.InvoiceRequest {
	ir := invoiceRequestObj(billingv1alpha1.InvoiceRequestModePreview)
	ir.CreationTimestamp = metav1.NewTime(invoiceRequestNow)
	ir.Spec.ContractDraft = &billingv1alpha1.InvoiceRequestContractDraft{
		OfferRef:   billingv1alpha1.OfferReference{Name: draftOfferName},
		StartsAt:   metav1.NewTime(contractStart),
		BillingDay: billingDay,
	}
	return ir
}

func TestInvoiceRequest_PreviewsAContractDraft(t *testing.T) {
	// The account is on no contract: the draft is previewed all the same.
	r := newInvoiceRequestReconciler(t, &fakeInvoicingClient{},
		append(draftObjects(), draftPreviewRequest(""))...)

	ir, err := reconcileInvoiceRequest(r)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	c := assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded)
	if !strings.Contains(c.Message, "The draft contract's first invoice, sent when it starts on 2026-11-18 if it's published") {
		t.Errorf("message = %q", c.Message)
	}
	// Setup at the Offer's 20% off, the rack at its own 10% off for two, and
	// support, billed in arrears, not until its first month ends.
	if len(ir.Status.Lines) != 2 {
		t.Fatalf("lines = %+v", ir.Status.Lines)
	}
	setup, rack := ir.Status.Lines[0], ir.Status.Lines[1]
	if setup.Description != "Setup fee" || setup.Amount != "400.00" || setup.Period != nil {
		t.Errorf("setup = %+v", setup)
	}
	if rack.Description != "Rack × 2" || rack.Amount != "360.00" || rack.Period == nil ||
		!rack.Period.Start.Time.Equal(contractStart) {
		t.Errorf("rack = %+v", rack)
	}
	if ir.Status.Total != "760.00" {
		t.Errorf("total = %s", ir.Status.Total)
	}
}

func TestInvoiceRequest_PreviewsAContractDraftOnTheInvoiceDay(t *testing.T) {
	r := newInvoiceRequestReconciler(t, &fakeInvoicingClient{},
		append(draftObjects(), draftPreviewRequest(billingv1alpha1.ContractBillingDayInvoiceDay))...)

	ir, err := reconcileInvoiceRequest(r)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, billingv1alpha1.InvoiceRequestReasonSucceeded)
	rack := ir.Status.Lines[1]
	wantEnd := time.Date(2026, time.December, 1, 0, 0, 0, 0, time.UTC).Add(-time.Second)
	if rack.Period == nil || !rack.Period.End.Time.Equal(wantEnd) {
		t.Fatalf("rack period = %+v, want to end before the 1st", rack.Period)
	}
	if rack.Amount == "360.00" {
		t.Errorf("rack = %s, want it prorated to the 1st", rack.Amount)
	}
}

func TestInvoiceRequest_PreviewsAContractDraftThroughStripe(t *testing.T) {
	r := newStripePreviewReconciler(t, append(draftObjects(), readyStripeCustomer(), draftPreviewRequest(""))...)

	if _, err := reconcileInvoiceRequest(r); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	inv := getStripeInvoice(t, r)
	if len(inv.Spec.Lines) != 2 || inv.Spec.Lines[0].ServicePricingRef.Name != "draft-setup" ||
		inv.Spec.Lines[1].ServicePricingRef.Name != "draft-rack" || inv.Spec.Lines[1].DiscountPercent != "10" {
		t.Errorf("Stripe lines = %+v", inv.Spec.Lines)
	}
}

func TestInvoiceRequest_RefusesAContractDraftItCantPreview(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func([]client.Object) []client.Object
		says string
	}{
		{"another account's", func(objs []client.Object) []client.Object {
			objs[1].(*billingv1alpha1.Offer).Labels[billingv1alpha1.ContractBillingAccountLabel] = "someone-else"
			return objs
		}, `Offer "contract-acct-draft" isn't a contract for billing account "acct"`},
		{"a pricing gone", func(objs []client.Object) []client.Object {
			return objs[:len(objs)-1]
		}, `ServicePricing "draft-support" of Offer "contract-acct-draft" not found`},
		{"no Offer", func(objs []client.Object) []client.Object {
			return append(objs[:1:1], objs[2:]...)
		}, `Offer "contract-acct-draft" not found`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newInvoiceRequestReconciler(t, &fakeInvoicingClient{},
				append(tc.edit(draftObjects()), draftPreviewRequest(""))...)
			ir, err := reconcileInvoiceRequest(r)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			c := assertCompleted(t, ir, billingv1alpha1.InvoiceRequestPhaseFailed, billingv1alpha1.InvoiceRequestReasonFailed)
			if !strings.Contains(c.Message, tc.says) {
				t.Errorf("message = %q, want %q", c.Message, tc.says)
			}
		})
	}
}
