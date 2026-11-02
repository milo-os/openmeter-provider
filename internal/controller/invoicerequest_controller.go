/*
Copyright 2026 Datum Technology Inc.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, version 3.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

const (
	invoiceRequestControllerName = "invoicerequest"

	// IssueAttemptedAtAnnotation records, on an Issue InvoiceRequest, when
	// this provider asked OpenMeter to invoice its charges (RFC 3339). It's
	// written before the call, so a retry after a crash between OpenMeter
	// creating the invoice and the request recording it can find that
	// invoice instead of reporting nothing left to invoice.
	IssueAttemptedAtAnnotation = "openmeter.miloapis.com/issue-attempted-at"

	// issueRecoveryWindow is how long before an attempt an invoice may have
	// been created and still count as the attempt's, allowing for clock
	// skew between this provider and OpenMeter.
	issueRecoveryWindow = time.Minute

	// EventReasonInvoiceIssued is emitted when an Issue request creates an
	// invoice.
	EventReasonInvoiceIssued = "InvoiceIssued"
)

// InvoiceRequestReconciler fulfils InvoiceRequests for billing accounts
// this provider invoices, writing only status.
//
//   - Preview: prices the account's fixed charges billable now (its Offer's
//     OneTime and Recurring pricings) with OpenMeter's invoice simulation,
//     creating nothing.
//   - Issue: invoices exactly those charges now, leaving usage on the
//     normal cycle. The invoice is projected to a Milo Invoice like any
//     other, once OpenMeter issues it; status.invoiceRef names it from the
//     start, since Invoice names are deterministic (see invoiceName).
//
// A request is done once its phase is Succeeded or Failed; it's never
// revisited after that.
type InvoiceRequestReconciler struct {
	client.Client
	OpenMeterClient openmeter.Client
	Recorder        record.EventRecorder
	// Now returns the current time; time.Now when unset.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=invoicerequests,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=invoicerequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingaccounts,verbs=get;list;watch

// Reconcile fulfils a single InvoiceRequest.
func (r *InvoiceRequestReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("invoiceRequest", req.Name, "namespace", req.Namespace)

	var ir billingv1alpha1.InvoiceRequest
	if err := r.Get(ctx, req.NamespacedName, &ir); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !ir.DeletionTimestamp.IsZero() || invoiceRequestDone(&ir) {
		return ctrl.Result{}, nil
	}
	ctx = log.IntoContext(ctx, logger.WithValues("billingAccount", ir.Spec.BillingAccountRef.Name, "mode", ir.Spec.Mode))

	if ir.Status.Phase == "" {
		ir.Status.Phase = billingv1alpha1.InvoiceRequestPhasePending
		ir.Status.ObservedGeneration = ir.Generation
		if err := r.Status().Update(ctx, &ir); err != nil {
			return ctrl.Result{}, fmt.Errorf("mark pending: %w", err)
		}
	}

	var account billingv1alpha1.BillingAccount
	if err := r.Get(ctx, types.NamespacedName{Namespace: ir.Namespace, Name: ir.Spec.BillingAccountRef.Name}, &account); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get billing account: %w", err)
		}
		return ctrl.Result{}, r.fail(ctx, &ir, billingv1alpha1.InvoiceRequestReasonFailed,
			fmt.Sprintf("BillingAccount %q not found", ir.Spec.BillingAccountRef.Name))
	}

	customer, err := r.OpenMeterClient.GetCustomer(ctx, openmeter.CustomerKey(account.UID))
	if err != nil {
		if errors.Is(err, openmeter.ErrCustomerNotFound) {
			return ctrl.Result{}, r.fail(ctx, &ir, billingv1alpha1.InvoiceRequestReasonFailed,
				fmt.Sprintf("BillingAccount %q has no OpenMeter customer yet, so it has nothing to invoice", account.Name))
		}
		return r.openMeterFailed(ctx, &ir, fmt.Errorf("get customer: %w", err))
	}
	customerID := openmeter.CustomerID(customer.Id)

	switch ir.Spec.Mode {
	case billingv1alpha1.InvoiceRequestModePreview:
		return r.preview(ctx, &ir, &account, customerID)
	case billingv1alpha1.InvoiceRequestModeIssue:
		return r.issue(ctx, &ir, &account, customerID)
	}
	// Billing's CRD only admits Preview and Issue.
	return ctrl.Result{}, r.fail(ctx, &ir, billingv1alpha1.InvoiceRequestReasonUnsupported,
		fmt.Sprintf("mode %q is not supported", ir.Spec.Mode))
}

// preview prices the account's billable fixed charges without creating
// anything. No charges is a successful, empty preview.
func (r *InvoiceRequestReconciler) preview(
	ctx context.Context,
	ir *billingv1alpha1.InvoiceRequest,
	account *billingv1alpha1.BillingAccount,
	customerID openmeter.CustomerID,
) (ctrl.Result, error) {
	charges, err := r.OpenMeterClient.PendingFixedCharges(ctx, customerID, r.now())
	if err != nil {
		return r.openMeterFailed(ctx, ir, fmt.Errorf("list fixed charges: %w", err))
	}
	if len(charges) == 0 {
		return ctrl.Result{}, r.succeed(ctx, ir, account.Spec.CurrencyCode, nil, "0", nil,
			"No fixed charges are waiting to be invoiced")
	}
	inv, err := r.OpenMeterClient.PreviewInvoice(ctx, customerID, charges)
	if err != nil {
		return r.openMeterFailed(ctx, ir, fmt.Errorf("preview invoice: %w", err))
	}
	lines, total, err := invoiceRequestLines(inv)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed, err.Error())
	}
	return ctrl.Result{}, r.succeed(ctx, ir, inv.Currency, lines, total, nil,
		fmt.Sprintf("Previewed %d fixed charges", len(lines)))
}

// issue invoices the account's billable fixed charges now.
func (r *InvoiceRequestReconciler) issue(
	ctx context.Context,
	ir *billingv1alpha1.InvoiceRequest,
	account *billingv1alpha1.BillingAccount,
	customerID openmeter.CustomerID,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// A previous attempt may have created the invoice before failing to
	// record it.
	if attempted, ok := issueAttemptedAt(ir); ok {
		inv, found, err := r.invoiceCreatedSince(ctx, customerID, attempted.Add(-issueRecoveryWindow))
		if err != nil {
			return r.openMeterFailed(ctx, ir, err)
		}
		if found {
			logger.Info("recovered the invoice an earlier attempt created", "invoiceID", inv.Id)
			return r.recordIssued(ctx, ir, account, inv)
		}
	}

	now := r.now()
	charges, err := r.OpenMeterClient.PendingFixedCharges(ctx, customerID, now)
	if err != nil {
		return r.openMeterFailed(ctx, ir, fmt.Errorf("list fixed charges: %w", err))
	}
	if len(charges) == 0 {
		return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonNothingToInvoice,
			"No fixed charges are waiting to be invoiced")
	}

	// Record the attempt first; Update fails on a stale copy, so two
	// reconciles can't both go on to invoice.
	if ir.Annotations == nil {
		ir.Annotations = map[string]string{}
	}
	ir.Annotations[IssueAttemptedAtAnnotation] = now.UTC().Format(time.RFC3339)
	if err := r.Update(ctx, ir); err != nil {
		return ctrl.Result{}, fmt.Errorf("record issue attempt: %w", err)
	}

	lineIDs := make([]string, 0, len(charges))
	for _, ch := range charges {
		lineIDs = append(lineIDs, ch.LineID)
	}
	invoices, err := r.OpenMeterClient.InvoiceFixedCharges(ctx, customerID, lineIDs, now)
	if err != nil {
		if errors.Is(err, openmeter.ErrNothingToInvoice) {
			return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonNothingToInvoice,
				"No fixed charges are waiting to be invoiced")
		}
		return r.openMeterFailed(ctx, ir, fmt.Errorf("invoice fixed charges: %w", err))
	}
	inv, err := r.OpenMeterClient.GetInvoiceWithLines(ctx, invoices[0].Id)
	if err != nil {
		return r.openMeterFailed(ctx, ir, fmt.Errorf("read issued invoice: %w", err))
	}
	return r.recordIssued(ctx, ir, account, inv)
}

// recordIssued completes an Issue request with the invoice it created.
func (r *InvoiceRequestReconciler) recordIssued(
	ctx context.Context,
	ir *billingv1alpha1.InvoiceRequest,
	account *billingv1alpha1.BillingAccount,
	inv om.Invoice,
) (ctrl.Result, error) {
	lines, total, err := invoiceRequestLines(inv)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed,
			fmt.Sprintf("OpenMeter invoice %s was created, but its lines can't be read: %v", inv.Id, err))
	}
	name := invoiceName(account.Name, inv)
	msg := fmt.Sprintf("Created OpenMeter invoice %s; it appears as Invoice %s once OpenMeter issues it", inv.Id, name)
	if err := r.succeed(ctx, ir, inv.Currency, lines, total, &billingv1alpha1.InvoiceRequestInvoiceRef{Name: name}, msg); err != nil {
		return ctrl.Result{}, err
	}
	r.event(ir, "Normal", EventReasonInvoiceIssued, msg)
	return ctrl.Result{}, nil
}

// invoiceCreatedSince returns the customer's newest non-gathering invoice
// created at or after since, if any.
func (r *InvoiceRequestReconciler) invoiceCreatedSince(ctx context.Context, customerID openmeter.CustomerID, since time.Time) (om.Invoice, bool, error) {
	invoices, err := r.OpenMeterClient.ListInvoices(ctx, customerID)
	if err != nil {
		return om.Invoice{}, false, fmt.Errorf("list invoices: %w", err)
	}
	// ListInvoices is newest first.
	for _, inv := range invoices {
		if inv.Status == om.InvoiceStatusGathering || inv.CreatedAt.Before(since) {
			continue
		}
		full, err := r.OpenMeterClient.GetInvoiceWithLines(ctx, inv.Id)
		if err != nil {
			return om.Invoice{}, false, fmt.Errorf("read invoice %s: %w", inv.Id, err)
		}
		return full, true, nil
	}
	return om.Invoice{}, false, nil
}

// invoiceRequestLines maps an OpenMeter invoice's lines to an
// InvoiceRequest's, with their total, each before tax.
func invoiceRequestLines(inv om.Invoice) ([]billingv1alpha1.InvoiceRequestLine, string, error) {
	total, err := openmeter.AmountBeforeTax(inv.Totals)
	if err != nil {
		return nil, "", fmt.Errorf("invoice total: %w", err)
	}
	if inv.Lines == nil {
		return nil, total, nil
	}
	lines := make([]billingv1alpha1.InvoiceRequestLine, 0, len(*inv.Lines))
	for _, line := range *inv.Lines {
		if line.DeletedAt != nil {
			continue
		}
		amount, err := openmeter.AmountBeforeTax(line.Totals)
		if err != nil {
			return nil, "", fmt.Errorf("line %s: %w", line.Id, err)
		}
		out := billingv1alpha1.InvoiceRequestLine{Description: line.Name, Amount: amount}
		// One-time charges cover an instant rather than a period.
		if line.Period.To.After(line.Period.From) {
			out.Period = &billingv1alpha1.InvoicePeriod{
				Start: metav1.NewTime(line.Period.From.Truncate(time.Second)),
				End:   metav1.NewTime(line.Period.To.Add(-time.Nanosecond).Truncate(time.Second)),
			}
		}
		lines = append(lines, out)
	}
	return lines, total, nil
}

// issueAttemptedAt reads IssueAttemptedAtAnnotation.
func issueAttemptedAt(ir *billingv1alpha1.InvoiceRequest) (time.Time, bool) {
	v, ok := ir.Annotations[IssueAttemptedAtAnnotation]
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	return t, err == nil
}

func invoiceRequestDone(ir *billingv1alpha1.InvoiceRequest) bool {
	return ir.Status.Phase == billingv1alpha1.InvoiceRequestPhaseSucceeded ||
		ir.Status.Phase == billingv1alpha1.InvoiceRequestPhaseFailed
}

// succeed completes the request.
func (r *InvoiceRequestReconciler) succeed(
	ctx context.Context,
	ir *billingv1alpha1.InvoiceRequest,
	currency string,
	lines []billingv1alpha1.InvoiceRequestLine,
	total string,
	invoiceRef *billingv1alpha1.InvoiceRequestInvoiceRef,
	msg string,
) error {
	ir.Status.CurrencyCode = currency
	ir.Status.Lines = lines
	ir.Status.Total = total
	ir.Status.InvoiceRef = invoiceRef
	return r.complete(ctx, ir, billingv1alpha1.InvoiceRequestPhaseSucceeded, metav1.ConditionTrue,
		billingv1alpha1.InvoiceRequestReasonSucceeded, msg)
}

// fail completes the request unsuccessfully. A request is never retried
// once failed: staff create a new one.
func (r *InvoiceRequestReconciler) fail(ctx context.Context, ir *billingv1alpha1.InvoiceRequest, reason, msg string) error {
	if err := r.complete(ctx, ir, billingv1alpha1.InvoiceRequestPhaseFailed, metav1.ConditionTrue, reason, msg); err != nil {
		return err
	}
	r.event(ir, "Warning", reason, msg)
	return nil
}

func (r *InvoiceRequestReconciler) complete(
	ctx context.Context,
	ir *billingv1alpha1.InvoiceRequest,
	phase billingv1alpha1.InvoiceRequestPhase,
	status metav1.ConditionStatus,
	reason, msg string,
) error {
	now := metav1.NewTime(r.now())
	ir.Status.Phase = phase
	ir.Status.CompletedAt = &now
	ir.Status.ObservedGeneration = ir.Generation
	apimeta.SetStatusCondition(&ir.Status.Conditions, metav1.Condition{
		Type:               billingv1alpha1.InvoiceRequestConditionCompleted,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: ir.Generation,
	})
	if err := r.Status().Update(ctx, ir); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	return nil
}

// openMeterFailed fails the request on a permanent OpenMeter error, and
// returns a transient one for a rate-limited retry, leaving it Pending.
func (r *InvoiceRequestReconciler) openMeterFailed(ctx context.Context, ir *billingv1alpha1.InvoiceRequest, err error) (ctrl.Result, error) {
	if openmeter.IsPermanent(err) {
		return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed, err.Error())
	}
	return ctrl.Result{}, err
}

func (r *InvoiceRequestReconciler) event(ir *billingv1alpha1.InvoiceRequest, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(ir, eventType, reason, msg)
	}
}

func (r *InvoiceRequestReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager registers the controller with the manager. Billing
// added InvoiceRequest after this provider's other kinds, so a control
// plane on an older billing skips the controller instead of failing to
// start: watching an API that isn't served would stop the whole manager.
// Upgrading billing later needs a restart to turn it on.
func (r *InvoiceRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	gvk := billingv1alpha1.GroupVersion.WithKind("InvoiceRequest")
	if _, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		if apimeta.IsNoMatchError(err) {
			mgr.GetLogger().Info("billing's InvoiceRequest API isn't served, so invoice requests are off", "kind", gvk.String())
			return nil
		}
		return fmt.Errorf("look up %s: %w", gvk, err)
	}
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("openmeter-provider") //nolint:staticcheck // SA1019: GetEventRecorder (events/v1) is a larger migration.
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(invoiceRequestControllerName).
		For(&billingv1alpha1.InvoiceRequest{}).
		Complete(r)
}
