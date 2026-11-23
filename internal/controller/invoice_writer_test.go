// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
	"github.com/openmeterio/openmeter/pkg/models"
	"github.com/shopspring/decimal"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
)

// fakeWriterOpenMeter is OpenMeter as the invoice writer sees it.
type fakeWriterOpenMeter struct {
	openmeter.Client
	invoices []om.Invoice
	draft    map[string][]openmeter.LineExternalID
	draftIDs map[string]string
	issued   map[string]string
	payments map[string][]openmeter.PaymentTrigger
	deleted  []string
}

func (f *fakeWriterOpenMeter) ListInvoicesAwaitingSync(context.Context, openmeter.CustomerID) ([]om.Invoice, error) {
	return f.invoices, nil
}

func (f *fakeWriterOpenMeter) DraftSynchronized(_ context.Context, id, externalID string, lines []openmeter.LineExternalID) error {
	f.draft[id], f.draftIDs[id] = lines, externalID
	return nil
}

func (f *fakeWriterOpenMeter) IssuingSynchronized(_ context.Context, id, number, _ string, _ *time.Time) error {
	f.issued[id] = number
	return nil
}

func (f *fakeWriterOpenMeter) UpdatePaymentStatus(_ context.Context, id string, trigger openmeter.PaymentTrigger) error {
	f.payments[id] = append(f.payments[id], trigger)
	return nil
}

func (f *fakeWriterOpenMeter) DeleteInvoice(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

const writerApp = "app_ci"

var writerPeriod = om.Period{
	From: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	To:   time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC),
}

func strp(s string) *string { return &s }

// writerInvoice is an OpenMeter invoice on the writer's app: a rack fee
// and its usage, 10% off, and a custom charge staff added.
func writerInvoice(status string) om.Invoice {
	var apps om.BillingProfileAppsOrReference
	_ = apps.FromBillingProfileAppReferences(om.BillingProfileAppReferences{
		Invoicing: om.AppReference{Id: writerApp}, Payment: om.AppReference{Id: writerApp}, Tax: om.AppReference{Id: writerApp},
	})
	discounts := &om.BillingDiscounts{Percentage: &om.BillingDiscountPercentage{Percentage: models.NewPercentage(10)}}
	rack := om.InvoiceLine{
		Id: "01LINERACK", Name: "Rack", Status: om.InvoiceLineStatusValid, Period: writerPeriod,
		Description: strp("ServicePricing rack-v1 (compute.datumapis.com)"),
		RateCard:    &om.InvoiceUsageBasedRateCard{Discounts: discounts},
		TaxConfig:   &om.TaxConfig{Stripe: &om.StripeTaxConfig{Code: "txcd_10000000"}},
		Totals:      om.InvoiceTotals{Amount: "330"},
		Children: &[]om.InvoiceDetailedLine{
			{Id: "01DETAILFEE", Name: "Rack × 1", Period: writerPeriod, Quantity: strp("1"), PerUnitAmount: strp("300"), Totals: om.InvoiceTotals{Amount: "300"}},
			{Id: "01DETAILTIER", Name: "Rack power: tier 2", Period: writerPeriod, Quantity: strp("3"), PerUnitAmount: strp("10"), Totals: om.InvoiceTotals{Amount: "30"}},
			{Id: "01DETAILZERO", Name: "Rack power: tier 1", Period: writerPeriod, Quantity: strp("0"), PerUnitAmount: strp("0"), Totals: om.InvoiceTotals{Amount: "0"}},
		},
	}
	charge := om.InvoiceLine{
		Id: "01LINECHARGE", Name: "Remote hands", Status: om.InvoiceLineStatusValid, Period: writerPeriod,
		Metadata: &om.Metadata{openmeter.ChargeMetadataKey: "org-a/remote-hands", openmeter.ChargeUIDMetadataKey: "charge-uid"},
		Totals:   om.InvoiceTotals{Amount: "100"},
		Children: &[]om.InvoiceDetailedLine{{Id: "01DETAILCHARGE", Totals: om.InvoiceTotals{Amount: "100"}}},
	}
	return om.Invoice{
		Id:            "01INVOICE",
		Currency:      "USD",
		Status:        om.InvoiceStatus(strings.Split(status, ".")[0]),
		StatusDetails: om.InvoiceStatusDetails{ExtendedStatus: status},
		Workflow:      om.InvoiceWorkflowSettings{Apps: &apps},
		Lines:         &[]om.InvoiceLine{rack, charge},
		Totals:        om.InvoiceTotals{Amount: "430", DiscountsTotal: "33"},
		UpdatedAt:     time.Now(),
	}
}

