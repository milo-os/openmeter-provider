// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	om "github.com/openmeterio/openmeter/api/client/go"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/stripe"
)

// Metadata this provider writes onto every Stripe invoice it projects, so
// staff can search Stripe for the invoice they see in the portal.
const (
	StripeMetadataMiloInvoice        = "milo_invoice"
	StripeMetadataMiloBillingAccount = "milo_billing_account"
)

// invoiceReasonNotFoundInStripe: the Stripe invoice a Milo Invoice was
// projected from is no longer listed for the account's Stripe customer (the
// account moved to another customer, say). The phase is left as it was.
const invoiceReasonNotFoundInStripe = "NotFoundInStripe"

// listStripeInvoices lists the invoices of the account's Stripe customer,
// and reports whether it did: false, with no error, while the account has
// no Stripe customer yet.
func (r *BillingAccountReconciler) listStripeInvoices(
	ctx context.Context,
	account *billingv1alpha1.BillingAccount,
) ([]stripe.Invoice, bool, error) {
	customer, err := r.resolveStripeCustomer(ctx, account)
	if err != nil {
		return nil, false, fmt.Errorf("resolve stripe customer for invoices: %w", err)
	}
	if customer.CustomerID == "" {
		return nil, false, nil
	}
	invoices, err := r.Stripe.ListCustomerInvoices(ctx, string(customer.CustomerID))
	if err != nil {
		return nil, false, err
	}
	return invoices, true, nil
}

// reconcileStripeInvoices brings what staff do in Stripe into Milo, given
// every invoice of the account's Stripe customer:
//
//   - An invoice created in Stripe rather than by OpenMeter (a one-off
//     charge, or the replacement for an invoice voided in Stripe) gets its
//     own Milo Invoice, projected from Stripe, so it shows in the portals
//     and counts towards the account's standing like any other. OpenMeter
//     ignores these: it only follows Stripe invoices it created. Drafts are
//     left until they are finalized.
//   - Every Stripe invoice with a Milo Invoice is given the Milo Invoice's
//     name in its metadata (StripeMetadataMiloInvoice).
//
// invoices are OpenMeter's, to tell its Stripe invoices apart, and
// stripeOnly the existing projections of invoices created in Stripe, by
// Stripe invoice id.
func (r *BillingAccountReconciler) reconcileStripeInvoices(
	ctx context.Context,
	account *billingv1alpha1.BillingAccount,
	invoices []om.Invoice,
	stripeInvoices []stripe.Invoice,
	stripeOnly map[string]*billingv1alpha1.Invoice,
) []error {
	createdByOpenMeter := openMeterCreated(invoices)

	// Oldest first, as for OpenMeter's invoices.
	sorted := append([]stripe.Invoice(nil), stripeInvoices...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].Created.Equal(sorted[j].Created) {
			return sorted[i].Created.Before(sorted[j].Created)
		}
		return sorted[i].ID < sorted[j].ID
	})

	var errs []error
	listed := make(map[string]bool, len(sorted))
	for i := range sorted {
		inv := &sorted[i]
		listed[inv.ID] = true
		if createdByOpenMeter(inv) {
			continue
		}
		if _, ok := stripeInvoicePhase(inv, time.Now()); !ok {
			continue
		}
		var err error
		if existing := stripeOnly[inv.ID]; existing != nil {
			err = r.patchStripeInvoiceStatus(ctx, existing, account, inv)
		} else {
			err = r.createStripeInvoice(ctx, account, inv)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("upsert Stripe invoice %s: %w", inv.ID, err))
		}
	}
	for id, obj := range stripeOnly {
		if listed[id] {
			continue
		}
		if err := r.markStripeInvoiceNotFound(ctx, obj, id); err != nil {
			errs = append(errs, err)
		}
	}

	r.writeMiloInvoiceNames(ctx, account, sorted)
	return errs
}

// openMeterCreated reports whether a Stripe invoice is one OpenMeter
// issued: one of its invoices names it, or it carries the id of one of its
// invoices that doesn't name its Stripe invoice yet (OpenMeter is still
// issuing it). OpenMeter's metadata alone isn't enough: Stripe copies an
// invoice's metadata to its revision, which is staff's invoice, not
// OpenMeter's.
func openMeterCreated(invoices []om.Invoice) func(*stripe.Invoice) bool {
	issued := map[string]bool{}
	issuing := map[string]bool{}
	for _, inv := range invoices {
		if id := openMeterStripeInvoiceID(inv); id != "" {
			issued[id] = true
		} else {
			issuing[inv.Id] = true
		}
	}
	return func(inv *stripe.Invoice) bool {
		return issued[inv.ID] || issuing[inv.Metadata[stripe.MetadataOpenMeterInvoiceID]]
	}
}

