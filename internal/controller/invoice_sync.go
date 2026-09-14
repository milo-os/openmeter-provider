// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	om "github.com/openmeterio/openmeter/api/client/go"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

// InvoiceKeyAnnotation stores the OpenMeter invoice id on the Milo Invoice
// it was projected from, for support lookup. Mirrors
// amberflo-provider's invoice.InvoiceKeyAnnotation.
const InvoiceKeyAnnotation = "openmeter.miloapis.com/invoiceId"

// mapInvoiceStatus translates an OpenMeter invoice status into a Milo
// InvoicePhase. Considerably simpler than amberflo-provider's MapPhase:
// OpenMeter's own billing engine already computes "overdue" server-side, so
// there is no client-side past-due/grace-period math to do.
//
//	draft, gathering, issuing, issued, payment_processing → Open
//	overdue                                               → PastDue
//	paid                                                  → Paid
//	voided                                                → Void
//	uncollectible                                         → PastDue (closest
//	  Milo phase; Milo has no "written off" concept)
func mapInvoiceStatus(status om.InvoiceStatus) billingv1alpha1.InvoicePhase {
	switch status {
	case om.InvoiceStatusPaid:
		return billingv1alpha1.InvoicePhasePaid
	case om.InvoiceStatusVoided:
		return billingv1alpha1.InvoicePhaseVoid
	case om.InvoiceStatusOverdue, om.InvoiceStatusUncollectible:
		return billingv1alpha1.InvoicePhasePastDue
	default: // draft, gathering, issuing, issued, payment_processing
		return billingv1alpha1.InvoicePhaseOpen
	}
}

// invoiceName builds the deterministic Milo Invoice name
// `<billing-account>-<YYYY>-<MM>` from the invoice's period start (or, for
// an invoice with no line items and therefore no period, its creation
// time) — mirrors amberflo-provider's invoice.InvoiceName.
func invoiceName(accountName string, inv om.Invoice) string {
	t := inv.CreatedAt
	if inv.Period != nil {
		t = inv.Period.From
	}
	return fmt.Sprintf("%s-%04d-%02d", accountName, t.Year(), int(t.Month()))
}

// reconcileInvoices lists OpenMeter's invoices for the customer and upserts
// a Milo Invoice per period. Mirrors amberflo-provider's
// invoice.Syncer.Upsert, adapted to OpenMeter's simpler status model.
// Errors are returned to the caller for classification the same way an
// EnsureCustomer failure would be; an empty invoice list is a healthy
// "NoInvoicesYet" state, not an error.
//
// openMeterCustomerID is OpenMeter's internal customer id (the Id field on
// the om.Customer EnsureCustomer returned in Reconcile) — ListInvoices'
// Customers filter requires it, same caveat as
// UpsertBillingProfileCustomerOverride.
func (r *BillingAccountReconciler) reconcileInvoices(
	ctx context.Context,
	account *billingv1alpha1.BillingAccount,
	openMeterCustomerID openmeter.CustomerID,
) error {
	invoices, err := r.OpenMeterClient.ListInvoices(ctx, openMeterCustomerID)
	if err != nil {
		return fmt.Errorf("list invoices: %w", err)
	}
	for _, inv := range pickInvoicePerPeriod(account.Name, invoices) {
		if err := r.upsertInvoice(ctx, account, inv); err != nil {
			return fmt.Errorf("upsert invoice %s: %w", inv.Id, err)
		}
	}
	return nil
}

// pickInvoicePerPeriod collapses OpenMeter's invoices down to at most one
// per Milo Invoice name, returned in a deterministic order.
//
// Milo Invoice names are `<account>-<YYYY>-<MM>` by CRD convention, but
// OpenMeter can legitimately hold several invoices covering one month — a
// voided invoice plus its reissue, or several progressive invoices (the
// default billing profile ships with progressiveBilling enabled). Upserting
// all of them writes them onto the SAME Invoice object one after another,
// so whichever happened to be processed last won: with ListInvoices'
// newest-first ordering that meant the OLDEST invoice silently overwrote
// the newest, e.g. leaving a superseded Void status masking a live invoice.
//
// Selection prefers a non-voided invoice, then the most recent — so a
// reissue wins over the void it replaced, and a month whose invoices are
// all voided still reports Void rather than vanishing.
func pickInvoicePerPeriod(accountName string, invoices []om.Invoice) []om.Invoice {
	best := make(map[string]om.Invoice, len(invoices))
	names := make([]string, 0, len(invoices))
	for _, inv := range invoices {
		name := invoiceName(accountName, inv)
		current, seen := best[name]
		if !seen {
			names = append(names, name)
			best[name] = inv
			continue
		}
		if preferInvoice(inv, current) {
			best[name] = inv
		}
	}

	sort.Strings(names)
	out := make([]om.Invoice, 0, len(names))
	for _, name := range names {
		out = append(out, best[name])
	}
	return out
}

// preferInvoice reports whether candidate should win over current for the
// same period: a non-voided invoice beats a voided one, otherwise the more
// recently created wins. Ties fall back to the id so the choice is stable
// across reconciles rather than depending on list order.
func preferInvoice(candidate, current om.Invoice) bool {
	candidateVoided := candidate.Status == om.InvoiceStatusVoided
	currentVoided := current.Status == om.InvoiceStatusVoided
	if candidateVoided != currentVoided {
		return currentVoided
	}
	if !candidate.CreatedAt.Equal(current.CreatedAt) {
		return candidate.CreatedAt.After(current.CreatedAt)
	}
	return candidate.Id > current.Id
}