type writerHarness struct {
	r   *BillingAccountReconciler
	k8s client.Client
	omc *fakeWriterOpenMeter
	rec *record.FakeRecorder
	acc *billingv1alpha1.BillingAccount
}

func newWriterHarness(t *testing.T) *writerHarness {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := billingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := stripev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	account := &billingv1alpha1.BillingAccount{ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "org-a", UID: "account-uid"}}
	charge := &billingv1alpha1.Charge{
		ObjectMeta: metav1.ObjectMeta{Name: "remote-hands", Namespace: "org-a", UID: "charge-uid"},
		Spec: billingv1alpha1.ChargeSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: "acme"},
			Description:       "Remote hands, January", Quantity: "2", UnitAmount: "50.00", Note: "Ticket 42",
		},
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(account, charge).
		WithStatusSubresource(&billingv1alpha1.BillingAccount{}, &stripev1alpha1.StripeInvoice{}).
		Build()
	omc := &fakeWriterOpenMeter{
		draft: map[string][]openmeter.LineExternalID{}, draftIDs: map[string]string{},
		issued: map[string]string{}, payments: map[string][]openmeter.PaymentTrigger{},
	}
	rec := record.NewFakeRecorder(100)
	r := &BillingAccountReconciler{
		Client: k8s, OpenMeterClient: omc, Recorder: rec,
		InvoiceWriter: &InvoiceWriter{AppName: "Datum invoice writer", CustomChargePricing: "custom-charge", appID: writerApp},
	}
	return &writerHarness{r: r, k8s: k8s, omc: omc, rec: rec, acc: account}
}

func (h *writerHarness) run(t *testing.T) bool {
	t.Helper()
	var account billingv1alpha1.BillingAccount
	if err := h.k8s.Get(context.Background(), client.ObjectKeyFromObject(h.acc), &account); err != nil {
		t.Fatal(err)
	}
	waiting, err := h.r.reconcileInvoiceWriter(context.Background(), &account, "customer-id", writerApp)
	if err != nil {
		t.Fatalf("reconcileInvoiceWriter: %v", err)
	}
	return waiting
}

func (h *writerHarness) stripeInvoice(t *testing.T) *stripev1alpha1.StripeInvoice {
	t.Helper()
	var si stripev1alpha1.StripeInvoice
	if err := h.k8s.Get(context.Background(), types.NamespacedName{Namespace: "org-a", Name: "openmeter-01invoice"}, &si); err != nil {
		t.Fatalf("get StripeInvoice: %v", err)
	}
	return &si
}

// stripeProviderSyncs does what stripe-provider would: writes the draft
// and reports it Synced, with Stripe's subtotal and discount.
func (h *writerHarness) stripeProviderSyncs(t *testing.T, subtotal, discount string) {
	t.Helper()
	si := h.stripeInvoice(t)
	si.Status.ObservedGeneration = si.Generation
	si.Status.InvoiceID = "in_stripe"
	si.Status.Subtotal, si.Status.Discount = subtotal, discount
	si.Status.Lines = nil
	for _, l := range si.Spec.Lines {
		si.Status.Lines = append(si.Status.Lines, stripev1alpha1.StripeInvoiceLineStatus{Key: l.Key, InvoiceItemID: "ii_" + strings.ToLower(l.Key)})
	}
	apimeta.SetStatusCondition(&si.Status.Conditions, metav1.Condition{Type: stripev1alpha1.StripeInvoiceConditionSynced, Status: metav1.ConditionTrue, Reason: "Synced"})
	if err := h.k8s.Status().Update(context.Background(), si); err != nil {
		t.Fatal(err)
	}
}

