// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	om "github.com/openmeterio/openmeter/api/client/go"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/stripe"
)

const testStripeCustomer = "cus_acme"

// newStripeInvoiceSyncReconciler is newInvoiceSyncReconciler for an account
// whose Stripe customer has stripeInvoices.
func newStripeInvoiceSyncReconciler(
	t *testing.T,
	invoices []om.Invoice,
	stripeInvoices []stripe.Invoice,
	objs ...client.Object,
) (*BillingAccountReconciler, client.Client, *billingv1alpha1.BillingAccount, *fakeStripe) {
	t.Helper()
	account := baseBillingAccount()
	objs = append(objs, publishedStripeCustomer(account, testStripeCustomer, account.UID))
	r, k8s, account := newInvoiceSyncReconciler(t, invoices, objs...)
	fake := &fakeStripe{customers: map[string][]stripe.Invoice{testStripeCustomer: stripeInvoices}}
	r.Stripe = fake
	return r, k8s, account, fake
}

func stripeInvoice(id, status string, total int64) stripe.Invoice {
	created := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	inv := stripe.Invoice{
		ID:               id,
		Number:           "ACME-" + strings.TrimPrefix(id, "in_"),
		Status:           status,
		Currency:         "USD",
		Total:            total,
		AmountDue:        total,
		AmountRemaining:  total,
		Created:          created,
		PeriodStart:      created,
		PeriodEnd:        created,
		HostedInvoiceURL: "https://invoice.stripe.com/i/" + id,
	}
	switch status {
	case stripe.InvoiceStatusPaid:
		inv.AmountPaid, inv.AmountRemaining = total, 0
		paidAt := created.Add(time.Hour)
		inv.PaidAt = &paidAt
	case stripe.InvoiceStatusDraft:
		inv.Number, inv.HostedInvoiceURL = "", ""
	}
	return inv
}

// miloInvoicesByStripeID lists the Milo Invoices by the Stripe invoice id
// on them.
func miloInvoicesByStripeID(t *testing.T, k8s client.Client) map[string]billingv1alpha1.Invoice {
	t.Helper()
	var list billingv1alpha1.InvoiceList
	if err := k8s.List(context.Background(), &list); err != nil {
		t.Fatalf("list Invoices: %v", err)
	}
	out := map[string]billingv1alpha1.Invoice{}
	for _, inv := range list.Items {
		if id := inv.Annotations[StripeInvoiceKeyAnnotation]; id != "" {
			out[id] = inv
		}
	}
	return out
}

func syncInvoices(t *testing.T, r *BillingAccountReconciler, account *billingv1alpha1.BillingAccount, passes int) {
	t.Helper()
	for pass := 1; pass <= passes; pass++ {
		if err := r.reconcileInvoices(context.Background(), account, "cust"); err != nil {
			t.Fatalf("reconcileInvoices pass %d: %v", pass, err)
		}
	}
}