// stripeInvoicePhase maps a Stripe invoice's status to a Milo phase, and
// reports whether an invoice in that status is projected at all:
//
//	draft               → not projected (still being written)
//	open                → Open, or PastDue once its due date has passed
//	paid                → Paid
//	void, uncollectible → Void (as for OpenMeter's invoices)
//
// Stripe has no overdue status of its own, so PastDue comes from the due
// date, which only invoices sent for payment have.
func stripeInvoicePhase(inv *stripe.Invoice, now time.Time) (billingv1alpha1.InvoicePhase, bool) {
	switch inv.Status {
	case stripe.InvoiceStatusOpen:
		if inv.DueDate != nil && now.After(*inv.DueDate) {
			return billingv1alpha1.InvoicePhasePastDue, true
		}
		return billingv1alpha1.InvoicePhaseOpen, true
	case stripe.InvoiceStatusPaid:
		return billingv1alpha1.InvoicePhasePaid, true
	case stripe.InvoiceStatusVoid, stripe.InvoiceStatusUncollectible:
		return billingv1alpha1.InvoicePhaseVoid, true
	}
	return "", false
}

// stripeInvoicePeriod is a Stripe invoice's period as Milo's InvoicePeriod,
// with the end made inclusive as for OpenMeter's (see invoicePeriod). An
// invoice created in Stripe usually has a period of an instant, its
// creation; one without a period uses its creation time.
func stripeInvoicePeriod(inv *stripe.Invoice) billingv1alpha1.InvoicePeriod {
	start := inv.PeriodStart
	if start.IsZero() {
		start = inv.Created
	}
	start = start.Truncate(time.Second)
	end := start
	if last := inv.PeriodEnd.Add(-time.Nanosecond).Truncate(time.Second); last.After(start) {
		end = last
	}
	return billingv1alpha1.InvoicePeriod{Start: metav1.NewTime(start), End: metav1.NewTime(end)}
}

func (r *BillingAccountReconciler) createStripeInvoice(
	ctx context.Context,
	account *billingv1alpha1.BillingAccount,
	inv *stripe.Invoice,
) error {
	period := stripeInvoicePeriod(inv)
	name := invoiceObjectName(account.Name, period.Start.Time, inv.ID)
	obj := &billingv1alpha1.Invoice{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   account.Namespace,
			Annotations: map[string]string{StripeInvoiceKeyAnnotation: inv.ID},
			// As for OpenMeter's invoices (see createInvoice).
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         billingv1alpha1.GroupVersion.String(),
				Kind:               "BillingAccount",
				Name:               account.Name,
				UID:                account.UID,
				Controller:         ptr.To(false),
				BlockOwnerDeletion: ptr.To(false),
			}},
		},
		Spec: billingv1alpha1.InvoiceSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: account.Name},
			Period:            period,
		},
	}
	if err := r.Create(ctx, obj); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create Invoice %s/%s: %w", account.Namespace, name, err)
		}
		var existing billingv1alpha1.Invoice
		if getErr := r.Get(ctx, types.NamespacedName{Name: name, Namespace: account.Namespace}, &existing); getErr != nil {
			return fmt.Errorf("create race re-get Invoice %s/%s: %w", account.Namespace, name, getErr)
		}
		if existing.Annotations[InvoiceKeyAnnotation] != "" || existing.Annotations[StripeInvoiceKeyAnnotation] != inv.ID {
			return fmt.Errorf("stripe invoice %s maps to Invoice %s/%s, which projects another invoice",
				inv.ID, account.Namespace, name)
		}
		obj = &existing
	}
	return r.patchStripeInvoiceStatus(ctx, obj, account, inv)
}

// patchStripeInvoiceStatus projects a Stripe invoice onto obj, the way
// patchInvoiceStatus does an OpenMeter invoice.
func (r *BillingAccountReconciler) patchStripeInvoiceStatus(
	ctx context.Context,
	obj *billingv1alpha1.Invoice,
	account *billingv1alpha1.BillingAccount,
	inv *stripe.Invoice,
) error {
	now := time.Now()
	refreshLink := inv.HostedInvoiceURL != "" && documentURIDue(obj, now)
	annotations := map[string]string{}
	addStripeInvoiceAnnotations(annotations, inv)
	if refreshLink {
		annotations[InvoiceDocumentFetchedAtAnnotation] = now.UTC().Format(time.RFC3339)
	}
	if err := r.patchInvoiceAnnotations(ctx, obj, annotations); err != nil {
		return err
	}

	statusBase := obj.DeepCopy()
	projectStripeInvoiceStatus(obj, account, inv, now)
	if refreshLink {
		obj.Status.DocumentURI = inv.HostedInvoiceURL
	}
	if err := r.Status().Patch(ctx, obj, client.MergeFrom(statusBase)); err != nil {
		return fmt.Errorf("patch Invoice status %s/%s: %w", obj.Namespace, obj.Name, err)
	}
	return nil
}

