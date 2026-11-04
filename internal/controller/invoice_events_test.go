// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/stripe"
)

// timelineEvent is one Event a timelineRecorder saw.
type timelineEvent struct {
	object      runtime.Object
	annotations map[string]string
	eventType   string
	reason      string
	message     string
}

// timelineRecorder is a record.EventRecorder that keeps what it's given.
type timelineRecorder struct {
	events []timelineEvent
}

func (c *timelineRecorder) Event(object runtime.Object, eventType, reason, message string) {
	c.AnnotatedEventf(object, nil, eventType, reason, "%s", message)
}

func (c *timelineRecorder) Eventf(object runtime.Object, eventType, reason, messageFmt string, args ...any) {
	c.AnnotatedEventf(object, nil, eventType, reason, messageFmt, args...)
}

func (c *timelineRecorder) AnnotatedEventf(object runtime.Object, annotations map[string]string, eventType, reason, messageFmt string, args ...any) {
	c.events = append(c.events, timelineEvent{
		object: object, annotations: annotations, eventType: eventType, reason: reason,
		message: fmt.Sprintf(messageFmt, args...),
	})
}

// reasons returns the reasons of the Events recorded on Invoices.
func (c *timelineRecorder) reasons() []string {
	var out []string
	for _, e := range c.events {
		if _, ok := e.object.(*billingv1alpha1.Invoice); ok {
			out = append(out, e.reason)
		}
	}
	return out
}

func (c *timelineRecorder) last(reason string) timelineEvent {
	for i := len(c.events) - 1; i >= 0; i-- {
		if c.events[i].reason == reason {
			return c.events[i]
		}
	}
	return timelineEvent{}
}

func eventInvoice(phase billingv1alpha1.InvoicePhase, paid, due string) *billingv1alpha1.Invoice {
	obj := &billingv1alpha1.Invoice{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-2026-10", Namespace: "organization-acme"},
		Spec: billingv1alpha1.InvoiceSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: "acme"},
			Period: billingv1alpha1.InvoicePeriod{
				Start: metav1.NewTime(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)),
				End:   metav1.NewTime(time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)),
			},
		},
	}
	obj.Status = invoiceStatus(phase, paid, due)
	return obj
}

func invoiceStatus(phase billingv1alpha1.InvoicePhase, paid, due string) billingv1alpha1.InvoiceStatus {
	dueDate := metav1.NewTime(time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC))
	return billingv1alpha1.InvoiceStatus{
		Phase: phase, CurrencyCode: "USD", Total: "482.19", AmountPaid: paid, AmountDue: due, DueDate: &dueDate,
		Conditions: []metav1.Condition{{Type: billingv1alpha1.InvoiceConditionReady, Status: metav1.ConditionTrue, Reason: string(phase)}},
	}
}

func reasonsOf(events []invoiceEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.reason)
	}
	return out
}