func TestInvoiceWriter_WritesAnInvoiceThroughToPaid(t *testing.T) {
	h := newWriterHarness(t)
	h.omc.invoices = []om.Invoice{writerInvoice(openmeter.InvoiceExtendedStatusDraftSyncing)}

	if !h.run(t) {
		t.Error("an invoice waiting on the writer should requeue the account soon")
	}
	var account billingv1alpha1.BillingAccount
	_ = h.k8s.Get(context.Background(), client.ObjectKeyFromObject(h.acc), &account)
	if !apimeta.IsStatusConditionTrue(account.Status.Conditions, billingv1alpha1.BillingAccountConditionInvoiceWriterReady) {
		t.Error("InvoiceWriterReady isn't True")
	}

	si := h.stripeInvoice(t)
	if si.Spec.Purpose != stripev1alpha1.StripeInvoicePurposeInvoice || si.Spec.Source.InvoiceID != "01INVOICE" || si.Spec.Finalize {
		t.Fatalf("StripeInvoice spec = %+v", si.Spec)
	}
	if len(si.Spec.Lines) != 3 {
		t.Fatalf("want the fee, the billed tier and the charge, got %+v", si.Spec.Lines)
	}
	fee, tier, charge := si.Spec.Lines[0], si.Spec.Lines[1], si.Spec.Lines[2]
	if fee.Key != "01DETAILFEE" || fee.ServicePricingRef.Name != "rack-v1" || fee.Quantity != "1" || fee.UnitAmount != "300" ||
		fee.Amount != "300" || fee.DiscountPercent != "10" || fee.Tax == nil || fee.Tax.Code != "txcd_10000000" {
		t.Errorf("fee line = %+v", fee)
	}
	if tier.Quantity != "3" || tier.UnitAmount != "10" || tier.Amount != "30" || tier.Description != "Rack power: tier 2" {
		t.Errorf("tier line = %+v", tier)
	}
	if !tier.Period.End.Time.Equal(time.Date(2027, 1, 31, 23, 59, 59, 0, time.UTC)) {
		t.Errorf("period end = %v, want the last second of January", tier.Period.End)
	}
	if charge.Key != "01LINECHARGE" || charge.ServicePricingRef.Name != "custom-charge" || charge.Amount != "100.00" ||
		charge.Note != "Ticket 42" || charge.Description != "Remote hands, January" {
		t.Errorf("charge line = %+v", charge)
	}

	// Not synced yet: OpenMeter waits.
	h.run(t)
	if len(h.omc.draft) != 0 {
		t.Fatal("draft synchronized before stripe-provider wrote the draft")
	}

	h.stripeProviderSyncs(t, "430.00", "33.00")
	h.run(t)
	if h.omc.draftIDs["01INVOICE"] != "in_stripe" || len(h.omc.draft["01INVOICE"]) != 3 {
		t.Fatalf("draft synchronized = %q %+v", h.omc.draftIDs["01INVOICE"], h.omc.draft["01INVOICE"])
	}

	// OpenMeter moves to issuing: Stripe finalizes it, then OpenMeter hears.
	h.omc.invoices = []om.Invoice{writerInvoice(openmeter.InvoiceExtendedStatusIssuingSyncing)}
	h.run(t)
	if si = h.stripeInvoice(t); !si.Spec.Finalize {
		t.Fatal("the StripeInvoice wasn't marked to finalize")
	}
	if len(h.omc.issued) != 0 {
		t.Fatal("issuing synchronized before Stripe finalized it")
	}
	si.Status.ObservedGeneration = si.Generation
	si.Status.Number, si.Status.State = "DATUM-0001", stripev1alpha1.StripeInvoiceStateOpen
	si.Status.FinalizedAt = &metav1.Time{Time: time.Now()}
	apimeta.SetStatusCondition(&si.Status.Conditions, metav1.Condition{Type: stripev1alpha1.StripeInvoiceConditionFinalized, Status: metav1.ConditionTrue, Reason: "Finalized"})
	if err := h.k8s.Status().Update(context.Background(), si); err != nil {
		t.Fatal(err)
	}
	h.run(t)
	if h.omc.issued["01INVOICE"] != "DATUM-0001" {
		t.Fatalf("issued = %+v, want Stripe's number", h.omc.issued)
	}

	// Issued, then paid in Stripe: OpenMeter hears once.
	h.omc.invoices = nil
	if h.run(t) {
		t.Error("nothing is waiting any more")
	}
	if len(h.omc.payments) != 0 {
		t.Fatalf("payment reported while still open: %+v", h.omc.payments)
	}
	si = h.stripeInvoice(t)
	si.Status.State = stripev1alpha1.StripeInvoiceStatePaid
	if err := h.k8s.Status().Update(context.Background(), si); err != nil {
		t.Fatal(err)
	}
	h.run(t)
	h.run(t)
	if got := h.omc.payments["01INVOICE"]; len(got) != 1 || got[0] != openmeter.PaymentTriggerPaid {
		t.Errorf("payments = %+v, want paid once", got)
	}
}

