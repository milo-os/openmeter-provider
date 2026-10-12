// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

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
// it was projected from. It is the projection's identity: an existing Milo
// Invoice is matched to its OpenMeter invoice by this annotation, not by
// name (see invoiceName). Mirrors amberflo-provider's
// invoice.InvoiceKeyAnnotation.
const InvoiceKeyAnnotation = "openmeter.miloapis.com/invoiceId"

// Invoice Ready condition reasons this provider sets besides the phase
// names (billing documents Paid and CurrencyMismatch).
const (
	invoiceReasonCurrencyMismatch = "CurrencyMismatch"
	// invoiceReasonNotFoundInOpenMeter: the OpenMeter invoice a Milo
	// Invoice was projected from is no longer listed for the customer,
	// not even among deleted invoices. The phase is left as it was.
	invoiceReasonNotFoundInOpenMeter = "NotFoundInOpenMeter"
)

// mapInvoiceStatus translates an OpenMeter invoice status into a Milo
// InvoicePhase, and reports whether an invoice in that status is projected
// into Milo at all.
//
// Billing defines Open as "issued and payment outstanding", so nothing is
// projected before OpenMeter has issued the invoice: gathering (pending
// lines), draft (still editable, may be deleted), and issuing (finalizing
// with the invoicing app; can still fail and be deleted) are skipped, as
// is delete (a deletion in progress — only pre-issue invoices can be
// deleted). From issued onward:
//
//	issued, payment_processing → Open
//	overdue                    → PastDue
//	paid                       → Paid
//	voided                     → Void
//	uncollectible              → Void
//
// uncollectible is written off: OpenMeter has stopped collecting it, which
// is what billing's Void means ("no longer collectible"). PastDue — the
// previous mapping — would keep the account's InvoicingReady condition
// False forever (billing picks the newest Open/Paid/PastDue invoice and
// reports PastDue for it), with nothing left that could ever clear it.
// Unlike a voided invoice, an uncollectible one still reports its unpaid
// balance as amountDue (see invoiceAmounts), so the write-off stays
// visible.
//
// A deleted invoice (status deleted, or deletedAt set) is handled before
// this mapping: see reconcileInvoices.
func mapInvoiceStatus(status om.InvoiceStatus) (billingv1alpha1.InvoicePhase, bool) {
	switch status {
	case om.InvoiceStatusIssued, om.InvoiceStatusPaymentProcessing:
		return billingv1alpha1.InvoicePhaseOpen, true
	case om.InvoiceStatusOverdue:
		return billingv1alpha1.InvoicePhasePastDue, true
	case om.InvoiceStatusPaid:
		return billingv1alpha1.InvoicePhasePaid, true
	case om.InvoiceStatusVoided, om.InvoiceStatusUncollectible:
		return billingv1alpha1.InvoicePhaseVoid, true
	default: // gathering, draft, issuing, delete, and anything newer
		return "", false
	}
}

// maxInvoiceNameLength is the Kubernetes object name limit (DNS-1123
// subdomain).
const maxInvoiceNameLength = 253

// invoiceNameHashLength is how many hex characters of the invoice id's
// SHA-256 the name carries: 40 bits, so a collision between two invoices
// of one account in one month is not a practical concern (and is detected
// rather than silently merged — see createInvoice).
const invoiceNameHashLength = 10