func TestInvoiceStatusEvents(t *testing.T) {
	const (
		open    = billingv1alpha1.InvoicePhaseOpen
		paid    = billingv1alpha1.InvoicePhasePaid
		pastDue = billingv1alpha1.InvoicePhasePastDue
		void    = billingv1alpha1.InvoicePhaseVoid
	)
	mismatch := invoiceStatus(open, "0", "482.19")
	mismatch.Conditions[0].Status, mismatch.Conditions[0].Reason = metav1.ConditionFalse, invoiceReasonCurrencyMismatch

	for name, tc := range map[string]struct {
		before billingv1alpha1.InvoiceStatus
		after  billingv1alpha1.InvoiceStatus
		want   []string
	}{
		"issued": {
			billingv1alpha1.InvoiceStatus{}, invoiceStatus(open, "0", "482.19"),
			[]string{billingv1alpha1.EventReasonInvoiceIssued},
		},
		"issued and paid in one sync": {
			billingv1alpha1.InvoiceStatus{}, invoiceStatus(paid, "482.19", "0"),
			[]string{billingv1alpha1.EventReasonInvoiceIssued, billingv1alpha1.EventReasonInvoicePaymentSucceeded},
		},
		"unchanged": {
			invoiceStatus(open, "0", "482.19"), invoiceStatus(open, "0", "482.19"), nil,
		},
		"paid": {
			invoiceStatus(open, "0", "482.19"), invoiceStatus(paid, "482.19", "0"),
			[]string{billingv1alpha1.EventReasonInvoicePaymentSucceeded},
		},
		"past due": {
			invoiceStatus(open, "0", "482.19"), invoiceStatus(pastDue, "0", "482.19"),
			[]string{billingv1alpha1.EventReasonInvoicePastDue},
		},
		"partly paid": {
			invoiceStatus(open, "0", "482.19"), invoiceStatus(open, "200.00", "282.19"),
			[]string{billingv1alpha1.EventReasonInvoicePartiallyPaid},
		},
		"partly paid again while past due": {
			invoiceStatus(pastDue, "200.00", "282.19"), invoiceStatus(pastDue, "300.00", "182.19"),
			[]string{billingv1alpha1.EventReasonInvoicePartiallyPaid},
		},
		"a credit note lowers what's due": {
			invoiceStatus(open, "200.00", "282.19"), invoiceStatus(open, "200.00", "182.19"), nil,
		},
		"voided": {
			invoiceStatus(open, "0", "482.19"), invoiceStatus(void, "0", "0"),
			[]string{billingv1alpha1.EventReasonInvoiceVoided},
		},
		"voided before it was shown": {
			billingv1alpha1.InvoiceStatus{}, invoiceStatus(void, "0", "0"), nil,
		},
		"currency mismatch": {
			invoiceStatus(open, "0", "482.19"), mismatch,
			[]string{billingv1alpha1.EventReasonInvoiceCurrencyMismatch},
		},
		"currency mismatch persists": {
			mismatch, mismatch, nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			obj := eventInvoice("", "", "")
			obj.Status = tc.after
			got := reasonsOf(invoiceStatusEvents(obj, tc.before))
			if !slices.Equal(got, tc.want) {
				t.Errorf("events = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInvoiceStatusEventDetail(t *testing.T) {
	obj := eventInvoice(billingv1alpha1.InvoicePhaseOpen, "0", "482.19")
	events := invoiceStatusEvents(obj, billingv1alpha1.InvoiceStatus{})
	if len(events) != 1 {
		t.Fatalf("events = %v", reasonsOf(events))
	}
	want := map[string]string{
		billingv1alpha1.EventAnnotationBillingAccount: "acme",
		billingv1alpha1.EventAnnotationPeriodStart:    "2026-10-01T00:00:00Z",
		billingv1alpha1.EventAnnotationCurrency:       "USD",
		billingv1alpha1.EventAnnotationAmount:         "482.19",
		billingv1alpha1.EventAnnotationDueDate:        "2026-11-30T00:00:00Z",
	}
	if fmt.Sprint(events[0].detail) != fmt.Sprint(want) {
		t.Errorf("detail = %v, want %v", events[0].detail, want)
	}
}

func TestRecordInvoiceEventsScope(t *testing.T) {
	rec := &timelineRecorder{}
	obj := eventInvoice(billingv1alpha1.InvoicePhaseOpen, "0", "482.19")
	recordInvoiceEvents(rec, obj,
		invoiceEvent{reason: "Customer", detail: map[string]string{"k": "v"}},
		invoiceEvent{reason: "Staff", detail: map[string]string{"k": "v"}, staffOnly: true},
	)
	if got := rec.events[0].annotations; got[billingv1alpha1.ScopeTypeAnnotation] != "Organization" ||
		got[billingv1alpha1.ScopeNameAnnotation] != "acme" || got["k"] != "v" {
		t.Errorf("customer event annotations = %v, want the organization's scope and the detail", got)
	}
	if got := rec.events[1].annotations; got[billingv1alpha1.ScopeTypeAnnotation] != "" || got["k"] != "v" {
		t.Errorf("staff-only event annotations = %v, want the detail without a scope", got)
	}

	// A nil recorder records nothing and doesn't panic.
	recordInvoiceEvents(nil, obj, invoiceEvent{reason: "Customer"})
}

func TestDeclineReason(t *testing.T) {
	for want, attempts := range map[billingv1alpha1.DeclineReason][]*stripe.PaymentAttempt{
		billingv1alpha1.DeclineReasonInsufficientFunds: {
			{ErrorType: "card_error", ErrorCode: "card_declined", DeclineCode: "insufficient_funds"},
		},
		billingv1alpha1.DeclineReasonExpiredCard: {
			{ErrorType: "card_error", ErrorCode: "expired_card"},
			{ErrorType: "card_error", ErrorCode: "card_declined", DeclineCode: "expired_card"},
		},
		billingv1alpha1.DeclineReasonAuthenticationRequired: {
			{ErrorType: "card_error", ErrorCode: "card_declined", DeclineCode: "authentication_required"},
			{ErrorType: "invalid_request_error", ErrorCode: "payment_intent_authentication_failure"},
		},
		billingv1alpha1.DeclineReasonProcessingError: {
			{ErrorType: "card_error", ErrorCode: "processing_error"},
			{ErrorType: "card_error", ErrorCode: "card_declined", DeclineCode: "try_again_later"},
			{ErrorType: "api_error"},
		},
		billingv1alpha1.DeclineReasonDeclined: {
			{ErrorType: "card_error", ErrorCode: "card_declined", DeclineCode: "do_not_honor"},
			{ErrorType: "card_error", ErrorCode: "card_declined", DeclineCode: "fraudulent"},
			{},
			nil,
		},
	} {
		for _, a := range attempts {
			if got := declineReason(a); got != want {
				t.Errorf("declineReason(%+v) = %s, want %s", a, got, want)
			}
		}
	}
}

// failedStripeInvoice is an open Stripe invoice whose first charge failed.
func failedStripeInvoice(id string) stripe.Invoice {
	inv := stripeInvoice(id, stripe.InvoiceStatusOpen, 48219)
	inv.AttemptCount = 1
	next := time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC)
	inv.NextPaymentAttempt = &next
	return inv
}

func TestReconcileInvoices_RecordsEachTransitionOnce(t *testing.T) {
	r, k8s, account, fake := newStripeInvoiceSyncReconciler(t, nil, []stripe.Invoice{failedStripeInvoice("in_1")})
	rec := &timelineRecorder{}
	r.Recorder = rec
	fake.attempts = map[string]*stripe.PaymentAttempt{"in_1": {
		Status: stripe.PaymentIntentRequiresPaymentMethod, ErrorType: "card_error",
		ErrorCode: "card_declined", DeclineCode: "insufficient_funds",
	}}

	syncInvoices(t, r, account, 3)
	want := []string{billingv1alpha1.EventReasonInvoiceIssued, billingv1alpha1.EventReasonInvoicePaymentFailed}
	if !slices.Equal(rec.reasons(), want) {
		t.Fatalf("events after three syncs = %v, want %v", rec.reasons(), want)
	}
	if fake.attemptCalls != 1 {
		t.Errorf("read the payment attempt %d times, want once", fake.attemptCalls)
	}
	failed := rec.last(billingv1alpha1.EventReasonInvoicePaymentFailed).annotations
	for key, want := range map[string]string{
		billingv1alpha1.EventAnnotationBillingAccount: account.Name,
		billingv1alpha1.EventAnnotationCurrency:       "USD",
		billingv1alpha1.EventAnnotationAmount:         "482.19",
		billingv1alpha1.EventAnnotationDeclineReason:  string(billingv1alpha1.DeclineReasonInsufficientFunds),
		billingv1alpha1.EventAnnotationNextAttempt:    "2026-11-05T09:30:00Z",
	} {
		if failed[key] != want {
			t.Errorf("failed event %s = %q, want %q", key, failed[key], want)
		}
	}
	for _, v := range failed {
		if v == "card_declined" || v == "insufficient_funds" {
			t.Errorf("failed event carries Stripe's code %q", v)
		}
	}
	if got := miloInvoicesByStripeID(t, k8s)["in_1"].Annotations[PaymentAttemptsReportedAnnotation]; got != "1" {
		t.Errorf("%s = %q, want 1", PaymentAttemptsReportedAnnotation, got)
	}

	// The retry asks the card holder to authenticate.
	retry := failedStripeInvoice("in_1")
	retry.AttemptCount = 2
	fake.customers[testStripeCustomer] = []stripe.Invoice{retry}
	fake.attempts["in_1"] = &stripe.PaymentAttempt{Status: stripe.PaymentIntentRequiresAction}
	syncInvoices(t, r, account, 2)

	// The third attempt succeeds.
	paid := stripeInvoice("in_1", stripe.InvoiceStatusPaid, 48219)
	paid.AttemptCount = 3
	fake.customers[testStripeCustomer] = []stripe.Invoice{paid}
	syncInvoices(t, r, account, 2)

	want = append(want, billingv1alpha1.EventReasonInvoicePaymentActionRequired, billingv1alpha1.EventReasonInvoicePaymentSucceeded)
	if !slices.Equal(rec.reasons(), want) {
		t.Errorf("events = %v, want %v", rec.reasons(), want)
	}
}

func TestReconcileInvoices_ProcessingPaymentIsReportedOnceSettled(t *testing.T) {
	r, _, account, fake := newStripeInvoiceSyncReconciler(t, nil, []stripe.Invoice{failedStripeInvoice("in_1")})
	rec := &timelineRecorder{}
	r.Recorder = rec
	fake.attempts = map[string]*stripe.PaymentAttempt{"in_1": {Status: stripe.PaymentIntentProcessing}}

	syncInvoices(t, r, account, 2)
	if got := rec.reasons(); !slices.Equal(got, []string{billingv1alpha1.EventReasonInvoiceIssued}) {
		t.Fatalf("events while processing = %v, want only InvoiceIssued", got)
	}

	fake.attempts["in_1"] = &stripe.PaymentAttempt{Status: stripe.PaymentIntentRequiresPaymentMethod, ErrorCode: "card_declined"}
	syncInvoices(t, r, account, 2)
	want := []string{billingv1alpha1.EventReasonInvoiceIssued, billingv1alpha1.EventReasonInvoicePaymentFailed}
	if got := rec.reasons(); !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	if got := rec.last(billingv1alpha1.EventReasonInvoicePaymentFailed).annotations[billingv1alpha1.EventAnnotationDeclineReason]; got != string(billingv1alpha1.DeclineReasonDeclined) {
		t.Errorf("decline reason = %q, want Declined", got)
	}
}

func TestReconcileInvoices_AttemptReadFailureIsRetried(t *testing.T) {
	r, _, account, fake := newStripeInvoiceSyncReconciler(t, nil, []stripe.Invoice{failedStripeInvoice("in_1")})
	rec := &timelineRecorder{}
	r.Recorder = rec
	fake.attemptErr = errors.New("stripe is down")
	syncInvoices(t, r, account, 1)

	fake.attemptErr = nil
	fake.attempts = map[string]*stripe.PaymentAttempt{"in_1": {Status: stripe.PaymentIntentRequiresPaymentMethod}}
	syncInvoices(t, r, account, 2)
	want := []string{billingv1alpha1.EventReasonInvoiceIssued, billingv1alpha1.EventReasonInvoicePaymentFailed}
	if got := rec.reasons(); !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

// Invoices projected before this provider recorded events must not replay
// their history when it's rolled out.
func TestReconcileInvoices_NoEventsForExistingInvoicesOnRollout(t *testing.T) {
	earlier := failedStripeInvoice("in_1")
	earlier.AttemptCount = 3
	r, k8s, account, fake := newStripeInvoiceSyncReconciler(t, nil, []stripe.Invoice{earlier})
	syncInvoices(t, r, account, 1)

	// Forget the attempts, as an invoice projected by the previous version.
	obj := miloInvoicesByStripeID(t, k8s)["in_1"]
	delete(obj.Annotations, PaymentAttemptsReportedAnnotation)
	if err := k8s.Update(context.Background(), &obj); err != nil {
		t.Fatal(err)
	}
	fake.attemptCalls = 0

	rec := &timelineRecorder{}
	r.Recorder = rec
	syncInvoices(t, r, account, 2)
	if got := rec.reasons(); len(got) != 0 {
		t.Errorf("events = %v, want none", got)
	}
	if fake.attemptCalls != 0 {
		t.Errorf("read the payment attempt %d times, want none", fake.attemptCalls)
	}
	var stored billingv1alpha1.Invoice
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(&obj), &stored); err != nil {
		t.Fatal(err)
	}
	if got := stored.Annotations[PaymentAttemptsReportedAnnotation]; got != "3" {
		t.Errorf("%s = %q, want 3", PaymentAttemptsReportedAnnotation, got)
	}
}

func TestReconcileInvoices_OpenMeterInvoiceEvents(t *testing.T) {
	issued := omInvoice("01INV", om.InvoiceStatusIssued, "12.50")
	r, _, account := newInvoiceSyncReconciler(t, []om.Invoice{issued})
	rec := &timelineRecorder{}
	r.Recorder = rec
	syncInvoices(t, r, account, 2)

	r.OpenMeterClient = &invoiceListOpenMeterClient{invoices: []om.Invoice{omInvoice("01INV", om.InvoiceStatusOverdue, "12.50")}}
	syncInvoices(t, r, account, 2)
	r.OpenMeterClient = &invoiceListOpenMeterClient{invoices: []om.Invoice{omInvoice("01INV", om.InvoiceStatusPaid, "12.50")}}
	syncInvoices(t, r, account, 2)

	want := []string{
		billingv1alpha1.EventReasonInvoiceIssued,
		billingv1alpha1.EventReasonInvoicePastDue,
		billingv1alpha1.EventReasonInvoicePaymentSucceeded,
	}
	if got := rec.reasons(); !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	if got := rec.last(billingv1alpha1.EventReasonInvoicePaymentSucceeded).annotations[billingv1alpha1.EventAnnotationAmount]; got != "12.50" {
		t.Errorf("paid amount = %q, want 12.50", got)
	}
}
