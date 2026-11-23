// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
	"github.com/shopspring/decimal"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
)

// The invoice writer: for an account it covers, OpenMeter still works out
// the invoice, but stripe-provider writes it to Stripe, line by line, in
// place of OpenMeter's own Stripe app. See docs/invoice-writer.md.
//
// Each OpenMeter invoice waiting on a Custom Invoicing hook becomes a
// StripeInvoice (purpose Invoice) in the account's namespace:
//
//  1. draft.syncing: its lines are built from OpenMeter's (see
//     invoiceWriterLines) and written to the StripeInvoice. Once
//     stripe-provider reports the Stripe draft Synced, and its totals agree
//     with OpenMeter's, OpenMeter is told the draft is written, with the
//     Stripe invoice and item ids.
//  2. issuing.syncing: the StripeInvoice is marked to finalize. Once Stripe
//     has finalized it, OpenMeter is told it's issued, under Stripe's
//     number.
//  3. Issued: what Stripe reports about payment is passed on to OpenMeter
//     (see paymentTrigger), which no longer learns it from Stripe itself.
const (
	// EventReasonInvoiceWriter is the reason on events about writing an
	// account's invoices.
	EventReasonInvoiceWriter = "InvoiceWriter"
	// EventReasonInvoiceWriterHeld is the reason on events about an
	// invoice the writer won't move on until someone looks at it.
	EventReasonInvoiceWriterHeld = "InvoiceWriterHeld"

	// StripeInvoicePaymentTriggerAnnotation records, on a StripeInvoice,
	// the last payment change passed on to OpenMeter, so each is sent once.
	StripeInvoicePaymentTriggerAnnotation = "openmeter.miloapis.com/payment-trigger"
	// stripeInvoiceNamePrefix starts the name of the StripeInvoice for an
	// OpenMeter invoice: the prefix, then its id in lower case.
	stripeInvoiceNamePrefix = "openmeter-"

	// invoiceWriterRequeue is how soon an account with an invoice waiting
	// on the writer is looked at again, besides the StripeInvoice watch.
	invoiceWriterRequeue = time.Minute
	// invoiceWriterStuckAfter is how long an invoice waits on a hook before
	// the writer warns about it: nothing in OpenMeter times it out.
	invoiceWriterStuckAfter = time.Hour

	// lineMetadataServicePricing and lineMetadataOpenMeterLine go on each
	// Stripe line, as datum_service_pricing and datum_om_line.
	lineMetadataServicePricing = "service_pricing"
	lineMetadataOpenMeterLine  = "om_line"
)

// InvoiceWriter is the invoice writer's configuration for
// BillingAccountReconciler. A nil one covers no account.
type InvoiceWriter struct {
	// AppName is the Custom Invoicing app's name in OpenMeter.
	AppName string
	// Selector picks the accounts covered. Nil covers every account.
	Selector labels.Selector
	// CustomChargePricing is the ServicePricing, in milo-system, a charge
	// without a catalog item is invoiced under.
	CustomChargePricing string

	mu    sync.Mutex
	appID string
}

// covers reports whether the writer writes account's invoices.
func (w *InvoiceWriter) covers(account *billingv1alpha1.BillingAccount) bool {
	if w == nil {
		return false
	}
	return w.Selector == nil || w.Selector.Matches(labels.Set(account.Labels))
}

// app returns the Custom Invoicing app's id, installing it the first time.
func (w *InvoiceWriter) app(ctx context.Context, omc openmeter.Client) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.appID != "" {
		return w.appID, nil
	}
	id, err := omc.EnsureCustomInvoicingApp(ctx, w.AppName)
	if err != nil {
		return "", fmt.Errorf("ensure Custom Invoicing app %q: %w", w.AppName, err)
	}
	w.appID = id
	return id, nil
}

// wrote reports whether inv was invoiced through the writer's app. It's
// false until the app's id is known, on the first account reconciled.
func (w *InvoiceWriter) wrote(inv om.Invoice) bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.appID != "" && openmeter.InvoiceInvoicingApp(inv) == w.appID
}

// invoiceWriterApp is the Custom Invoicing app's id when the writer covers
// account, or "" when it doesn't.
func (r *BillingAccountReconciler) invoiceWriterApp(ctx context.Context, account *billingv1alpha1.BillingAccount) (string, error) {
	if !r.InvoiceWriter.covers(account) {
		return "", nil
	}
	return r.InvoiceWriter.app(ctx, r.OpenMeterClient)
}