// TestReconcileInvoices_VoidAndReissueInStripe is what staff do to correct
// an invoice: void OpenMeter's in Stripe (OpenMeter follows, via its
// webhook) and create a replacement in Stripe, which OpenMeter never sees.
// Milo shows both, and the replacement is what's owed.
func TestReconcileInvoices_VoidAndReissueInStripe(t *testing.T) {
	original := inStripe(omInvoice("01ORIGINAL", om.InvoiceStatusVoided, "2000.00"), "in_original")
	voided := stripeInvoice("in_original", stripe.InvoiceStatusVoid, 200000)
	voided.AmountRemaining = 0
	voided.Metadata = map[string]string{stripe.MetadataOpenMeterInvoiceID: "01ORIGINAL"}
	replacement := stripeInvoice("in_replacement", stripe.InvoiceStatusOpen, 180000)
	due := replacement.Created.AddDate(0, 0, 30)
	replacement.DueDate = &due

	r, k8s, account, fake := newStripeInvoiceSyncReconciler(t, []om.Invoice{original}, []stripe.Invoice{voided, replacement})
	syncInvoices(t, r, account, 2)

	got := miloInvoicesByStripeID(t, k8s)
	if len(got) != 2 {
		t.Fatalf("Invoices for Stripe invoices %v, want the original and its replacement", keysOf(got))
	}
	if obj := got["in_original"]; obj.Annotations[InvoiceKeyAnnotation] != "01ORIGINAL" ||
		obj.Status.Phase != billingv1alpha1.InvoicePhaseVoid || obj.Status.AmountDue != "0" {
		t.Errorf("original: annotations %v phase %s due %s, want OpenMeter's, Void, nothing owed",
			obj.Annotations, obj.Status.Phase, obj.Status.AmountDue)
	}

	obj := got["in_replacement"]
	if obj.Annotations[InvoiceKeyAnnotation] != "" || obj.Annotations[StripeInvoiceNumberAnnotation] != "ACME-replacement" {
		t.Errorf("replacement annotations = %v, want only Stripe's id and number", obj.Annotations)
	}
	if !regexp.MustCompile(`^acme-account-2026-11-[0-9a-f]{10}$`).MatchString(obj.Name) {
		t.Errorf("replacement named %q, want acme-account-2026-11-<hash>", obj.Name)
	}
	if obj.Status.Phase != billingv1alpha1.InvoicePhaseOpen || obj.Status.Total != "1800.00" ||
		obj.Status.AmountDue != "1800.00" || obj.Status.AmountPaid != "0.00" ||
		obj.Status.DueDate == nil || !obj.Status.DueDate.Time.Equal(due) ||
		obj.Status.DocumentURI != "https://invoice.stripe.com/i/in_replacement" {
		t.Errorf("replacement status = %+v", obj.Status)
	}
	if !obj.Spec.Period.Start.Time.Equal(replacement.Created) || !obj.Spec.Period.End.Time.Equal(replacement.Created) {
		t.Errorf("replacement period %s..%s, want its creation", obj.Spec.Period.Start.Time, obj.Spec.Period.End.Time)
	}
	cond := apimeta.FindStatusCondition(obj.Status.Conditions, billingv1alpha1.InvoiceConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || !strings.Contains(cond.Message, "created in Stripe (ACME-replacement") {
		t.Errorf("Ready = %+v", cond)
	}

	// Both Stripe invoices name their Milo Invoice, written once.
	for id, want := range map[string]string{"in_original": got["in_original"].Name, "in_replacement": obj.Name} {
		md := fake.metadata[id]
		if md[StripeMetadataMiloInvoice] != want || md[StripeMetadataMiloBillingAccount] != account.Name {
			t.Errorf("metadata written onto %s = %v, want milo_invoice %s", id, md, want)
		}
	}
	if fake.calls != 0 {
		t.Errorf("Stripe invoice read %d times, want the links from the listing", fake.calls)
	}
}

func TestReconcileInvoices_StripeAmountsForOpenMetersInvoice(t *testing.T) {
	inv := inStripe(omInvoice("01OPEN", om.InvoiceStatusIssued, "2000.00"), "in_om")
	credited := stripeInvoice("in_om", stripe.InvoiceStatusOpen, 200000)
	// A $500 credit note and a $300 payment.
	credited.AmountDue, credited.AmountPaid, credited.AmountRemaining = 150000, 30000, 120000
	credited.Metadata = map[string]string{stripe.MetadataOpenMeterInvoiceID: "01OPEN"}

	r, k8s, account, fake := newStripeInvoiceSyncReconciler(t, []om.Invoice{inv}, []stripe.Invoice{credited})
	syncInvoices(t, r, account, 2)

	got := listMiloInvoices(t, k8s)
	if len(got) != 1 {
		t.Fatalf("Invoices %v, want only OpenMeter's: its Stripe invoice isn't another one", keysOf(got))
	}
	obj := got["01OPEN"]
	if obj.Status.Total != "2000.00" || obj.Status.AmountPaid != "300.00" || obj.Status.AmountDue != "1200.00" {
		t.Errorf("total/paid/due = %s/%s/%s, want 2000.00/300.00/1200.00", obj.Status.Total, obj.Status.AmountPaid, obj.Status.AmountDue)
	}
	if obj.Annotations[StripeInvoiceKeyAnnotation] != "in_om" || obj.Annotations[StripeInvoiceNumberAnnotation] != "ACME-om" {
		t.Errorf("annotations = %v, want the Stripe invoice's id and number", obj.Annotations)
	}
	if md := fake.metadata["in_om"]; md[StripeMetadataMiloInvoice] != obj.Name || len(fake.metadata) != 1 {
		t.Errorf("metadata written = %v, want milo_invoice %s on in_om once", fake.metadata, obj.Name)
	}
}

func TestStripeInvoiceAmounts_OnlyWhileTheyAgree(t *testing.T) {
	open := stripeInvoice("in_1", stripe.InvoiceStatusOpen, 1000)
	open.AmountRemaining = 400
	paid := stripeInvoice("in_1", stripe.InvoiceStatusPaid, 1000)
	paid.AmountPaid = 600 // the rest credited
	uncollectible := stripeInvoice("in_1", stripe.InvoiceStatusUncollectible, 1000)
	uncollectible.AmountRemaining = 700
	eur := open
	eur.Currency = "EUR"

	for _, tc := range []struct {
		name      string
		status    om.InvoiceStatus
		inStripe  *stripe.Invoice
		wantPaid  string
		wantDue   string
		wantFound bool
	}{
		{"both open", om.InvoiceStatusIssued, &open, "0.00", "4.00", true},
		{"overdue and open", om.InvoiceStatusOverdue, &open, "0.00", "4.00", true},
		{"both paid", om.InvoiceStatusPaid, &paid, "6.00", "0", true},
		{"both uncollectible", om.InvoiceStatusUncollectible, &uncollectible, "0.00", "7.00", true},
		{"paid in Stripe, OpenMeter not yet told", om.InvoiceStatusIssued, &paid, "", "", false},
		{"voided", om.InvoiceStatusVoided, &open, "", "", false},
		{"another currency", om.InvoiceStatusIssued, &eur, "", "", false},
		{"no Stripe invoice", om.InvoiceStatusIssued, nil, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paid, due, ok := stripeInvoiceAmounts(omInvoice("01", tc.status, "10.00"), tc.inStripe)
			if paid != tc.wantPaid || due != tc.wantDue || ok != tc.wantFound {
				t.Errorf("= %q, %q, %v; want %q, %q, %v", paid, due, ok, tc.wantPaid, tc.wantDue, tc.wantFound)
			}
		})
	}
}