func TestInvoiceWriter_HoldsWhenStripesTotalDiffers(t *testing.T) {
	h := newWriterHarness(t)
	h.omc.invoices = []om.Invoice{writerInvoice(openmeter.InvoiceExtendedStatusDraftSyncing)}
	h.run(t)
	h.stripeProviderSyncs(t, "430.00", "32.99")
	h.run(t)
	if len(h.omc.draft) != 0 {
		t.Fatal("draft synchronized although Stripe's total differs from OpenMeter's")
	}
	if ev := drainEvents(h.rec); !strings.Contains(ev, "397.01") || !strings.Contains(ev, "397.00") {
		t.Errorf("events = %q, want the two totals", ev)
	}
}

func TestInvoiceWriter_LeavesOtherAppsInvoicesAlone(t *testing.T) {
	h := newWriterHarness(t)
	inv := writerInvoice(openmeter.InvoiceExtendedStatusDraftSyncing)
	var apps om.BillingProfileAppsOrReference
	_ = apps.FromBillingProfileAppReferences(om.BillingProfileAppReferences{Invoicing: om.AppReference{Id: "app_stripe"}})
	inv.Workflow.Apps = &apps
	h.omc.invoices = []om.Invoice{inv}
	if h.run(t) {
		t.Error("an invoice on another app isn't the writer's to wait for")
	}
	var list stripev1alpha1.StripeInvoiceList
	_ = h.k8s.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Errorf("wrote another app's invoice: %+v", list.Items)
	}
}

func TestInvoiceWriter_DeletesADraftWithNothingToBill(t *testing.T) {
	h := newWriterHarness(t)
	inv := writerInvoice(openmeter.InvoiceExtendedStatusDraftSyncing)
	inv.Lines = &[]om.InvoiceLine{(*inv.Lines)[0]}
	(*inv.Lines)[0].Children = &[]om.InvoiceDetailedLine{{Id: "zero", Totals: om.InvoiceTotals{Amount: "0"}}}
	(*inv.Lines)[0].Totals.Amount = "0"
	h.omc.invoices = []om.Invoice{inv}
	h.run(t)
	if len(h.omc.deleted) != 1 || h.omc.deleted[0] != "01INVOICE" {
		t.Errorf("deleted = %v, want the empty draft", h.omc.deleted)
	}
}

func TestInvoiceWriter_HoldsALineWithoutItsPricing(t *testing.T) {
	h := newWriterHarness(t)
	inv := writerInvoice(openmeter.InvoiceExtendedStatusDraftSyncing)
	(*inv.Lines)[0].Description = strp("Something else")
	h.omc.invoices = []om.Invoice{inv}
	h.run(t)
	var list stripev1alpha1.StripeInvoiceList
	_ = h.k8s.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Fatal("wrote an invoice with a line it couldn't price")
	}
	if ev := drainEvents(h.rec); !strings.Contains(ev, "doesn't name the ServicePricing") {
		t.Errorf("events = %q", ev)
	}
}

