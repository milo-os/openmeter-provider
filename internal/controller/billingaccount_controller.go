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
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	om "github.com/openmeterio/openmeter/api/client/go"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

const (
	// CustomerLinkFinalizer blocks deletion of a BillingAccount until the
	// matching OpenMeter customer has been deleted (or a 404 confirms it is
	// already gone). Shares the openmeter.miloapis.com domain with
	// MeterFinalizer so operators can grep a single prefix.
	CustomerLinkFinalizer = "openmeter.miloapis.com/customer-link"

	// billingAccountControllerName is the controller label value used for
	// the SetupWithManager "Named" call.
	billingAccountControllerName = "billingaccount"

	// ForceDeleteAnnotation is the ops escape hatch for a BillingAccount
	// whose deletion is stuck behind customerLinkFinalizer (e.g. OpenMeter
	// keeps rejecting a cleanup step with a permanent error, or invoices
	// will never be settled). Set to "true" on a deleting account, it makes
	// the finalizer release without the rest of the teardown: the customer,
	// its subject keys and its invoices are left in place and have to be
	// cleaned up by hand. Two things are still done, best effort: its
	// subscriptions are canceled, and its Stripe app data is removed, since
	// OpenMeter links a Stripe customer to one live customer only and the
	// left-behind customer would otherwise keep a new account for the same
	// Stripe customer from being billed.
	ForceDeleteAnnotation = "openmeter.miloapis.com/force-delete"

	// syncedRequeueAfter re-runs a successfully synced account periodically.
	// OpenMeter invoice status changes (issued, paid, overdue) have no event
	// that reaches this controller, so without it they would only reach the
	// Milo Invoice on an unrelated BillingAccount/binding/payment-method
	// event or the manager's 10h resync.
	syncedRequeueAfter = 10 * time.Minute

	// customerNameMaxLength is OpenMeter's limit on a customer name (1-256
	// characters in api/openapi.yaml's CustomerCreate/CustomerReplaceUpdate;
	// enforced as a rune count by the request validator).
	customerNameMaxLength = 256
)

// Event reasons specific to BillingAccount. The generic ones (Synced,
// SyncFailed, Deleted, DeleteFailed) are shared with MeterDefinition.
const (
	// EventReasonWaitingForInvoices: deletion is waiting for outstanding
	// OpenMeter invoices to be paid, voided or marked uncollectible.
	// Emitted when the set of outstanding invoices changes, not every poll.
	EventReasonWaitingForInvoices = "WaitingForInvoices"
	// EventReasonForceDeleted: ForceDeleteAnnotation released the finalizer
	// without any OpenMeter cleanup.
	EventReasonForceDeleted = "ForceDeleted"
	// EventReasonStripeCustomerMissing: the account is billed by invoice (an
	// active Invoice BillingArrangement), but OpenMeter has no Stripe
	// customer for it, so OpenMeter's Stripe app can't invoice it.
	EventReasonStripeCustomerMissing = "StripeCustomerMissing"
	// EventReasonStripeAppNotConfigured: the customer's billing profile has
	// no Stripe app (e.g. a Sandbox install), so Stripe app data can't be
	// synced.
	EventReasonStripeAppNotConfigured = "StripeAppNotConfigured"

	// finalizerReasonWaitingForSubscriptions is the FinalizerWaitingError
	// reason while the customer still has a live subscription.
	finalizerReasonWaitingForSubscriptions = "WaitingForSubscriptions"
)

// FinalizerWaitingError reports that a finalizer cannot finish YET, for a
// reason expected to resolve on its own (an invoice being paid, a
// subscription ending, another finalizer finishing first) — as opposed to a
// failure. Any finalizer registered on BillingAccountReconciler.Finalizers
// can return one (directly or wrapped with %w); Reconcile requeues after
// RequeueAfter without returning an error, so waiting doesn't show up as
// error logs, error backoff, or failure metrics.
//
// That only applies when EVERY error from Finalizers.Finalize is a
// FinalizerWaitingError: if any finalizer fails for real in the same pass,
// the aggregate is returned as an error so the failure isn't masked by a
// sibling that happens to be waiting. With several waiting finalizers the
// shortest RequeueAfter wins.
type FinalizerWaitingError struct {
	// Reason is a short CamelCase label for what is being waited on (e.g.
	// "WaitingForInvoices"), used in logs.
	Reason string
	// RequeueAfter is when to check again. Zero means transientRequeueAfter.
	RequeueAfter time.Duration
	// Err describes the condition in detail. May be nil.
	Err error
}

