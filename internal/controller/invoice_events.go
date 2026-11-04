// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"maps"
	"math/big"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/log"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/stripe"
)

// PaymentAttemptsReportedAnnotation is the number of the Stripe invoice's
// collection attempts this provider has already looked at for the activity
// timeline, so a requeue doesn't record a failed attempt twice.
const PaymentAttemptsReportedAnnotation = "openmeter.miloapis.com/paymentAttemptsReported"

// invoiceEvent is an activity timeline Event to record on an Invoice. The
// reasons, annotation keys and visibility rules are billing's; see
// api/v1alpha1/events.go in go.miloapis.com/billing.
type invoiceEvent struct {
	reason    string
	eventType string
	message   string
	detail    map[string]string
	// staffOnly events are recorded without the organization's scope
	// annotations, so they only reach the platform timeline.
	staffOnly bool
}

// recordInvoiceEvents records events on obj, scoped to its organization
// unless they're staff-only. A nil recorder records nothing.
func recordInvoiceEvents(recorder record.EventRecorder, obj *billingv1alpha1.Invoice, events ...invoiceEvent) {
	if recorder == nil {
		return
	}
	for _, e := range events {
		annotations := map[string]string{}
		if !e.staffOnly {
			maps.Copy(annotations, billingv1alpha1.OrganizationScopeAnnotations(obj.Namespace))
		}
		maps.Copy(annotations, e.detail)
		recorder.AnnotatedEventf(obj, annotations, e.eventType, e.reason, "%s", e.message)
	}
}

// invoiceEventDetail returns the annotations every invoice event carries,
// plus extra key/value pairs whose value isn't empty.
func invoiceEventDetail(obj *billingv1alpha1.Invoice, currency string, extra ...string) map[string]string {
	detail := map[string]string{
		billingv1alpha1.EventAnnotationBillingAccount: obj.Spec.BillingAccountRef.Name,
	}
	if !obj.Spec.Period.Start.IsZero() {
		detail[billingv1alpha1.EventAnnotationPeriodStart] = billingv1alpha1.EventTime(obj.Spec.Period.Start.Time)
	}
	if currency != "" {
		detail[billingv1alpha1.EventAnnotationCurrency] = currency
	}
	for i := 0; i+1 < len(extra); i += 2 {
		if extra[i+1] != "" {
			detail[extra[i]] = extra[i+1]
		}
	}
	return detail
}