// invoiceName builds the deterministic Milo Invoice name for one OpenMeter
// invoice: `<billing-account>-<YYYY>-<MM>-<hash>`.
//
// Billing names invoices `<billing-account>-<year>-<month>` so that
// creation is idempotent. That pattern assumes one invoice per account per
// month, which OpenMeter does not guarantee — progressive billing (on by
// default), a subscription ending or changing mid-period, or a void and
// reissue all produce several invoices for one month. Collapsing them onto
// one object either hid all but one of them or let spec.period (immutable)
// describe a different invoice than status. So each OpenMeter invoice gets
// its own Invoice, keeping billing's prefix for readability and sorting and
// appending a short stable hash of the OpenMeter invoice id, which keeps
// creation idempotent (the same invoice always maps to the same name) and
// never renames anything. YYYY-MM is the period start (or, for an invoice
// without a period, its creation time) in UTC.
//
// Objects created under the earlier `<billing-account>-<YYYY>-<MM>`
// scheme keep being updated: projections are matched by
// InvoiceKeyAnnotation, not by name.
//
// The name is capped at maxInvoiceNameLength by shortening the account
// prefix. Uniqueness does not depend on the prefix — the hash is of the
// globally unique OpenMeter invoice id.
func invoiceName(accountName string, inv om.Invoice) string {
	t := invoicePeriodStart(inv).UTC()
	sum := sha256.Sum256([]byte(inv.Id))
	suffix := fmt.Sprintf("-%04d-%02d-%s", t.Year(), int(t.Month()), hex.EncodeToString(sum[:])[:invoiceNameHashLength])
	prefix := accountName
	if len(prefix)+len(suffix) > maxInvoiceNameLength {
		prefix = strings.TrimRight(prefix[:maxInvoiceNameLength-len(suffix)], "-.")
	}
	return prefix + suffix
}

// invoicePeriodStart is the start of inv's billing period, or its creation
// time when it has none (an invoice without line items).
func invoicePeriodStart(inv om.Invoice) time.Time {
	if inv.Period != nil {
		return inv.Period.From
	}
	return inv.CreatedAt
}

// invoicePeriod converts inv's billing period to Milo's InvoicePeriod.
//
// OpenMeter periods are half-open — Period.To is exclusive (the next
// period starts at exactly that instant) — while Milo's period.end is
// documented as inclusive. The inclusive end is the last representable
// instant before To at the precision the field is stored with: metav1.Time
// serializes to whole seconds, so it is To minus one nanosecond, truncated
// to the second (one second before a whole-second To). A period shorter
// than that (or an invoice without a period, which uses its creation time
// for both ends) keeps end == start, satisfying the CRD's
// `self.start <= self.end` rule.
func invoicePeriod(inv om.Invoice) billingv1alpha1.InvoicePeriod {
	start := invoicePeriodStart(inv).Truncate(time.Second)
	end := start
	if inv.Period != nil {
		if last := inv.Period.To.Add(-time.Nanosecond).Truncate(time.Second); last.After(start) {
			end = last
		}
	}
	return billingv1alpha1.InvoicePeriod{
		Start: metav1.NewTime(start),
		End:   metav1.NewTime(end),
	}
}