func (r *BillingAccountReconciler) upsertInvoice(
	ctx context.Context,
	account *billingv1alpha1.BillingAccount,
	inv om.Invoice,
) error {
	name := invoiceName(account.Name, inv)
	nn := types.NamespacedName{Name: name, Namespace: account.Namespace}

	var existing billingv1alpha1.Invoice
	err := r.Get(ctx, nn, &existing)
	switch {
	case apierrors.IsNotFound(err):
		return r.createInvoice(ctx, account, inv, name)
	case err != nil:
		return fmt.Errorf("get Invoice %s: %w", nn, err)
	}
	return r.patchInvoiceStatus(ctx, &existing, account, inv)
}

func (r *BillingAccountReconciler) createInvoice(
	ctx context.Context,
	account *billingv1alpha1.BillingAccount,
	inv om.Invoice,
	name string,
) error {
	period := billingv1alpha1.InvoicePeriod{
		Start: metav1.NewTime(inv.CreatedAt),
		End:   metav1.NewTime(inv.CreatedAt),
	}
	if inv.Period != nil {
		period = billingv1alpha1.InvoicePeriod{
			Start: metav1.NewTime(inv.Period.From),
			End:   metav1.NewTime(inv.Period.To),
		}
	}

	obj := &billingv1alpha1.Invoice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: account.Namespace,
			Annotations: map[string]string{
				InvoiceKeyAnnotation: inv.Id,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: billingv1alpha1.GroupVersion.String(),
				Kind:       "BillingAccount",
				Name:       account.Name,
				UID:        account.UID,
				// Non-controller: BillingAccount deletion must not be
				// blocked by still-existing invoices; GC still cascades
				// when the BillingAccount itself is deleted. Matches
				// amberflo-provider's invoice ownership contract.
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
		if apierrors.IsAlreadyExists(err) {
			// Race with a concurrent reconcile: re-fetch and patch status.
			var existing billingv1alpha1.Invoice
			if getErr := r.Get(ctx, types.NamespacedName{Name: name, Namespace: account.Namespace}, &existing); getErr != nil {
				return fmt.Errorf("create race re-get Invoice %s/%s: %w", account.Namespace, name, getErr)
			}
			return r.patchInvoiceStatus(ctx, &existing, account, inv)
		}
		return fmt.Errorf("create Invoice %s/%s: %w", account.Namespace, name, err)
	}
	return r.patchInvoiceStatus(ctx, obj, account, inv)
}

// patchInvoiceStatus projects inv onto obj.status. Spec is immutable and
// never touched here. This provider is the sole writer of Invoice.status
// (see the CRD's own doc comment: "Populated exclusively by the invoicing
// provider"), so a plain read-modify-write is safe — unlike
// BillingAccount.status, which billing's own controller also writes.
func (r *BillingAccountReconciler) patchInvoiceStatus(
	ctx context.Context,
	obj *billingv1alpha1.Invoice,
	account *billingv1alpha1.BillingAccount,
	inv om.Invoice,
) error {
	base := obj.DeepCopy()
	if obj.Annotations == nil {
		obj.Annotations = map[string]string{}
	}
	obj.Annotations[InvoiceKeyAnnotation] = inv.Id
	if err := r.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch Invoice metadata %s/%s: %w", obj.Namespace, obj.Name, err)
	}

	phase := mapInvoiceStatus(inv.Status)
	statusBase := obj.DeepCopy()
	obj.Status.Phase = phase
	obj.Status.CurrencyCode = inv.Currency
	obj.Status.Total = inv.Totals.Total
	if phase == billingv1alpha1.InvoicePhasePaid {
		obj.Status.AmountPaid = inv.Totals.Total
		obj.Status.AmountDue = "0"
	} else {
		obj.Status.AmountPaid = "0"
		obj.Status.AmountDue = inv.Totals.Total
	}
	obj.Status.DocumentURI = ""
	obj.Status.ObservedGeneration = obj.Generation
	if inv.DueAt != nil {
		obj.Status.DueDate = &metav1.Time{Time: *inv.DueAt}
	}
	if phase == billingv1alpha1.InvoicePhasePaid {
		obj.Status.PaidAt = &metav1.Time{Time: inv.UpdatedAt}
	} else {
		obj.Status.PaidAt = nil
	}

	ready := metav1.Condition{
		Type:               billingv1alpha1.InvoiceConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             string(phase),
		Message:            fmt.Sprintf("Invoice projected from OpenMeter with status %s", inv.Status),
		ObservedGeneration: obj.Generation,
	}
	if inv.Currency != "" && account.Spec.CurrencyCode != "" && string(inv.Currency) != account.Spec.CurrencyCode {
		ready.Status = metav1.ConditionFalse
		ready.Reason = "CurrencyMismatch"
		ready.Message = fmt.Sprintf(
			"invoice currency %q does not match BillingAccount currency %q",
			inv.Currency, account.Spec.CurrencyCode,
		)
	}
	apimeta.SetStatusCondition(&obj.Status.Conditions, ready)

	if err := r.Status().Patch(ctx, obj, client.MergeFrom(statusBase)); err != nil {
		return fmt.Errorf("patch Invoice status %s/%s: %w", obj.Namespace, obj.Name, err)
	}
	return nil
}