func TestReconcileInvoices_WhichStripeInvoicesAreStaffs(t *testing.T) {
	// OpenMeter is still issuing this one: its Stripe invoice exists, but
	// OpenMeter hasn't recorded it yet.
	issuing := omInvoice("01ISSUING", om.InvoiceStatusIssuing, "50.00")
	issuingInStripe := stripeInvoice("in_issuing", stripe.InvoiceStatusOpen, 5000)
	issuingInStripe.Metadata = map[string]string{stripe.MetadataOpenMeterInvoiceID: "01ISSUING"}
	// A revision staff made of OpenMeter's invoice carries its metadata.
	original := inStripe(omInvoice("01ORIGINAL", om.InvoiceStatusVoided, "50.00"), "in_original")
	revision := stripeInvoice("in_revision", stripe.InvoiceStatusOpen, 4000)
	revision.Metadata = map[string]string{stripe.MetadataOpenMeterInvoiceID: "01ORIGINAL"}
	draft := stripeInvoice("in_draft", stripe.InvoiceStatusDraft, 9900)

	r, k8s, account, fake := newStripeInvoiceSyncReconciler(t,
		[]om.Invoice{issuing, original}, []stripe.Invoice{issuingInStripe, revision, draft})
	syncInvoices(t, r, account, 1)

	got := miloInvoicesByStripeID(t, k8s)
	if _, ok := got["in_revision"]; !ok || len(got) != 1 {
		t.Errorf("Invoices for Stripe invoices %v, want only the revision", keysOf(got))
	}
	if _, ok := fake.metadata["in_draft"]; ok {
		t.Error("wrote metadata onto a draft")
	}
}