// invoiceStatusEvents returns the timeline events for an Invoice whose
// status changed from before to obj.Status. It works from the stored status
// alone, so calling it only after the status write succeeded records each
// transition once: a failed write leaves before in place for the retry, and
// a repeated sync finds nothing changed. Invoices first projected by an
// earlier version of this provider already have a phase, so nothing is
// replayed for them.
func invoiceStatusEvents(obj *billingv1alpha1.Invoice, before billingv1alpha1.InvoiceStatus) []invoiceEvent {
	after := obj.Status
	currency := after.CurrencyCode
	var events []invoiceEvent

	if before.Phase == "" && after.Phase != "" && after.Phase != billingv1alpha1.InvoicePhaseVoid {
		due := ""
		if after.DueDate != nil {
			due = billingv1alpha1.EventTime(after.DueDate.Time)
		}
		events = append(events, invoiceEvent{
			reason:    billingv1alpha1.EventReasonInvoiceIssued,
			eventType: corev1.EventTypeNormal,
			message:   "Invoice issued.",
			detail: invoiceEventDetail(obj, currency,
				billingv1alpha1.EventAnnotationAmount, after.Total,
				billingv1alpha1.EventAnnotationDueDate, due),
		})
	}

	if after.Phase != before.Phase {
		switch after.Phase {
		case billingv1alpha1.InvoicePhasePaid:
			amount := after.AmountPaid
			if !positive(amount) {
				amount = after.Total
			}
			events = append(events, invoiceEvent{
				reason:    billingv1alpha1.EventReasonInvoicePaymentSucceeded,
				eventType: corev1.EventTypeNormal,
				message:   "Invoice paid.",
				detail:    invoiceEventDetail(obj, currency, billingv1alpha1.EventAnnotationAmount, amount),
			})
		case billingv1alpha1.InvoicePhasePastDue:
			events = append(events, invoiceEvent{
				reason:    billingv1alpha1.EventReasonInvoicePastDue,
				eventType: corev1.EventTypeWarning,
				message:   "Invoice is past due.",
				detail:    invoiceEventDetail(obj, currency, billingv1alpha1.EventAnnotationAmountDue, after.AmountDue),
			})
		case billingv1alpha1.InvoicePhaseVoid:
			// An invoice voided before it was ever shown needs no entry.
			if before.Phase != "" {
				events = append(events, invoiceEvent{
					reason:    billingv1alpha1.EventReasonInvoiceVoided,
					eventType: corev1.EventTypeNormal,
					message:   "Invoice voided.",
					detail:    invoiceEventDetail(obj, currency),
				})
			}
		}
	}

	if (after.Phase == billingv1alpha1.InvoicePhaseOpen || after.Phase == billingv1alpha1.InvoicePhasePastDue) &&
		positive(after.AmountPaid) && positive(after.AmountDue) && greater(after.AmountPaid, before.AmountPaid) {
		events = append(events, invoiceEvent{
			reason:    billingv1alpha1.EventReasonInvoicePartiallyPaid,
			eventType: corev1.EventTypeNormal,
			message:   "Invoice partly paid.",
			detail: invoiceEventDetail(obj, currency,
				billingv1alpha1.EventAnnotationAmountPaid, after.AmountPaid,
				billingv1alpha1.EventAnnotationAmountDue, after.AmountDue),
		})
	}

	if readyReason(after) == invoiceReasonCurrencyMismatch && readyReason(before) != invoiceReasonCurrencyMismatch {
		events = append(events, invoiceEvent{
			reason:    billingv1alpha1.EventReasonInvoiceCurrencyMismatch,
			eventType: corev1.EventTypeWarning,
			message:   "Invoice currency does not match the billing account's.",
			detail:    invoiceEventDetail(obj, currency),
			staffOnly: true,
		})
	}
	return events
}

func readyReason(status billingv1alpha1.InvoiceStatus) string {
	c := apimeta.FindStatusCondition(status.Conditions, billingv1alpha1.InvoiceConditionReady)
	if c == nil {
		return ""
	}
	return c.Reason
}

// positive reports whether the decimal string amount is above zero.
func positive(amount string) bool {
	r, ok := new(big.Rat).SetString(amount)
	return ok && r.Sign() > 0
}

// greater reports whether decimal string a is above b; an empty or
// malformed b counts as zero.
func greater(a, b string) bool {
	ra, ok := new(big.Rat).SetString(a)
	if !ok {
		return false
	}
	rb, ok := new(big.Rat).SetString(b)
	if !ok {
		rb = new(big.Rat)
	}
	return ra.Cmp(rb) > 0
}