// NewFinalizerWaiting returns a *FinalizerWaitingError; see its doc.
func NewFinalizerWaiting(reason string, after time.Duration, cause error) error {
	return &FinalizerWaitingError{Reason: reason, RequeueAfter: after, Err: cause}
}

// Error implements error.
func (e *FinalizerWaitingError) Error() string {
	if e.Err == nil {
		return "waiting: " + e.Reason
	}
	return fmt.Sprintf("waiting: %s: %v", e.Reason, e.Err)
}

// Unwrap exposes the cause so errors.Is/As still see through it.
func (e *FinalizerWaitingError) Unwrap() error {
	return e.Err
}

// finalizerWaitRequeue reports whether err — typically the aggregate from
// finalizer.Finalizers.Finalize — consists ENTIRELY of FinalizerWaitingErrors,
// and if so the shortest RequeueAfter among them.
func finalizerWaitRequeue(err error) (time.Duration, []string, bool) {
	waits, ok := collectFinalizerWaits(err, nil)
	if !ok || len(waits) == 0 {
		return 0, nil, false
	}
	var after time.Duration
	reasons := make([]string, 0, len(waits))
	for _, w := range waits {
		reasons = append(reasons, w.Reason)
		if w.RequeueAfter > 0 && (after == 0 || w.RequeueAfter < after) {
			after = w.RequeueAfter
		}
	}
	if after == 0 {
		after = transientRequeueAfter
	}
	return after, reasons, true
}

// collectFinalizerWaits walks err's tree. Every leaf must be (or be wrapped
// around) a *FinalizerWaitingError, which is not descended into; ok is false
// as soon as any branch ends in something else. Aggregates (Finalizers'
// kerrors.Aggregate, which has no Unwrap) and errors.Join trees are walked
// member by member, so one waiting member can't vouch for a failing one the
// way a plain errors.As over the whole aggregate would.
func collectFinalizerWaits(err error, acc []*FinalizerWaitingError) ([]*FinalizerWaitingError, bool) {
	if err == nil {
		return acc, true
	}
	if w, ok := err.(*FinalizerWaitingError); ok {
		return append(acc, w), true
	}
	var members []error
	switch e := err.(type) {
	case kerrors.Aggregate:
		members = e.Errors()
	case interface{ Unwrap() []error }:
		members = e.Unwrap()
	default:
		next := errors.Unwrap(err)
		if next == nil {
			return nil, false
		}
		return collectFinalizerWaits(next, acc)
	}
	if len(members) == 0 {
		return nil, false
	}
	for _, m := range members {
		var ok bool
		if acc, ok = collectFinalizerWaits(m, acc); !ok {
			return nil, false
		}
	}
	return acc, true
}

// notices remembers the last message reported per key, so a condition that
// persists across reconciles is reported (event + log) once, and again only
// when it changes — instead of on every pass. In-memory only: after a
// restart each persisting condition is reported once more. The zero value
// is ready to use.
type notices struct {
	mu   sync.Mutex
	last map[string]string
}

// changed records msg under key and reports whether it differs from the
// previously recorded message (true the first time a key is seen).
func (n *notices) changed(key, msg string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.last == nil {
		n.last = map[string]string{}
	}
	if prev, ok := n.last[key]; ok && prev == msg {
		return false
	}
	n.last[key] = msg
	return true
}

// forget drops key, so the condition is reported again if it comes back.
func (n *notices) forget(key string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.last, key)
}

// forgetPrefix drops every key starting with prefix.
func (n *notices) forgetPrefix(prefix string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for k := range n.last {
		if strings.HasPrefix(k, prefix) {
			delete(n.last, k)
		}
	}
}