func TestProjectStripeInvoiceStatus(t *testing.T) {
	now := time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)
	pastDue := stripeInvoice("in_1", stripe.InvoiceStatusOpen, 1000)
	due := now.AddDate(0, 0, -1)
	pastDue.DueDate = &due
	paid := stripeInvoice("in_1", stripe.InvoiceStatusPaid, 1000)
	void := stripeInvoice("in_1", stripe.InvoiceStatusVoid, 1000)
	uncollectible := stripeInvoice("in_1", stripe.InvoiceStatusUncollectible, 1000)
	uncollectible.AmountPaid, uncollectible.AmountRemaining = 250, 750
	charged := stripeInvoice("in_1", stripe.InvoiceStatusOpen, 1000) // charged to a card: no due date

	for _, tc := range []struct {
		name             string
		inv              stripe.Invoice
		phase            billingv1alpha1.InvoicePhase
		paid, due        string
		paidAtFromStripe bool
	}{
		{"past its due date", pastDue, billingv1alpha1.InvoicePhasePastDue, "0.00", "10.00", false},
		{"no due date", charged, billingv1alpha1.InvoicePhaseOpen, "0.00", "10.00", false},
		{"paid", paid, billingv1alpha1.InvoicePhasePaid, "10.00", "0", true},
		{"void", void, billingv1alpha1.InvoicePhaseVoid, "0.00", "0", false},
		{"uncollectible keeps the balance", uncollectible, billingv1alpha1.InvoicePhaseVoid, "2.50", "7.50", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := &billingv1alpha1.Invoice{}
			projectStripeInvoiceStatus(obj, baseBillingAccount(), &tc.inv, now)
			if obj.Status.Phase != tc.phase || obj.Status.AmountPaid != tc.paid || obj.Status.AmountDue != tc.due || obj.Status.Total != "10.00" {
				t.Errorf("phase/paid/due/total = %s/%s/%s/%s, want %s/%s/%s/10.00",
					obj.Status.Phase, obj.Status.AmountPaid, obj.Status.AmountDue, obj.Status.Total, tc.phase, tc.paid, tc.due)
			}
			if tc.paidAtFromStripe && (obj.Status.PaidAt == nil || !obj.Status.PaidAt.Time.Equal(*tc.inv.PaidAt)) {
				t.Errorf("paidAt = %v, want Stripe's %v", obj.Status.PaidAt, tc.inv.PaidAt)
			}
			if !tc.paidAtFromStripe && obj.Status.PaidAt != nil {
				t.Errorf("paidAt = %v, want none", obj.Status.PaidAt)
			}
		})
	}

	t.Run("another currency", func(t *testing.T) {
		inv := stripeInvoice("in_1", stripe.InvoiceStatusOpen, 1000)
		inv.Currency = "EUR"
		obj := &billingv1alpha1.Invoice{}
		projectStripeInvoiceStatus(obj, baseBillingAccount(), &inv, now)
		cond := apimeta.FindStatusCondition(obj.Status.Conditions, billingv1alpha1.InvoiceConditionReady)
		if cond == nil || cond.Reason != invoiceReasonCurrencyMismatch {
			t.Errorf("Ready = %+v, want CurrencyMismatch", cond)
		}
	})
}