// paymentAttemptEvent checks inStripe, obj's Stripe invoice, for collection
// attempts this provider hasn't looked at yet. It returns the event to
// record for the latest one, if any, and the value to store in
// PaymentAttemptsReportedAnnotation ("" to leave it as it is, so the next
// sync looks again). The caller stores the value before recording the
// event, so a requeue can't record it twice.
//
// Only an attempt on an invoice that's still open is reported: if the
// invoice is paid or closed, the attempt succeeded or collection stopped,
// which the status events cover.
func (r *BillingAccountReconciler) paymentAttemptEvent(
	ctx context.Context,
	obj *billingv1alpha1.Invoice,
	inStripe *stripe.Invoice,
) (*invoiceEvent, string) {
	if inStripe == nil {
		return nil, ""
	}
	current := strconv.FormatInt(inStripe.AttemptCount, 10)
	stored, hasStored := obj.Annotations[PaymentAttemptsReportedAnnotation]
	if !hasStored && obj.Status.Phase != "" {
		// Projected before this provider reported attempts: start
		// counting now rather than replay old failures.
		return nil, current
	}
	reported, _ := strconv.ParseInt(stored, 10, 64)
	// lookAgain is what to store to decide on the attempts later: nothing
	// for an invoice already being counted, and the count so far for a new
	// one, which would otherwise look like one projected before this
	// provider reported attempts.
	lookAgain := ""
	if !hasStored {
		lookAgain = strconv.FormatInt(reported, 10)
	}
	if inStripe.AttemptCount <= reported {
		return nil, lookAgain
	}
	if inStripe.Status != stripe.InvoiceStatusOpen {
		return nil, current
	}

	attempt, err := r.Stripe.LatestPaymentAttempt(ctx, *inStripe)
	if err != nil {
		log.FromContext(ctx).Error(err, "reading the invoice's latest payment attempt failed; will look again on the next sync",
			"invoice", obj.Name, "stripeInvoice", inStripe.ID)
		return nil, lookAgain
	}
	if attempt != nil {
		switch attempt.Status {
		case stripe.PaymentIntentProcessing:
			// Not decided yet; look again on the next sync.
			return nil, lookAgain
		case stripe.PaymentIntentSucceeded:
			return nil, current
		}
	}

	amount := stripe.FormatAmount(inStripe.AmountRemaining, inStripe.Currency)
	if attempt != nil && attempt.Status == stripe.PaymentIntentRequiresAction {
		return &invoiceEvent{
			reason:    billingv1alpha1.EventReasonInvoicePaymentActionRequired,
			eventType: corev1.EventTypeWarning,
			message:   "Payment needs the card holder to confirm it.",
			detail:    invoiceEventDetail(obj, inStripe.Currency, billingv1alpha1.EventAnnotationAmount, amount),
		}, current
	}
	next := ""
	if inStripe.NextPaymentAttempt != nil {
		next = billingv1alpha1.EventTime(*inStripe.NextPaymentAttempt)
	}
	return &invoiceEvent{
		reason:    billingv1alpha1.EventReasonInvoicePaymentFailed,
		eventType: corev1.EventTypeWarning,
		message:   "Payment failed.",
		detail: invoiceEventDetail(obj, inStripe.Currency,
			billingv1alpha1.EventAnnotationAmount, amount,
			billingv1alpha1.EventAnnotationDeclineReason, string(declineReason(attempt)),
			billingv1alpha1.EventAnnotationNextAttempt, next),
	}, current
}

// declineReason maps Stripe's error on a failed payment onto billing's
// normalized DeclineReason. Stripe's codes never reach the timeline.
func declineReason(attempt *stripe.PaymentAttempt) billingv1alpha1.DeclineReason {
	if attempt == nil {
		return billingv1alpha1.DeclineReasonDeclined
	}
	is := func(codes ...string) bool {
		for _, c := range codes {
			if attempt.ErrorCode == c || attempt.DeclineCode == c {
				return true
			}
		}
		return false
	}
	switch {
	case is("insufficient_funds"):
		return billingv1alpha1.DeclineReasonInsufficientFunds
	case is("expired_card"):
		return billingv1alpha1.DeclineReasonExpiredCard
	case is("authentication_required", "payment_intent_authentication_failure"):
		return billingv1alpha1.DeclineReasonAuthenticationRequired
	case is("processing_error", "issuer_not_available", "try_again_later", "reenter_transaction"),
		attempt.ErrorType == "api_error", attempt.ErrorType == "api_connection_error", attempt.ErrorType == "rate_limit_error":
		return billingv1alpha1.DeclineReasonProcessingError
	}
	return billingv1alpha1.DeclineReasonDeclined
}