// reconcileInvoiceWriter runs once the account's billing profile is on
// appID ("" when the writer doesn't cover it): it reports whether the
// writer has the account, then moves each of its invoices waiting on a
// hook along. It reports whether any invoice is still waiting.
func (r *BillingAccountReconciler) reconcileInvoiceWriter(
	ctx context.Context,
	account *billingv1alpha1.BillingAccount,
	customerID openmeter.CustomerID,
	appID string,
) (bool, error) {
	if err := r.setInvoiceWriterReady(ctx, account, appID != ""); err != nil {
		return false, err
	}
	if appID == "" {
		forgetInvoiceWriterMetrics(account.Namespace, account.Name)
		return false, nil
	}

	invoices, err := r.OpenMeterClient.ListInvoicesAwaitingSync(ctx, customerID)
	if err != nil {
		return false, fmt.Errorf("list invoices awaiting sync: %w", err)
	}
	var errs []error
	waiting := false
	heldCount, stuckCount := 0, 0
	for _, inv := range invoices {
		if openmeter.InvoiceInvoicingApp(inv) != appID {
			// Cut while the account was on another profile.
			continue
		}
		waiting = true
		var h heldError
		switch err := r.syncInvoiceToWriter(ctx, account, inv); {
		case errors.As(err, &h):
			heldCount++
			r.holdInvoice(account, inv, h.reason)
		case err != nil:
			errs = append(errs, fmt.Errorf("invoice %s: %w", inv.Id, err))
		}
		if r.warnIfStuck(account, inv) {
			stuckCount++
		}
	}
	if err := r.reportPayments(ctx, account); err != nil {
		errs = append(errs, err)
	}
	// Events expire and are sent once: the gauges stay, to alert on.
	invoiceWriterHeldInvoices.WithLabelValues(account.Namespace, account.Name).Set(float64(heldCount))
	invoiceWriterStuckInvoices.WithLabelValues(account.Namespace, account.Name).Set(float64(stuckCount))
	return waiting, errors.Join(errs...)
}

// heldError is an invoice the writer won't move on until someone looks at
// it; reason says why.
type heldError struct{ reason string }

func (e heldError) Error() string { return "held: " + e.reason }

func heldInvoice(reason string) error { return heldError{reason: reason} }

// setInvoiceWriterReady sets the account's InvoiceWriterReady condition:
// True once its invoices are written by the writer, False if it was and
// isn't any more. An account never on the writer is left without one.
func (r *BillingAccountReconciler) setInvoiceWriterReady(ctx context.Context, account *billingv1alpha1.BillingAccount, ready bool) error {
	cond := metav1.Condition{
		Type:               billingv1alpha1.BillingAccountConditionInvoiceWriterReady,
		Status:             metav1.ConditionTrue,
		Reason:             "OnInvoiceWriter",
		Message:            "The account's invoices are written to Stripe by the invoice writer",
		ObservedGeneration: account.Generation,
	}
	if !ready {
		if apimeta.FindStatusCondition(account.Status.Conditions, cond.Type) == nil {
			return nil
		}
		cond.Status, cond.Reason = metav1.ConditionFalse, "NotOnInvoiceWriter"
		cond.Message = "The account's invoices are written by OpenMeter's Stripe app"
	}
	if current := apimeta.FindStatusCondition(account.Status.Conditions, cond.Type); current != nil &&
		current.Status == cond.Status && current.Reason == cond.Reason && current.ObservedGeneration == cond.ObservedGeneration {
		return nil
	}
	base := account.DeepCopy()
	apimeta.SetStatusCondition(&account.Status.Conditions, cond)
	// Optimistic: billing writes the account's status too, and a patch
	// from a stale copy mustn't undo its conditions.
	if err := r.Status().Patch(ctx, account, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("set %s: %w", cond.Type, err)
	}
	return nil
}