// reconcileInvoices projects OpenMeter's invoices for the customer onto Milo
// Invoices: one Invoice per issued OpenMeter invoice (see invoiceName and
// mapInvoiceStatus), plus closing out projections whose invoice was deleted.
// Mirrors amberflo-provider's invoice.Syncer.Upsert, adapted to OpenMeter's
// status model. A failure on one invoice does not stop the others; the
// failures are joined and returned to the caller for classification the
// same way an EnsureCustomer failure would be. An empty invoice list is a
// healthy "NoInvoicesYet" state, not an error.
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
	invoices, err := r.OpenMeterClient.ListInvoicesIncludingDeleted(ctx, openMeterCustomerID)
	if err != nil {
		return fmt.Errorf("list invoices: %w", err)
	}
	projected, err := r.projectedInvoices(ctx, account)
	if err != nil {
		return err
	}

	// Oldest first, so the Invoices' creation order follows OpenMeter's when
	// several are created in one pass (billing picks the account's latest
	// Invoice by creationTimestamp).
	sort.SliceStable(invoices, func(i, j int) bool {
		if !invoices[i].CreatedAt.Equal(invoices[j].CreatedAt) {
			return invoices[i].CreatedAt.Before(invoices[j].CreatedAt)
		}
		return invoices[i].Id < invoices[j].Id
	})

	var errs []error
	listed := make(map[string]bool, len(invoices))
	for _, inv := range invoices {
		listed[inv.Id] = true
		existing := projected[inv.Id]

		if openmeter.IsInvoiceDeleted(inv) {
			// Only invoices that were never issued can be deleted, so this
			// only finds a projection made by an earlier version that
			// projected drafts. Close it out instead of leaving it Open.
			if existing != nil {
				if err := r.patchInvoiceStatus(ctx, existing, account, inv); err != nil {
					errs = append(errs, fmt.Errorf("close out Invoice for deleted invoice %s: %w", inv.Id, err))
				}
			}
			continue
		}
		if _, ok := mapInvoiceStatus(inv.Status); !ok {
			continue
		}

		if existing != nil {
			err = r.patchInvoiceStatus(ctx, existing, account, inv)
		} else {
			err = r.createInvoice(ctx, account, inv, invoiceName(account.Name, inv))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("upsert invoice %s: %w", inv.Id, err))
		}
	}

	for id, obj := range projected {
		if listed[id] {
			continue
		}
		if err := r.markInvoiceNotFound(ctx, obj, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// projectedInvoices returns this account's existing Milo Invoices keyed by
// the OpenMeter invoice id on their InvoiceKeyAnnotation. Invoices without
// the annotation were not written by this provider and are ignored. Should
// two carry the same id (not something this provider creates), the one
// with the lexicographically smallest name wins, deterministically.
func (r *BillingAccountReconciler) projectedInvoices(
	ctx context.Context,
	account *billingv1alpha1.BillingAccount,
) (map[string]*billingv1alpha1.Invoice, error) {
	var list billingv1alpha1.InvoiceList
	if err := r.List(ctx, &list, client.InNamespace(account.Namespace)); err != nil {
		return nil, fmt.Errorf("list Invoices: %w", err)
	}
	out := make(map[string]*billingv1alpha1.Invoice)
	for i := range list.Items {
		obj := &list.Items[i]
		if obj.Spec.BillingAccountRef.Name != account.Name {
			continue
		}
		id := obj.Annotations[InvoiceKeyAnnotation]
		if id == "" {
			continue
		}
		if current, ok := out[id]; ok && current.Name < obj.Name {
			continue
		}
		out[id] = obj
	}
	return out, nil
}

func (r *BillingAccountReconciler) createInvoice(
	ctx context.Context,
	account *billingv1alpha1.BillingAccount,
	inv om.Invoice,
	name string,
) error {
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
			// Set from this invoice only, and never changed afterwards
			// (spec.period is immutable).
			Period: invoicePeriod(inv),
		},
	}

	if err := r.Create(ctx, obj); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create Invoice %s/%s: %w", account.Namespace, name, err)
		}
		// Race with a concurrent reconcile (or a stale cache): re-fetch and
		// patch status — but only if it is this invoice's projection.
		var existing billingv1alpha1.Invoice
		if getErr := r.Get(ctx, types.NamespacedName{Name: name, Namespace: account.Namespace}, &existing); getErr != nil {
			return fmt.Errorf("create race re-get Invoice %s/%s: %w", account.Namespace, name, getErr)
		}
		if got := existing.Annotations[InvoiceKeyAnnotation]; got != inv.Id {
			return fmt.Errorf("invoice %s maps to Invoice %s/%s, which already projects invoice %q",
				inv.Id, account.Namespace, name, got)
		}
		return r.patchInvoiceStatus(ctx, &existing, account, inv)
	}
	return r.patchInvoiceStatus(ctx, obj, account, inv)
}

// invoiceAmounts returns the Milo amountPaid/amountDue for an invoice in
// phase with total. OpenMeter reports no partial payments, so:
//
//	Paid                          → paid total, due 0
//	Void from voided (or deleted) → paid 0, due 0 — nothing is owed
//	Void from uncollectible       → paid 0, due total — written off, but
//	                                the unpaid balance stays visible
//	Open, PastDue                 → paid 0, due total
func invoiceAmounts(inv om.Invoice, phase billingv1alpha1.InvoicePhase) (paid, due string) {
	total := inv.Totals.Total
	switch {
	case phase == billingv1alpha1.InvoicePhasePaid:
		return total, "0"
	case phase == billingv1alpha1.InvoicePhaseVoid && inv.Status != om.InvoiceStatusUncollectible:
		return "0", "0"
	default:
		return "0", total
	}
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
	if obj.Annotations[InvoiceKeyAnnotation] != inv.Id {
		base := obj.DeepCopy()
		if obj.Annotations == nil {
			obj.Annotations = map[string]string{}
		}
		obj.Annotations[InvoiceKeyAnnotation] = inv.Id
		if err := r.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("patch Invoice metadata %s/%s: %w", obj.Namespace, obj.Name, err)
		}
	}

	statusBase := obj.DeepCopy()
	projectInvoiceStatus(obj, account, inv, time.Now())
	if err := r.Status().Patch(ctx, obj, client.MergeFrom(statusBase)); err != nil {
		return fmt.Errorf("patch Invoice status %s/%s: %w", obj.Namespace, obj.Name, err)
	}
	return nil
}