// projectStripeInvoiceStatus writes a Stripe invoice's state onto
// obj.Status, as projectInvoiceStatus does an OpenMeter invoice's. Amounts
// are Stripe's: amountDue is what's left to pay after payments and credit
// notes, nothing for a void invoice, and the unpaid balance for one marked
// uncollectible.
func projectStripeInvoiceStatus(obj *billingv1alpha1.Invoice, account *billingv1alpha1.BillingAccount, inv *stripe.Invoice, now time.Time) {
	phase, ok := stripeInvoicePhase(inv, now)
	if !ok {
		return
	}
	obj.Status.Phase = phase
	obj.Status.CurrencyCode = inv.Currency
	obj.Status.Total = stripe.FormatAmount(inv.Total, inv.Currency)
	obj.Status.AmountPaid = stripe.FormatAmount(inv.AmountPaid, inv.Currency)
	obj.Status.AmountDue = stripe.FormatAmount(inv.AmountRemaining, inv.Currency)
	if inv.Status == stripe.InvoiceStatusVoid || inv.Status == stripe.InvoiceStatusPaid {
		obj.Status.AmountDue = "0"
	}
	obj.Status.ObservedGeneration = obj.Generation

	obj.Status.DueDate = nil
	if inv.DueDate != nil {
		obj.Status.DueDate = &metav1.Time{Time: *inv.DueDate}
	}
	obj.Status.PaidAt = nil
	if phase == billingv1alpha1.InvoicePhasePaid {
		paidAt := now
		if inv.PaidAt != nil {
			paidAt = *inv.PaidAt
		}
		obj.Status.PaidAt = &metav1.Time{Time: paidAt}
	}

	ready := metav1.Condition{
		Type:               billingv1alpha1.InvoiceConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             string(phase),
		Message:            fmt.Sprintf("Invoice created in Stripe (%s), projected with status %s", stripeInvoiceLabel(inv), inv.Status),
		ObservedGeneration: obj.Generation,
	}
	if inv.Status == stripe.InvoiceStatusUncollectible {
		ready.Message = fmt.Sprintf("Invoice created in Stripe (%s) and marked uncollectible (written off); amountDue is the unpaid balance", stripeInvoiceLabel(inv))
	}
	if inv.Currency != "" && account.Spec.CurrencyCode != "" && inv.Currency != account.Spec.CurrencyCode {
		ready.Status = metav1.ConditionFalse
		ready.Reason = invoiceReasonCurrencyMismatch
		ready.Message = fmt.Sprintf(
			"invoice currency %q does not match BillingAccount currency %q",
			inv.Currency, account.Spec.CurrencyCode,
		)
	}
	apimeta.SetStatusCondition(&obj.Status.Conditions, ready)
}

// stripeInvoiceLabel names a Stripe invoice by its number, and its id.
func stripeInvoiceLabel(inv *stripe.Invoice) string {
	if inv.Number == "" {
		return inv.ID
	}
	return inv.Number + ", " + inv.ID
}

// markStripeInvoiceNotFound is markInvoiceNotFound for an invoice created
// in Stripe.
func (r *BillingAccountReconciler) markStripeInvoiceNotFound(ctx context.Context, obj *billingv1alpha1.Invoice, stripeInvoiceID string) error {
	statusBase := obj.DeepCopy()
	apimeta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
		Type:               billingv1alpha1.InvoiceConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             invoiceReasonNotFoundInStripe,
		Message:            fmt.Sprintf("Stripe invoice %s is no longer listed for this billing account's Stripe customer", stripeInvoiceID),
		ObservedGeneration: obj.Generation,
	})
	if err := r.Status().Patch(ctx, obj, client.MergeFrom(statusBase)); err != nil {
		return fmt.Errorf("patch Invoice status %s/%s: %w", obj.Namespace, obj.Name, err)
	}
	return nil
}

// writeMiloInvoiceNames gives every finalized Stripe invoice that has a
// Milo Invoice the Milo Invoice's name and billing account in its metadata,
// once. A failure is logged and retried on the next sync: it mustn't hold
// up the invoices themselves.
func (r *BillingAccountReconciler) writeMiloInvoiceNames(
	ctx context.Context,
	account *billingv1alpha1.BillingAccount,
	stripeInvoices []stripe.Invoice,
) {
	logger := log.FromContext(ctx)
	var list billingv1alpha1.InvoiceList
	if err := r.List(ctx, &list, client.InNamespace(account.Namespace)); err != nil {
		logger.Error(err, "listing Invoices to name their Stripe invoices failed")
		return
	}
	names := map[string]string{}
	for i := range list.Items {
		obj := &list.Items[i]
		if id := obj.Annotations[StripeInvoiceKeyAnnotation]; id != "" && obj.Spec.BillingAccountRef.Name == account.Name {
			if current, ok := names[id]; !ok || obj.Name < current {
				names[id] = obj.Name
			}
		}
	}
	for i := range stripeInvoices {
		inv := &stripeInvoices[i]
		name := names[inv.ID]
		if name == "" || inv.Status == stripe.InvoiceStatusDraft ||
			(inv.Metadata[StripeMetadataMiloInvoice] == name && inv.Metadata[StripeMetadataMiloBillingAccount] == account.Name) {
			continue
		}
		if err := r.Stripe.SetInvoiceMetadata(ctx, inv.ID, map[string]string{
			StripeMetadataMiloInvoice:        name,
			StripeMetadataMiloBillingAccount: account.Name,
		}); err != nil {
			logger.Error(err, "writing the Milo invoice's name onto its Stripe invoice failed; retrying next sync",
				"invoice", name, "stripeInvoice", inv.ID)
		}
	}
}