// syncInvoiceToWriter moves one invoice waiting on a hook along.
func (r *BillingAccountReconciler) syncInvoiceToWriter(ctx context.Context, account *billingv1alpha1.BillingAccount, inv om.Invoice) error {
	lines, held, err := r.invoiceWriterLines(ctx, inv)
	if err != nil {
		return err
	}
	if held != "" {
		return heldInvoice(held)
	}

	key := types.NamespacedName{Namespace: account.Namespace, Name: stripeInvoiceNameFor(inv.Id)}
	var si stripev1alpha1.StripeInvoice
	err = r.Get(ctx, key, &si)
	switch {
	case apierrors.IsNotFound(err):
		if len(lines) == 0 {
			return r.deleteEmptyInvoice(ctx, account, inv)
		}
		si = r.desiredStripeInvoice(account, inv, lines)
		if err := controllerutil.SetOwnerReference(account, &si, r.Scheme()); err != nil {
			return fmt.Errorf("own StripeInvoice: %w", err)
		}
		if err := r.Create(ctx, &si); err != nil {
			return fmt.Errorf("create StripeInvoice %s: %w", key.Name, err)
		}
		r.event(account, "Normal", EventReasonInvoiceWriter, "Writing OpenMeter invoice %s to Stripe as StripeInvoice %s", inv.Id, key.Name)
		return nil
	case err != nil:
		return fmt.Errorf("get StripeInvoice %s: %w", key.Name, err)
	}

	// Semantic: times come back from the API server as whole seconds in
	// UTC, which reflect.DeepEqual would count as a change every pass.
	if !equality.Semantic.DeepEqual(si.Spec.Lines, lines) {
		if si.Spec.Finalize {
			return heldInvoice(fmt.Sprintf(
				"its lines changed in OpenMeter after StripeInvoice %s was marked to finalize; Stripe keeps the lines it was written with", si.Name))
		}
		if len(lines) == 0 {
			return heldInvoice("it has nothing left to bill, but its Stripe draft was already written")
		}
		base := si.DeepCopy()
		si.Spec.Lines = lines
		if err := r.Patch(ctx, &si, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("update StripeInvoice %s's lines: %w", si.Name, err)
		}
		return nil
	}

	switch inv.StatusDetails.ExtendedStatus {
	case openmeter.InvoiceExtendedStatusDraftSyncing:
		return r.draftSynchronized(ctx, account, inv, &si)
	case openmeter.InvoiceExtendedStatusIssuingSyncing:
		return r.issuingSynchronized(ctx, account, inv, &si)
	}
	return nil
}

// draftSynchronized tells OpenMeter the invoice's Stripe draft is written,
// once stripe-provider says so for the current lines and Stripe's totals
// before tax agree with OpenMeter's.
func (r *BillingAccountReconciler) draftSynchronized(ctx context.Context, account *billingv1alpha1.BillingAccount, inv om.Invoice, si *stripev1alpha1.StripeInvoice) error {
	if !stripeInvoiceSynced(si) {
		return nil
	}
	if msg := totalsDiffer(inv, si); msg != "" {
		return heldInvoice(msg)
	}
	mapping := make([]openmeter.LineExternalID, 0, len(si.Status.Lines))
	for _, l := range si.Status.Lines {
		if l.InvoiceItemID == "" {
			return nil
		}
		mapping = append(mapping, openmeter.LineExternalID{LineID: l.Key, ExternalID: l.InvoiceItemID})
	}
	sort.Slice(mapping, func(i, j int) bool { return mapping[i].LineID < mapping[j].LineID })
	if err := r.OpenMeterClient.DraftSynchronized(ctx, inv.Id, si.Status.InvoiceID, mapping); err != nil {
		return err
	}
	r.event(account, "Normal", EventReasonInvoiceWriter, "OpenMeter invoice %s is written to Stripe as %s", inv.Id, si.Status.InvoiceID)
	return nil
}

// issuingSynchronized has the invoice finalized in Stripe, then tells
// OpenMeter it's issued, under Stripe's number.
func (r *BillingAccountReconciler) issuingSynchronized(ctx context.Context, account *billingv1alpha1.BillingAccount, inv om.Invoice, si *stripev1alpha1.StripeInvoice) error {
	if !si.Spec.Finalize {
		if !stripeInvoiceSynced(si) {
			return nil
		}
		// Checked again: the lines may have changed since the draft step,
		// with late usage for instance, and a difference finalized now is
		// charged.
		if msg := totalsDiffer(inv, si); msg != "" {
			return heldInvoice(msg)
		}
		base := si.DeepCopy()
		si.Spec.Finalize = true
		if err := r.Patch(ctx, si, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("finalize StripeInvoice %s: %w", si.Name, err)
		}
		return nil
	}
	if !apimeta.IsStatusConditionTrue(si.Status.Conditions, stripev1alpha1.StripeInvoiceConditionFinalized) || si.Status.Number == "" {
		return nil
	}
	var sentAt *time.Time
	if si.Status.FinalizedAt != nil {
		t := si.Status.FinalizedAt.Time
		sentAt = &t
	}
	if err := r.OpenMeterClient.IssuingSynchronized(ctx, inv.Id, si.Status.Number, si.Status.InvoiceID, sentAt); err != nil {
		return err
	}
	r.event(account, "Normal", EventReasonInvoiceWriter, "OpenMeter invoice %s is issued in Stripe as %s", inv.Id, si.Status.Number)
	return nil
}