func TestPaymentTrigger(t *testing.T) {
	now := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	due := func(d time.Time) *metav1.Time { return &metav1.Time{Time: d} }
	failed := []metav1.Condition{{Type: stripev1alpha1.StripeInvoiceConditionPaymentFailed, Status: metav1.ConditionTrue}}
	for _, tt := range []struct {
		name   string
		status stripev1alpha1.StripeInvoiceStatus
		want   openmeter.PaymentTrigger
	}{
		{"draft", stripev1alpha1.StripeInvoiceStatus{State: stripev1alpha1.StripeInvoiceStateDraft}, ""},
		{"open, not due", stripev1alpha1.StripeInvoiceStatus{State: stripev1alpha1.StripeInvoiceStateOpen, DueDate: due(now.Add(time.Hour))}, ""},
		{"open, payment failed", stripev1alpha1.StripeInvoiceStatus{State: stripev1alpha1.StripeInvoiceStateOpen, Conditions: failed}, openmeter.PaymentTriggerFailed},
		{"open, past due", stripev1alpha1.StripeInvoiceStatus{State: stripev1alpha1.StripeInvoiceStateOpen, DueDate: due(now.Add(-time.Hour)), Conditions: failed}, openmeter.PaymentTriggerOverdue},
		{"paid", stripev1alpha1.StripeInvoiceStatus{State: stripev1alpha1.StripeInvoiceStatePaid}, openmeter.PaymentTriggerPaid},
		{"void", stripev1alpha1.StripeInvoiceStatus{State: stripev1alpha1.StripeInvoiceStateVoid}, openmeter.PaymentTriggerVoid},
		{"uncollectible", stripev1alpha1.StripeInvoiceStatus{State: stripev1alpha1.StripeInvoiceStateUncollectible}, openmeter.PaymentTriggerUncollectible},
	} {
		if got := paymentTrigger(&stripev1alpha1.StripeInvoice{Status: tt.status}, now); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestQuantityAndUnit(t *testing.T) {
	for _, tt := range []struct {
		quantity, unit *string
		amount         string
		wantQ, wantU   string
	}{
		{strp("3"), strp("10"), "30", "3", "10"},
		// Usage too small to bill a cent still multiplies out.
		{strp("1234"), strp("0.0001"), "0.12", "1234", "0.0001"},
		// A prorated fee: OpenMeter's quantity and price don't make its amount.
		{strp("1"), strp("1000"), "430.11", "1", "430.11"},
		{nil, nil, "12.34", "1", "12.34"},
	} {
		amount := decimal.RequireFromString(tt.amount)
		if q, u := quantityAndUnit(tt.quantity, tt.unit, amount); q != tt.wantQ || u != tt.wantU {
			t.Errorf("quantityAndUnit(%v, %v, %s) = %s × %s, want %s × %s", deref(tt.quantity), deref(tt.unit), tt.amount, q, u, tt.wantQ, tt.wantU)
		}
	}
}

func TestPricingFromDescription(t *testing.T) {
	for in, want := range map[string]string{
		"ServicePricing servers":                                     "servers",
		"ServicePricing setup (compute.datumapis.com)":               "setup",
		"ServicePricing usage-v1 (x.datumapis.com), priced per vcpu": "usage-v1",
		"Rack":           "",
		"ServicePricing": "",
	} {
		if got := pricingFromDescription(&in); got != want {
			t.Errorf("pricingFromDescription(%q) = %q, want %q", in, got, want)
		}
	}
}

func drainEvents(rec *record.FakeRecorder) string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return strings.Join(out, "\n")
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// TestDiscountPercent_FromTheDetailedLine uses the shape staging's
// OpenMeter returns: the rate card's percentage is on the detailed line's
// own amount discount, and not on any rate card snapshot.
func TestDiscountPercent_FromTheDetailedLine(t *testing.T) {
	var d om.InvoiceDetailedLine
	if err := json.Unmarshal([]byte(`{
		"id": "01DETAIL", "name": "ALB Egress: usage price for tier 1", "type": "flat_fee",
		"quantity": "12", "perUnitAmount": "0.05",
		"rateCard": {"price": {"amount": "0.05", "paymentTerm": "in_arrears", "type": "flat"}, "quantity": "12"},
		"totals": {"amount": "0.6", "discountsTotal": "0.6", "total": "0"},
		"discounts": {"amount": [{"amount": "0.6", "id": "d1",
			"reason": {"correlationId": "c1", "percentage": 100, "type": "ratecard_percentage"}}]}
	}`), &d); err != nil {
		t.Fatal(err)
	}
	if got, ok := discountPercent(d, om.InvoiceLine{}); !ok || got != "100" {
		t.Errorf("discountPercent = %q, %v; want 100", got, ok)
	}

	var spend om.InvoiceDetailedLine
	_ = json.Unmarshal([]byte(`{"discounts": {"amount": [{"amount": "5", "reason": {"type": "maximum_spend"}}]}}`), &spend)
	if _, ok := discountPercent(spend, om.InvoiceLine{}); ok {
		t.Error("a maximum-spend discount isn't a percentage Stripe can take")
	}
}