// projectInvoiceStatus writes inv's state onto obj.Status. now is only used
// for paidAt when OpenMeter gives no usable timestamp.
func projectInvoiceStatus(obj *billingv1alpha1.Invoice, account *billingv1alpha1.BillingAccount, inv om.Invoice, now time.Time) {
	phase, ok := mapInvoiceStatus(inv.Status)
	deleted := openmeter.IsInvoiceDeleted(inv)
	if deleted {
		phase = billingv1alpha1.InvoicePhaseVoid
	} else if !ok {
		// Not projectable (never reached for a status reconcileInvoices
		// skips); leave the status as it is.
		return
	}

	obj.Status.Phase = phase
	obj.Status.CurrencyCode = string(inv.Currency)
	obj.Status.Total = inv.Totals.Total
	obj.Status.AmountPaid, obj.Status.AmountDue = invoiceAmounts(inv, phase)
	obj.Status.DocumentURI = ""
	obj.Status.ObservedGeneration = obj.Generation

	obj.Status.DueDate = nil
	if inv.DueAt != nil && !deleted {
		obj.Status.DueDate = &metav1.Time{Time: *inv.DueAt}
	}

	// OpenMeter has no "paid at" timestamp. The invoice's updatedAt at the
	// first observation of Paid is the closest proxy (the transition to
	// paid is what last updated it); it is recorded once and kept, since
	// updatedAt moves on with any later change to the invoice.
	switch {
	case phase != billingv1alpha1.InvoicePhasePaid:
		obj.Status.PaidAt = nil
	case obj.Status.PaidAt == nil:
		paidAt := inv.UpdatedAt
		if paidAt.IsZero() {
			paidAt = now
		}
		obj.Status.PaidAt = &metav1.Time{Time: paidAt}
	}

	ready := metav1.Condition{
		Type:               billingv1alpha1.InvoiceConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             string(phase),
		Message:            fmt.Sprintf("Invoice projected from OpenMeter with status %s", inv.Status),
		ObservedGeneration: obj.Generation,
	}
	switch {
	case deleted:
		ready.Message = "Invoice was deleted in OpenMeter before it was issued"
	case inv.Status == om.InvoiceStatusUncollectible:
		ready.Message = "Invoice marked uncollectible in OpenMeter (written off); amountDue is the unpaid balance"
	}
	if inv.Currency != "" && account.Spec.CurrencyCode != "" && string(inv.Currency) != account.Spec.CurrencyCode {
		ready.Status = metav1.ConditionFalse
		ready.Reason = invoiceReasonCurrencyMismatch
		ready.Message = fmt.Sprintf(
			"invoice currency %q does not match BillingAccount currency %q",
			inv.Currency, account.Spec.CurrencyCode,
		)
	}
	apimeta.SetStatusCondition(&obj.Status.Conditions, ready)
}

// markInvoiceNotFound records that obj's OpenMeter invoice is no longer
// listed for the customer at all — not even with deleted invoices
// included. That is not evidence the invoice is void (the customer may
// have been replaced, for instance), so the phase and amounts are left
// untouched and only the Ready condition says the projection is stale.
func (r *BillingAccountReconciler) markInvoiceNotFound(ctx context.Context, obj *billingv1alpha1.Invoice, invoiceID string) error {
	statusBase := obj.DeepCopy()
	apimeta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
		Type:               billingv1alpha1.InvoiceConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             invoiceReasonNotFoundInOpenMeter,
		Message:            fmt.Sprintf("OpenMeter invoice %s is no longer listed for this billing account's customer", invoiceID),
		ObservedGeneration: obj.Generation,
	})
	if err := r.Status().Patch(ctx, obj, client.MergeFrom(statusBase)); err != nil {
		return fmt.Errorf("patch Invoice status %s/%s: %w", obj.Namespace, obj.Name, err)
	}
	return nil
}