// stripeInvoiceSynced reports whether the Stripe draft holds the
// StripeInvoice's current lines.
func stripeInvoiceSynced(si *stripev1alpha1.StripeInvoice) bool {
	cond := apimeta.FindStatusCondition(si.Status.Conditions, stripev1alpha1.StripeInvoiceConditionSynced)
	return cond != nil && cond.Status == metav1.ConditionTrue &&
		si.Status.ObservedGeneration == si.Generation && si.Status.InvoiceID != ""
}

// totalsDiffer compares Stripe's total before tax with OpenMeter's, after
// discounts, and explains a difference.
func totalsDiffer(inv om.Invoice, si *stripev1alpha1.StripeInvoice) string {
	stripeTotal, err1 := decimal.NewFromString(si.Status.Subtotal)
	stripeDiscount, err2 := decimal.NewFromString(orZero(si.Status.Discount))
	omAmount, err3 := decimal.NewFromString(inv.Totals.Amount)
	omDiscount, err4 := decimal.NewFromString(orZero(inv.Totals.DiscountsTotal))
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return fmt.Sprintf("its totals can't be compared with Stripe's: %v", err)
	}
	stripeNet, omNet := stripeTotal.Sub(stripeDiscount), omAmount.Sub(omDiscount)
	if stripeNet.Equal(omNet) {
		return ""
	}
	return fmt.Sprintf("Stripe's total before tax, %s, differs from OpenMeter's, %s", stripeNet.StringFixed(2), omNet.StringFixed(2))
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

// deleteEmptyInvoice deletes an invoice with nothing on it to bill: Stripe
// needs at least one line, and nothing is owed.
func (r *BillingAccountReconciler) deleteEmptyInvoice(ctx context.Context, account *billingv1alpha1.BillingAccount, inv om.Invoice) error {
	if inv.StatusDetails.ExtendedStatus != openmeter.InvoiceExtendedStatusDraftSyncing {
		return heldInvoice("it has nothing to bill and is already being issued")
	}
	if err := r.OpenMeterClient.DeleteInvoice(ctx, inv.Id); err != nil {
		return fmt.Errorf("delete empty invoice: %w", err)
	}
	r.event(account, "Normal", EventReasonInvoiceWriter, "Deleted OpenMeter invoice %s: it has nothing to bill", inv.Id)
	return nil
}

// desiredStripeInvoice is the StripeInvoice that writes inv.
func (r *BillingAccountReconciler) desiredStripeInvoice(account *billingv1alpha1.BillingAccount, inv om.Invoice, lines []stripev1alpha1.StripeInvoiceLine) stripev1alpha1.StripeInvoice {
	return stripev1alpha1.StripeInvoice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      stripeInvoiceNameFor(inv.Id),
			Namespace: account.Namespace,
			Labels:    map[string]string{StripeInvoiceAccountLabel: account.Name},
		},
		Spec: stripev1alpha1.StripeInvoiceSpec{
			BillingAccountRef: stripev1alpha1.BillingAccountLocalRef{Name: account.Name},
			Purpose:           stripev1alpha1.StripeInvoicePurposeInvoice,
			Currency:          string(inv.Currency),
			Lines:             lines,
			Source: &stripev1alpha1.StripeInvoiceSource{
				System:    stripev1alpha1.StripeInvoiceSourceOpenMeter,
				InvoiceID: inv.Id,
			},
		},
	}
}

// StripeInvoiceAccountLabel names, on a StripeInvoice the writer made, the
// BillingAccount it's for.
const StripeInvoiceAccountLabel = "openmeter.miloapis.com/billing-account"

// stripeInvoiceNameFor is the name of the StripeInvoice for OpenMeter
// invoice id.
func stripeInvoiceNameFor(id string) string {
	return stripeInvoiceNamePrefix + strings.ToLower(id)
}

// holdInvoice reports an invoice the writer won't move on, once while
// the reason stays the same.
func (r *BillingAccountReconciler) holdInvoice(account *billingv1alpha1.BillingAccount, inv om.Invoice, why string) {
	key := noticePrefix(client.ObjectKeyFromObject(account)) + EventReasonInvoiceWriterHeld + "/" + inv.Id
	if r.notices.changed(key, why) {
		r.event(account, "Warning", EventReasonInvoiceWriterHeld, "OpenMeter invoice %s is held: %s", inv.Id, why)
	}
}