func TestReconcileInvoices_StripeInvoiceUpdatesAndGoesMissing(t *testing.T) {
	inv := stripeInvoice("in_manual", stripe.InvoiceStatusOpen, 5000)
	r, k8s, account, fake := newStripeInvoiceSyncReconciler(t, nil, []stripe.Invoice{inv})
	syncInvoices(t, r, account, 1)

	// Paid in Stripe.
	fake.customers[testStripeCustomer][0].Status = stripe.InvoiceStatusPaid
	fake.customers[testStripeCustomer][0].AmountPaid = 5000
	fake.customers[testStripeCustomer][0].AmountRemaining = 0
	syncInvoices(t, r, account, 1)
	obj := miloInvoicesByStripeID(t, k8s)["in_manual"]
	if obj.Status.Phase != billingv1alpha1.InvoicePhasePaid || obj.Status.AmountDue != "0" {
		t.Errorf("phase %s due %s, want Paid with nothing owed", obj.Status.Phase, obj.Status.AmountDue)
	}

	// No longer listed for the customer: flagged, phase kept.
	fake.customers[testStripeCustomer] = nil
	syncInvoices(t, r, account, 1)
	obj = miloInvoicesByStripeID(t, k8s)["in_manual"]
	cond := apimeta.FindStatusCondition(obj.Status.Conditions, billingv1alpha1.InvoiceConditionReady)
	if obj.Status.Phase != billingv1alpha1.InvoicePhasePaid || cond == nil || cond.Reason != invoiceReasonNotFoundInStripe {
		t.Errorf("phase %s, Ready %+v; want Paid kept and NotFoundInStripe", obj.Status.Phase, cond)
	}
}

func TestReconcileInvoices_StripeFailingStillProjectsOpenMeter(t *testing.T) {
	inv := inStripe(omInvoice("01OPEN", om.InvoiceStatusIssued, "10.00"), "in_1")
	r, k8s, account, fake := newStripeInvoiceSyncReconciler(t, []om.Invoice{inv}, nil)
	fake.listErr = errors.New("stripe is down")
	fake.urls = map[string]string{"in_1": "https://invoice.stripe.com/i/one"}

	err := r.reconcileInvoices(context.Background(), account, "cust")
	if err == nil || !strings.Contains(err.Error(), "stripe is down") {
		t.Errorf("reconcileInvoices = %v, want Stripe's failure reported", err)
	}
	obj := listMiloInvoices(t, k8s)["01OPEN"]
	if obj.Status.Phase != billingv1alpha1.InvoicePhaseOpen || obj.Status.DocumentURI != "https://invoice.stripe.com/i/one" {
		t.Errorf("phase %s link %q, want OpenMeter's invoice projected as before", obj.Status.Phase, obj.Status.DocumentURI)
	}
}

func TestReconcileInvoices_NoStripeCustomerNoStripeListing(t *testing.T) {
	r, _, account := newInvoiceSyncReconciler(t, []om.Invoice{omInvoice("01OPEN", om.InvoiceStatusIssued, "10.00")})
	fake := &fakeStripe{}
	r.Stripe = fake
	syncInvoices(t, r, account, 1)
	if fake.listCalls != 0 {
		t.Errorf("listed Stripe invoices %d times for an account with no Stripe customer", fake.listCalls)
	}
}

func TestReconcileInvoices_StripeFailingKeepsStripesAmounts(t *testing.T) {
	inv := inStripe(omInvoice("01OPEN", om.InvoiceStatusIssued, "20.00"), "in_om")
	credited := stripeInvoice("in_om", stripe.InvoiceStatusOpen, 2000)
	credited.AmountRemaining = 500
	r, k8s, account, fake := newStripeInvoiceSyncReconciler(t, []om.Invoice{inv}, []stripe.Invoice{credited})
	syncInvoices(t, r, account, 1)

	fake.listErr = errors.New("stripe is down")
	_ = r.reconcileInvoices(context.Background(), account, "cust")
	if obj := listMiloInvoices(t, k8s)["01OPEN"]; obj.Status.AmountDue != "5.00" {
		t.Errorf("amountDue = %s while Stripe is down, want Stripe's last 5.00", obj.Status.AmountDue)
	}
}