// BillingAccountReconciler reconciles a billing.miloapis.com/v1alpha1
// BillingAccount into an OpenMeter customer via the openmeter.Client. State
// surfacing happens via Kubernetes Events — the reconciler does NOT write to
// BillingAccount.status: billing's own controller owns Phase/Conditions/
// LinkedProjectsCount there, and we deliberately avoid a dual-writer race on
// those fields.
//
// It also syncs Milo Invoice resources from OpenMeter's own invoices (see
// invoice_sync.go) and, once the account's default payment method is
// Active, upserts the customer's Stripe app data (see stripe_customer.go) —
// unlike amberflo-provider, this is an immediate assignment rather than a
// scheduled future switch, since OpenMeter has no equivalent concept — and
// clears the stored default payment method again once there is no usable
// one.
//
// A synced account is re-reconciled every syncedRequeueAfter so OpenMeter
// invoice status changes reach the Milo Invoices.
//
// spec.paymentTerms IS synced, via a per-account BillingProfile (see
// billingprofile_sync.go) — but only partly: OpenMeter honors netDays
// (Workflow.Invoicing.DueAfter) and silently discards the collection
// alignment carrying invoiceFrequency/invoiceDayOfMonth, verified live
// against a real instance. See DesiredBillingProfile's field docs. The
// invoice day instead reaches OpenMeter as each new subscription's billing
// anchor (see subscriptionBillingAnchor).
//
// spec.taxIds is NOT synced: it has no per-customer home on OpenMeter (tax
// identity lives only on a BillingProfile's Supplier, and as a read-only
// per-invoice snapshot) — see TODO.md.
type BillingAccountReconciler struct {
	client.Client

	// OpenMeterClient is the typed wrapper around the OpenMeter REST API.
	// Shared with MeterDefinitionReconciler at startup.
	OpenMeterClient openmeter.Client

	// InvoiceLinks reads the links to the accounts' Stripe invoices for
	// Invoice.status.documentUri.
	InvoiceLinks InvoiceLinker

	// Recorder emits Kubernetes events onto reconciled BillingAccounts.
	Recorder record.EventRecorder

	// Finalizers manages CustomerLinkFinalizer's add-on-create and
	// remove-after-cleanup bookkeeping (see SetupWithManager, and
	// customerLinkFinalizer's Finalize method for the cleanup itself).
	Finalizers finalizer.Finalizers

	// Log is the reconciler-scoped logger. Each Reconcile call derives a
	// per-reconcile logger with account/namespace/customerKey values.
	Log logr.Logger

	// notices de-duplicates per-account events for conditions that persist
	// across reconciles (StripeCustomerMissing, StripeAppNotConfigured) and
	// records the last synced fingerprint behind
	// the Synced event. Keyed by "<namespace>/<name>/<kind>".
	notices notices
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingaccounts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingaccounts/finalizers,verbs=update
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=invoices,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=invoices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=paymentmethods,verbs=get;list;watch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripepaymentmethods,verbs=get;list;watch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripecustomers,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile runs a single sync iteration for a BillingAccount.
func (r *BillingAccountReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("account", req.Name, "namespace", req.Namespace)

	var account billingv1alpha1.BillingAccount
	if err := r.Get(ctx, req.NamespacedName, &account); err != nil {
		if apierrors.IsNotFound(err) {
			r.notices.forgetPrefix(noticePrefix(req.NamespacedName))
			return ctrl.Result{}, nil
		}
		logger.Error(err, "failed to get BillingAccount")
		return ctrl.Result{}, err
	}

	customerKey := openmeter.CustomerKey(account.UID)
	logger = logger.WithValues("uid", account.UID, "customerKey", customerKey)

	// Run finalizers: adds the finalizers if absent (and not being deleted),
	// or — if being deleted — runs each registered Finalize and drops the
	// ones that succeed.
	//
	// Persist BEFORE returning any error. Finalizers.Finalize runs *every*
	// registered finalizer and can come back with both "finalizer A
	// succeeded and was removed from the object" and "finalizer B failed".
	// Returning early on the error would throw away A's removal, so A's
	// remote cleanup would be re-run on every retry until B finally
	// succeeds — harmless (the deletes are idempotent) but it re-issues
	// remote calls and re-emits Deleted events indefinitely, which is
	// exactly what was observed when billingProfileFinalizer was wedged.
	finalizeResult, finalizeErr := r.Finalizers.Finalize(ctx, &account)
	if finalizeResult.Updated {
		if err := r.Update(ctx, &account); err != nil {
			if apierrors.IsConflict(err) {
				logger.Info("conflict persisting finalizer change; requeueing")
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("persisting finalizer change: %w", err)
		}
	}
	if finalizeErr != nil {
		// Waiting (on invoices being paid, subscriptions ending, a sibling
		// finalizer) is not a failure: requeue quietly. Only when nothing
		// else in the aggregate failed — see FinalizerWaitingError.
		if after, reasons, ok := finalizerWaitRequeue(finalizeErr); ok {
			logger.Info("finalizers waiting; requeueing", "waitingOn", reasons, "requeueAfter", after.String())
			return ctrl.Result{RequeueAfter: after}, nil
		}
		return ctrl.Result{}, fmt.Errorf("running finalizers: %w", finalizeErr)
	}
	if finalizeResult.Updated {
		return ctrl.Result{}, nil
	}

	if !account.DeletionTimestamp.IsZero() {
		logger.Info("BillingAccount is being deleted, skipping reconciliation")
		return ctrl.Result{}, nil
	}

	// Usage is attributed by customer key (the submission consumer sets
	// each event's subject to the account UID), so the account's project
	// bindings play no part in the customer.
	desired := desiredCustomerFromAccount(&account, customerKey)

	customer, err := r.OpenMeterClient.EnsureCustomer(ctx, desired)
	if err != nil {
		return r.handleOpenMeterCustomerError(&account, "EnsureCustomer", err)
	}
	// Resolved once and reused below: every call past this point needs
	// OpenMeter's internal id, not customerKey — several endpoints
	// (UpsertBillingProfileCustomerOverride, EnsureCustomerStripeAppData,
	// ListInvoices) only accept the internal id and reject the external key
	// (see CustomerKey/CustomerID's doc comments in client.go for why both
	// exist). customerKey itself is still needed below, but only for
	// reconcileBillingProfile's own AccountKey dedup tag — an unrelated use
	// that predates and isn't replaceable by this id.
	customerID := openmeter.CustomerID(customer.Id)

	// The customer must exist before anything below can reference it, so
	// that one failure aborts. The three syncs that follow are independent
	// of each other, though, so they're all attempted and their failures
	// aggregated rather than short-circuiting: previously a single wedged
	// subsystem silently blocked the others (a Stripe app-data precondition
	// failure stopped invoice sync entirely on every pass, and suppressed
	// the Synced event with it), which made one broken thing look like
	// several.
	var syncErrs []error

	if err := r.reconcileBillingProfile(ctx, &account, customerKey, customerID); err != nil {
		syncErrs = append(syncErrs, fmt.Errorf("reconcile billing profile: %w", err))
	}

	stripeState, err := r.reconcileStripeAppData(ctx, logger, &account, customerID)
	if err != nil {
		syncErrs = append(syncErrs, err)
	}

	if err := r.reconcileInvoices(ctx, &account, customerID); err != nil {
		syncErrs = append(syncErrs, fmt.Errorf("reconcile invoices: %w", err))
	}

	if len(syncErrs) > 0 {
		// errors.Join keeps every failure inspectable: IsPermanent/
		// IsTransient use errors.As, which walks a joined tree, so the
		// event classifies the aggregate as permanent if ANY member is and
		// transient when they all are.
		// Forgetting the last synced state makes the next successful pass
		// report Synced again, so recovering from a failure is visible.
		r.notices.forget(noticePrefix(req.NamespacedName) + EventReasonSynced)
		return r.handleOpenMeterCustomerError(&account, "sync", errors.Join(syncErrs...))
	}

	// Synced is reported on the first successful sync this process sees for
	// the account, and afterwards only when the synced state actually
	// changed. billing's own status writes and the periodic requeue below
	// re-run this reconcile constantly; emitting on every pass buried the
	// events that matter. Not a GenerationChanged predicate: this reconcile
	// legitimately reacts to status (DefaultPaymentMethodReady,
	// paymentArrangement).
	fingerprint := r.syncFingerprint(ctx, &account, customer, stripeState)
	if r.notices.changed(noticePrefix(req.NamespacedName)+EventReasonSynced, fingerprint) {
		logger.Info("reconciled billing account")
		if r.Recorder != nil {
			// customerKey (not the response's Customer.Key) — it is always
			// exactly desired.Key by construction, and this avoids a
			// nil-pointer deref if a response somehow came back without it.
			r.Recorder.Eventf(&account, "Normal", EventReasonSynced,
				"OpenMeter customer %s synced", customerKey)
		}
	} else {
		logger.V(1).Info("billing account already in sync")
	}
	return ctrl.Result{RequeueAfter: syncedRequeueAfter}, nil
}

// noticePrefix is the per-account key prefix for BillingAccountReconciler's
// notices.
func noticePrefix(nn types.NamespacedName) string {
	return nn.String() + "/"
}

// syncFingerprint summarizes the remote state a successful reconcile
// converged to: the customer (its UpdatedAt moves on every write), the
// billing-profile link, the Stripe app data, and the account's Milo
// Invoices. Two equal fingerprints mean the pass changed nothing worth a
// Synced event.
func (r *BillingAccountReconciler) syncFingerprint(
	ctx context.Context,
	account *billingv1alpha1.BillingAccount,
	customer om.Customer,
	stripeState string,
) string {
	parts := []string{
		customer.Id,
		customer.UpdatedAt.UTC().Format(time.RFC3339Nano),
		account.Annotations[BillingProfileIDAnnotation],
		account.Annotations[OpenMeterCustomerIDAnnotation],
		stripeState,
	}
	var invoices billingv1alpha1.InvoiceList
	if err := r.List(ctx, &invoices, client.InNamespace(account.Namespace)); err != nil {
		// Only costs a missed (or extra) Synced event.
		log.FromContext(ctx).V(1).Info("listing invoices for the sync fingerprint failed", "err", err.Error())
	} else {
		var inv []string
		for i := range invoices.Items {
			item := &invoices.Items[i]
			if item.Spec.BillingAccountRef.Name != account.Name {
				continue
			}
			inv = append(inv, fmt.Sprintf("%s=%s/%s", item.Name, item.Status.Phase, item.Status.Total))
		}
		sort.Strings(inv)
		parts = append(parts, inv...)
	}
	return strings.Join(parts, "|")
}

// handleOpenMeterCustomerError emits a SyncFailed event and returns the
// error for controller-runtime to log, count and retry with backoff.
// Mirrors handleOpenMeterError in meterdefinition_controller.go (and reuses
// its event-reason constants), specialized to BillingAccount. Shared across
// every OpenMeter write in Reconcile (EnsureCustomer, the billing profile,
// Stripe app data, invoices) — operation names the specific call that
// failed so the error doesn't misattribute e.g. a Stripe app-data failure
// to EnsureCustomer.
func (r *BillingAccountReconciler) handleOpenMeterCustomerError(
	account *billingv1alpha1.BillingAccount,
	operation string,
	err error,
) (ctrl.Result, error) {
	if r.Recorder != nil {
		r.Recorder.Eventf(account, "Warning", EventReasonSyncFailed,
			"%s: %v", syncReason(err), err)
	}
	return ctrl.Result{}, fmt.Errorf("OpenMeter %s: %w", operation, err)
}

// desiredCustomerFromAccount translates a BillingAccount into the
// openmeter-client DesiredCustomer shape.
//
// Key is string(account.UID) — stable across renames and guaranteed unique
// cluster-wide regardless of namespace (Kubernetes UIDs are server-assigned
// and unique across the whole cluster, unlike metadata.name), mirroring
// amberflo-provider's customerId convention.
//
// Name follows BillingContactInfo.BusinessName's own documented convention
// ("the provider controller maps this onto its Customer.name field... When
// unset, [ContactInfo.]Name is used instead"): BusinessName, else
// ContactInfo.Name, else "namespace/name" as a guaranteed-non-empty fallback
// (OpenMeter requires Name; a brand-new account may have neither set yet).
// BillingAccount is namespaced, so metadata.name alone is only unique within
// its namespace — two different orgs could each have an account named
// "acme"; qualifying the fallback with the namespace keeps their OpenMeter
// customers distinguishable by name. BusinessName/ContactInfo.Name don't
// need this: they're operator-authored values chosen to be human-meaningful
// on their own.
func desiredCustomerFromAccount(
	account *billingv1alpha1.BillingAccount,
	customerKey openmeter.CustomerKey,
) openmeter.DesiredCustomer {
	desired := openmeter.DesiredCustomer{
		Key:      customerKey,
		Name:     account.Namespace + "/" + account.Name,
		Currency: account.Spec.CurrencyCode,
	}
	if ci := account.Spec.ContactInfo; ci != nil {
		desired.Email = ci.Email
		if ci.Name != "" {
			desired.Name = ci.Name
		}
		if ci.BusinessName != "" {
			desired.Name = ci.BusinessName
		}
		if ci.Address != nil {
			desired.Address = &openmeter.Address{
				Country:    ci.Address.Country,
				Line1:      ci.Address.Line1,
				Line2:      ci.Address.Line2,
				City:       ci.Address.City,
				Region:     ci.Address.Region,
				PostalCode: ci.Address.PostalCode,
			}
		}
	}
	desired.Name = truncateCustomerName(desired.Name)
	return desired
}

// truncateCustomerName fits name within OpenMeter's customerNameMaxLength
// (counted in runes, like the server's validator), so an over-long name is
// never a permanent 400 on every reconcile. Only the namespace/name fallback
// can realistically exceed it (a namespace of up to 63 characters plus a
// name of up to 253); BusinessName/ContactInfo.Name are capped at 256 by
// the CRD. A truncated name ends in "~" plus 8 hex characters of the full
// name's SHA-256, so it is deterministic (no update loop) and two long names
// sharing a prefix still come out different.
func truncateCustomerName(name string) string {
	if utf8.RuneCountInString(name) <= customerNameMaxLength {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "~" + hex.EncodeToString(sum[:])[:8]
	runes := []rune(name)
	return string(runes[:customerNameMaxLength-utf8.RuneCountInString(suffix)]) + suffix
}

// SetupWithManager registers the reconciler with mgr, wiring a watch on
// BillingAccount and map-func watches that enqueue the parent BillingAccount
// when one of its payment methods changes.
func (r *BillingAccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("openmeter-provider") //nolint:staticcheck // SA1019: GetEventRecorder (events/v1) is a larger migration.
	}
	if r.Log.GetSink() == nil {
		r.Log = mgr.GetLogger().WithName("billingaccount-controller")
	}

	r.Finalizers = finalizer.NewFinalizers()
	if err := r.Finalizers.Register(CustomerLinkFinalizer, &customerLinkFinalizer{
		OpenMeterClient: r.OpenMeterClient,
		Recorder:        r.Recorder,
	}); err != nil {
		return fmt.Errorf("registering finalizer: %w", err)
	}
	if err := r.Finalizers.Register(BillingProfileFinalizer, &billingProfileFinalizer{
		OpenMeterClient: r.OpenMeterClient,
		Recorder:        r.Recorder,
	}); err != nil {
		return fmt.Errorf("registering finalizer: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named(billingAccountControllerName).
		For(&billingv1alpha1.BillingAccount{}).
		Watches(&billingv1alpha1.PaymentMethod{},
			handler.EnqueueRequestsFromMapFunc(mapPaymentMethodToAccount),
		).
		Watches(&stripev1alpha1.StripePaymentMethod{},
			handler.EnqueueRequestsFromMapFunc(r.mapStripePaymentMethodToAccount),
		).
		// stripe-provider publishing the Stripe customer for an account
		// with no card is what lets it be invoiced.
		Watches(&stripev1alpha1.StripeCustomer{},
			handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(),
				&billingv1alpha1.BillingAccount{}, handler.OnlyControllerOwner()),
		).
		Complete(r)
}

// customerLinkFinalizer tears down the OpenMeter customer for a BillingAccount
// being deleted. Registered under CustomerLinkFinalizer via
// finalizer.Finalizers (see SetupWithManager); the pkg/finalizer helper
// handles the add-on-create / remove-after-cleanup bookkeeping, and the
// caller persists via a plain client.Update. Kubernetes' optimistic
// concurrency control (resourceVersion) already rejects a stale write with
// a 409 rather than silently overwriting a concurrent writer's changes, so
// no Server-Side Apply dance is needed for the finalizer itself.
//
// Unlike amberflo-provider — which defaults to soft-disabling via traits,
// because Amberflo has no native customer soft-delete — this ends in a
// DeleteCustomer. OpenMeter customers carry their own deletedAt marker and
// key-based lookups already exclude soft-deleted records (see
// openmeter.client.GetCustomer's doc comment), so a hard DELETE here is the
// correct end state. ForceDeleteAnnotation is the escape hatch for when the
// teardown can't complete.
type customerLinkFinalizer struct {
	OpenMeterClient openmeter.Client
	Recorder        record.EventRecorder

	// notices de-duplicates WaitingForInvoices events, keyed by account UID.
	notices notices
}

// Finalize tears down the account's OpenMeter customer, in an order that
// matters:
//
//  1. Cancel every live subscription and confirm none is left. Self-
//     sufficient on purpose: the BillingEntitlement controller also cancels
//     them when the account is deleting, but nothing guarantees it has run
//     (or exists) by now. While a subscription is live, OpenMeter keeps
//     adding gathering lines and — with progressive billing, its default —
//     each pending-lines pass below would cut yet another small invoice.
//     OpenMeter also refuses steps 3 and 4 while one is active.
//  2. Invoice the pending lines (and drop zero-total drafts) via
//     PrepareInvoicesForCustomerDeletion. This has to precede step 3: a new
//     invoice snapshots the customer's CURRENT subject keys to compute
//     usage (billing/service/gatheringinvoicependinglines.go → the invoice
//     row's customer usage attribution), so releasing them first would bill
//     the final period's usage as zero.
//  3. Release the customer's subject keys. Only customers created before
//     usage was attributed by customer key carry any (their projects'
//     "projects/<name>"); dropping them once the final invoice is cut keeps
//     a customer that may wait weeks on unpaid invoices from holding
//     subjects it no longer bills.
//  4. Wait for outstanding invoices — a FinalizerWaitingError, polled every
//     permanentRequeueAfter, with a WaitingForInvoices event only when the
//     set of outstanding invoices changes — then delete the customer.
//
// Each step is idempotent, so a retry simply re-walks them. 404s from
// OpenMeter are treated as "already gone". Real failures keep the finalizer
// in place and return the error, so controller-runtime's rate-limited
// backoff retries it.
func (f *customerLinkFinalizer) Finalize(ctx context.Context, obj client.Object) (finalizer.Result, error) {
	account, ok := obj.(*billingv1alpha1.BillingAccount)
	if !ok {
		return finalizer.Result{}, fmt.Errorf("customerLinkFinalizer: object is not a BillingAccount (%T)", obj)
	}
	customerKey := openmeter.CustomerKey(account.UID)
	logger := log.FromContext(ctx).WithValues("customerKey", customerKey)
	invoicesNoticeKey := string(account.UID)

	if account.Annotations[ForceDeleteAnnotation] == "true" {
		// Annotations are writable by whoever can edit the account, so the
		// escape hatch must never leave a subscription billing a deleted
		// account: canceling is attempted first, best effort.
		if err := f.OpenMeterClient.CancelSubscriptions(ctx, customerKey); err != nil {
			logger.Error(err, "force-delete: canceling subscriptions failed; releasing anyway")
		}
		// The customer stays behind, so its Stripe customer is unlinked:
		// a new account for the same Stripe customer could not be billed
		// otherwise.
		if err := f.OpenMeterClient.DeleteCustomerStripeAppData(ctx, customerKey); err != nil {
			logger.Error(err, "force-delete: removing the customer's Stripe app data failed; releasing anyway")
		}
		logger.Info("force-delete annotation set; releasing the finalizer without the remaining OpenMeter cleanup",
			"annotation", ForceDeleteAnnotation)
		f.event(account, "Warning", EventReasonForceDeleted, fmt.Sprintf(
			"%s=true: finalizer released after a best-effort subscription cancel and Stripe unlink; customer %s (its subject keys and invoices) is left in OpenMeter and must be cleaned up by hand",
			ForceDeleteAnnotation, customerKey))
		f.notices.forget(invoicesNoticeKey)
		return finalizer.Result{}, nil
	}

	customer, err := f.OpenMeterClient.GetCustomer(ctx, customerKey)
	switch {
	case errors.Is(err, openmeter.ErrCustomerNotFound):
		logger.Info("OpenMeter customer already gone")
		f.notices.forget(invoicesNoticeKey)
		return finalizer.Result{}, nil
	case err != nil:
		return finalizer.Result{}, f.failed(logger, account, "get customer", err)
	}
	customerID := openmeter.CustomerID(customer.Id)

	// 1. Subscriptions.
	if err := f.OpenMeterClient.CancelSubscriptions(ctx, customerKey); err != nil {
		return finalizer.Result{}, f.failed(logger, account, "cancel subscriptions", err)
	}
	subs, err := f.OpenMeterClient.ListSubscriptions(ctx, customerKey)
	switch {
	case errors.Is(err, openmeter.ErrCustomerNotFound):
		logger.Info("OpenMeter customer already gone")
		f.notices.forget(invoicesNoticeKey)
		return finalizer.Result{}, nil
	case err != nil:
		return finalizer.Result{}, f.failed(logger, account, "list subscriptions", err)
	}
	if live := liveSubscriptionIDs(subs); len(live) > 0 {
		logger.Info("waiting for canceled subscriptions to end before tearing down the customer", "subscriptions", live)
		return finalizer.Result{}, NewFinalizerWaiting(finalizerReasonWaitingForSubscriptions, transientRequeueAfter,
			fmt.Errorf("subscriptions still live: %s", strings.Join(live, ", ")))
	}

	// 2. Pending lines → invoices. ErrInvoicesOutstanding is only reported
	// after the pending lines were invoiced, so it doesn't stop step 3.
	prepareErr := f.OpenMeterClient.PrepareInvoicesForCustomerDeletion(ctx, customerID)
	if prepareErr != nil && !errors.Is(prepareErr, openmeter.ErrInvoicesOutstanding) {
		return finalizer.Result{}, f.failed(logger, account, "prepare invoices for customer deletion", prepareErr)
	}

	// 3. Subject keys.
	if err := f.OpenMeterClient.ReleaseCustomerSubjects(ctx, customerKey); err != nil {
		return finalizer.Result{}, f.failed(logger, account, "release subject keys", err)
	}

	// 4. Outstanding invoices, then the customer itself.
	if prepareErr != nil {
		logger.Info("waiting for outstanding invoices before deleting the customer", "invoices", prepareErr.Error())
		if f.notices.changed(invoicesNoticeKey, prepareErr.Error()) {
			f.event(account, "Normal", EventReasonWaitingForInvoices,
				fmt.Sprintf("OpenMeter customer %s is deleted once its invoices are settled: %v", customerKey, prepareErr))
		}
		return finalizer.Result{}, NewFinalizerWaiting(EventReasonWaitingForInvoices, permanentRequeueAfter, prepareErr)
	}
	if err := f.OpenMeterClient.DeleteCustomer(ctx, customerKey); err != nil {
		return finalizer.Result{}, f.failed(logger, account, "delete customer", err)
	}

	f.notices.forget(invoicesNoticeKey)
	logger.Info("OpenMeter customer deleted")
	f.event(account, "Normal", EventReasonDeleted, fmt.Sprintf("OpenMeter customer %s deleted", customerKey))
	return finalizer.Result{}, nil
}

// failed logs and reports a failed teardown step, and returns the error
// (annotated with the step) for the caller to return.
func (f *customerLinkFinalizer) failed(logger logr.Logger, account *billingv1alpha1.BillingAccount, step string, err error) error {
	err = fmt.Errorf("%s: %w", step, err)
	if openmeter.IsTransient(err) {
		logger.Info("customer teardown transient failure; finalizer blocks deletion", "err", err.Error())
	} else {
		logger.Error(err, "customer teardown failure; finalizer blocks deletion")
	}
	f.event(account, "Warning", EventReasonDeleteFailed, err.Error())
	return err
}

func (f *customerLinkFinalizer) event(account *billingv1alpha1.BillingAccount, eventType, reason, msg string) {
	if f.Recorder != nil {
		f.Recorder.Event(account, eventType, reason, msg)
	}
}

// liveSubscriptionIDs returns the ids of subs still live: active, canceled
// (ending in the future) or scheduled (starting in the future). An
// immediately canceled subscription is inactive.
func liveSubscriptionIDs(subs []om.Subscription) []string {
	var live []string
	for _, s := range subs {
		switch s.Status {
		case om.SubscriptionStatusActive, om.SubscriptionStatusCanceled, om.SubscriptionStatusScheduled:
			live = append(live, s.Id)
		}
	}
	return live
}