// warnIfStuck warns, once, about an invoice that has waited on a hook for
// longer than invoiceWriterStuckAfter, and reports whether it has.
func (r *BillingAccountReconciler) warnIfStuck(account *billingv1alpha1.BillingAccount, inv om.Invoice) bool {
	since := r.now().Sub(inv.UpdatedAt)
	if since > invoiceWriterStuckAfter {
		key := noticePrefix(client.ObjectKeyFromObject(account)) + "InvoiceWriterStuck/" + inv.Id
		if r.notices.changed(key, inv.StatusDetails.ExtendedStatus) {
			r.event(account, "Warning", EventReasonInvoiceWriterHeld,
				"OpenMeter invoice %s has been in %s since %s", inv.Id, inv.StatusDetails.ExtendedStatus, inv.UpdatedAt.UTC().Format(time.RFC3339))
		}
	}
	return since > invoiceWriterStuckAfter
}

func (r *BillingAccountReconciler) event(account *billingv1alpha1.BillingAccount, kind, reason, format string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(account, kind, reason, format, args...)
	}
}

// reportPayments passes what Stripe reports about the payment of each of
// the account's issued invoices on to OpenMeter.
func (r *BillingAccountReconciler) reportPayments(ctx context.Context, account *billingv1alpha1.BillingAccount) error {
	var list stripev1alpha1.StripeInvoiceList
	if err := r.List(ctx, &list, client.InNamespace(account.Namespace),
		client.MatchingLabels{StripeInvoiceAccountLabel: account.Name}); err != nil {
		return fmt.Errorf("list the account's StripeInvoices: %w", err)
	}
	var errs []error
	for i := range list.Items {
		si := &list.Items[i]
		if si.Spec.Source == nil || si.Spec.Source.System != stripev1alpha1.StripeInvoiceSourceOpenMeter {
			continue
		}
		trigger := paymentTrigger(si, r.now())
		if trigger == "" || si.Annotations[StripeInvoicePaymentTriggerAnnotation] == string(trigger) {
			continue
		}
		err := r.OpenMeterClient.UpdatePaymentStatus(ctx, si.Spec.Source.InvoiceID, trigger)
		if err != nil && !openmeter.IsPermanent(err) {
			errs = append(errs, fmt.Errorf("invoice %s: %w", si.Spec.Source.InvoiceID, err))
			continue
		}
		if err != nil {
			// OpenMeter won't take it from the state it has: say so once,
			// rather than on every pass.
			r.event(account, "Warning", EventReasonInvoiceWriterHeld,
				"OpenMeter refused payment status %s for invoice %s: %v", trigger, si.Spec.Source.InvoiceID, err)
		}
		base := si.DeepCopy()
		if si.Annotations == nil {
			si.Annotations = map[string]string{}
		}
		si.Annotations[StripeInvoicePaymentTriggerAnnotation] = string(trigger)
		if err := r.Patch(ctx, si, client.MergeFrom(base)); err != nil {
			errs = append(errs, fmt.Errorf("record payment status on %s: %w", si.Name, err))
		}
	}
	return errors.Join(errs...)
}

// paymentTrigger is what OpenMeter should know about an issued invoice's
// payment, from its StripeInvoice, or "" while there's nothing to say.
func paymentTrigger(si *stripev1alpha1.StripeInvoice, now time.Time) openmeter.PaymentTrigger {
	switch si.Status.State {
	case stripev1alpha1.StripeInvoiceStatePaid:
		return openmeter.PaymentTriggerPaid
	case stripev1alpha1.StripeInvoiceStateVoid:
		return openmeter.PaymentTriggerVoid
	case stripev1alpha1.StripeInvoiceStateUncollectible:
		return openmeter.PaymentTriggerUncollectible
	case stripev1alpha1.StripeInvoiceStateOpen:
		if si.Status.DueDate != nil && now.After(si.Status.DueDate.Time) {
			return openmeter.PaymentTriggerOverdue
		}
		if apimeta.IsStatusConditionTrue(si.Status.Conditions, stripev1alpha1.StripeInvoiceConditionPaymentFailed) {
			return openmeter.PaymentTriggerFailed
		}
	}
	return ""
}

// mapStripeInvoiceToAccount enqueues the account a writer's StripeInvoice
// is for, so stripe-provider's progress moves its invoice on at once.
func mapStripeInvoiceToAccount(_ context.Context, obj client.Object) []reconcile.Request {
	name := obj.GetLabels()[StripeInvoiceAccountLabel]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}
