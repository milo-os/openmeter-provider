// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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
)

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
// scheduled future switch, since OpenMeter has no equivalent concept.
//
// spec.paymentTerms IS synced, via a per-account BillingProfile (see
// billingprofile_sync.go) — but only partly: OpenMeter honors netDays
// (Workflow.Invoicing.DueAfter) and silently discards the collection
// alignment carrying invoiceFrequency/invoiceDayOfMonth, verified live
// against a real instance. See DesiredBillingProfile's field docs.
//
// spec.taxIds is NOT synced: it has no per-customer home on OpenMeter (tax
// identity lives only on a BillingProfile's Supplier, and as a read-only
// per-invoice snapshot) — see TODO.md.
type BillingAccountReconciler struct {
	client.Client

	// OpenMeterClient is the typed wrapper around the OpenMeter REST API.
	// Shared with MeterDefinitionReconciler at startup.
	OpenMeterClient openmeter.Client

	// Recorder emits Kubernetes events onto reconciled BillingAccounts.
	Recorder record.EventRecorder

	// Finalizers manages CustomerLinkFinalizer's add-on-create and
	// remove-after-cleanup bookkeeping (see SetupWithManager, and
	// customerLinkFinalizer's Finalize method for the cleanup itself).
	Finalizers finalizer.Finalizers

	// Log is the reconciler-scoped logger. Each Reconcile call derives a
	// per-reconcile logger with account/namespace/customerKey values.
	Log logr.Logger
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingaccounts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingaccounts/finalizers,verbs=update
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingaccountbindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=invoices,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=invoices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=paymentmethods,verbs=get;list;watch
// +kubebuilder:rbac:groups=stripe.billing.miloapis.com,resources=stripepaymentmethods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile runs a single sync iteration for a BillingAccount.
func (r *BillingAccountReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("account", req.Name, "namespace", req.Namespace)

	var account billingv1alpha1.BillingAccount
	if err := r.Get(ctx, req.NamespacedName, &account); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "failed to get BillingAccount")
		return ctrl.Result{}, err
	}

	customerKey := string(account.UID)
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
		return ctrl.Result{}, fmt.Errorf("running finalizers: %w", finalizeErr)
	}
	if finalizeResult.Updated {
		return ctrl.Result{}, nil
	}

	if !account.DeletionTimestamp.IsZero() {
		logger.Info("BillingAccount is being deleted, skipping reconciliation")
		return ctrl.Result{}, nil
	}

	// Aggregate the project list from Active BillingAccountBindings
	// referencing this account. Recomputed every reconcile — there is no
	// stashed intermediate state.
	projects, err := r.aggregateActiveBindingProjects(ctx, &account)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("aggregate active binding projects: %w", err)
	}

	desired := desiredCustomerFromAccount(&account, customerKey, projects)
	logger = logger.WithValues("projects", len(desired.SubjectKeys))

	customer, err := r.OpenMeterClient.EnsureCustomer(ctx, desired)
	if err != nil {
		return r.handleOpenMeterCustomerError(logger, &account, "EnsureCustomer", err)
	}

	// The customer must exist before anything below can reference it, so
	// that one failure aborts. The three syncs that follow are independent
	// of each other, though, so they're all attempted and their failures
	// aggregated rather than short-circuiting: previously a single wedged
	// subsystem silently blocked the others (a Stripe app-data precondition
	// failure stopped invoice sync entirely on every pass, and suppressed
	// the Synced event with it), which made one broken thing look like
	// several.
	var syncErrs []error

	// reconcileBillingProfile needs OpenMeter's internal customer.Id (a
	// ULID), not customerKey — the customer override endpoint doesn't
	// accept the external key the way EnsureCustomer/GetCustomer do (see
	// UpsertBillingProfileCustomerOverride's doc comment).
	if err := r.reconcileBillingProfile(ctx, &account, customerKey, customer.Id); err != nil {
		syncErrs = append(syncErrs, fmt.Errorf("reconcile billing profile: %w", err))
	}

	stripe, err := r.resolveStripeCustomer(ctx, &account)
	switch {
	case err != nil:
		syncErrs = append(syncErrs, fmt.Errorf("resolve stripe customer: %w", err))
	case stripe.CustomerID != "":
		if err := r.OpenMeterClient.EnsureCustomerStripeAppData(ctx, customerKey, stripe.CustomerID, stripe.PaymentMethodID); err != nil {
			syncErrs = append(syncErrs, fmt.Errorf("ensure customer stripe app data: %w", err))
		}
	}
	// stripe.CustomerID == "" means the gate isn't met yet (or the
	// StripePaymentMethod hasn't reported its id) — deliberately not
	// calling EnsureCustomerStripeAppData in that case preserves whatever
	// Stripe app data was already synced, mirroring amberflo-provider's
	// "preserve previously synced traits" behavior across a transient
	// DefaultPaymentMethodReady flap.

	if err := r.reconcileInvoices(ctx, &account, customer.Id); err != nil {
		syncErrs = append(syncErrs, fmt.Errorf("reconcile invoices: %w", err))
	}

	if len(syncErrs) > 0 {
		// errors.Join keeps every failure inspectable: IsPermanent/
		// IsTransient use errors.As, which walks a joined tree, so the
		// aggregate classifies as permanent if ANY member is (5m backoff,
		// checked first) and transient when they all are (15s) — rather
		// than collapsing to an unclassified error and hot-looping.
		return r.handleOpenMeterCustomerError(logger, &account, "sync", errors.Join(syncErrs...))
	}

	logger.Info("reconciled billing account")
	if r.Recorder != nil {
		// customerKey (not the response's Customer.Key) — it is always
		// exactly desired.Key by construction, and this avoids a nil-pointer
		// deref if a response somehow came back without it.
		r.Recorder.Eventf(&account, "Normal", EventReasonSynced,
			"OpenMeter customer %s synced", customerKey)
	}
	return ctrl.Result{}, nil
}

// handleOpenMeterCustomerError classifies the error, emits an event, and
// decides whether to requeue. Mirrors handleOpenMeterError in
// meterdefinition_controller.go (and reuses its event-reason constants),
// specialized to BillingAccount. Shared across every OpenMeter write in
// Reconcile (EnsureCustomer, EnsureCustomerStripeAppData, ...) — operation
// names the specific call that failed so the log line doesn't misattribute
// e.g. a Stripe app-data failure to EnsureCustomer.
func (r *BillingAccountReconciler) handleOpenMeterCustomerError(
	logger logr.Logger,
	account *billingv1alpha1.BillingAccount,
	operation string,
	err error,
) (ctrl.Result, error) {
	switch {
	case openmeter.IsPermanent(err):
		logger.Error(err, fmt.Sprintf("OpenMeter %s permanent failure; requeueing", operation),
			"requeueAfter", permanentRequeueAfter.String())
		if r.Recorder != nil {
			r.Recorder.Eventf(account, "Warning", EventReasonSyncFailed,
				"%s: %v", syncReasonPermanent, err)
		}
		return ctrl.Result{RequeueAfter: permanentRequeueAfter}, nil
	case openmeter.IsTransient(err):
		logger.Info(fmt.Sprintf("OpenMeter %s transient failure; requeueing", operation),
			"err", err.Error(),
			"requeueAfter", transientRequeueAfter.String())
		if r.Recorder != nil {
			r.Recorder.Eventf(account, "Warning", EventReasonSyncFailed,
				"%s: %v", syncReasonTransient, err)
		}
		return ctrl.Result{RequeueAfter: transientRequeueAfter}, nil
	default:
		logger.Error(err, fmt.Sprintf("OpenMeter %s unclassified failure; treating as transient", operation))
		if r.Recorder != nil {
			r.Recorder.Eventf(account, "Warning", EventReasonSyncFailed,
				"%s: %v", syncReasonInvalid, err)
		}
		return ctrl.Result{RequeueAfter: transientRequeueAfter}, nil
	}
}

// aggregateActiveBindingProjects lists every BillingAccountBinding
// referencing the given account via the BindingBillingAccountRefField
// indexer, filters to status.phase == Active, collects the project names,
// and returns the deterministic sorted+deduped result.
func (r *BillingAccountReconciler) aggregateActiveBindingProjects(
	ctx context.Context,
	account *billingv1alpha1.BillingAccount,
) ([]string, error) {
	var bindings billingv1alpha1.BillingAccountBindingList
	if err := r.List(ctx, &bindings,
		client.InNamespace(account.Namespace),
		client.MatchingFields{BindingBillingAccountRefField: account.Name},
	); err != nil {
		return nil, err
	}
	return projectsFromActiveBindings(bindings.Items), nil
}

// projectSubjectKeyPrefix must match billing/emission/cloudevents.go's
// toCloudEvent exactly: ce.SetSubject("projects/" + ev.Project.Name). The
// submission consumer forwards that CloudEvent's subject to OpenMeter
// unmodified, and OpenMeter attributes usage to a customer by exact string
// match against Customer.UsageAttribution.SubjectKeys (confirmed in
// OpenMeter's own source — CustomerUsageAttribution.GetValues does no
// normalization). A bare project name here would never match, so every
// usage event would silently fail to attribute to any customer.
const projectSubjectKeyPrefix = "projects/"

// projectsFromActiveBindings extracts the project subject keys from
// bindings whose status.phase is Active, drops empty entries,
// de-duplicates, and sorts. See projectSubjectKeyPrefix for why these are
// prefixed rather than bare project names.
func projectsFromActiveBindings(items []billingv1alpha1.BillingAccountBinding) []string {
	projects := make([]string, 0, len(items))
	for i := range items {
		b := &items[i]
		if b.Status.Phase != billingv1alpha1.BillingAccountBindingPhaseActive {
			continue
		}
		if b.Spec.ProjectRef.Name == "" {
			continue
		}
		projects = append(projects, projectSubjectKeyPrefix+b.Spec.ProjectRef.Name)
	}
	return sortedCopy(projects)
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
	customerKey string,
	projects []string,
) openmeter.DesiredCustomer {
	desired := openmeter.DesiredCustomer{
		Key:         customerKey,
		Name:        account.Namespace + "/" + account.Name,
		Currency:    account.Spec.CurrencyCode,
		SubjectKeys: sortedCopy(projects),
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
	return desired
}

// sortedCopy returns a de-duplicated, sorted copy of in. Nil in => nil out.
func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// SetupWithManager registers the reconciler with mgr, wiring a watch on
// BillingAccount and a map-func watch that enqueues the parent
// BillingAccount when one of its bindings changes.
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
	}); err != nil {
		return fmt.Errorf("registering finalizer: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named(billingAccountControllerName).
		For(&billingv1alpha1.BillingAccount{}).
		Watches(&billingv1alpha1.BillingAccountBinding{},
			handler.EnqueueRequestsFromMapFunc(mapBindingToAccount),
		).
		Watches(&billingv1alpha1.PaymentMethod{},
			handler.EnqueueRequestsFromMapFunc(mapPaymentMethodToAccount),
		).
		Watches(&stripev1alpha1.StripePaymentMethod{},
			handler.EnqueueRequestsFromMapFunc(r.mapStripePaymentMethodToAccount),
		).
		Complete(r)
}

func mapBindingToAccount(_ context.Context, obj client.Object) []reconcile.Request {
	binding, ok := obj.(*billingv1alpha1.BillingAccountBinding)
	if !ok {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Name:      binding.Spec.BillingAccountRef.Name,
			Namespace: binding.Namespace,
		},
	}}
}

// customerLinkFinalizer deletes the OpenMeter customer for a BillingAccount
// being deleted. Registered under CustomerLinkFinalizer via
// finalizer.Finalizers (see SetupWithManager); the pkg/finalizer helper
// handles the add-on-create / remove-after-cleanup bookkeeping, and the
// caller persists via a plain client.Update. Kubernetes' optimistic
// concurrency control (resourceVersion) already rejects a stale write with
// a 409 rather than silently overwriting a concurrent writer's changes, so
// no Server-Side Apply dance is needed for the finalizer itself.
//
// Unlike amberflo-provider — which defaults to soft-disabling via traits,
// gated behind an AllowCustomerDelete flag / force-delete annotation,
// because Amberflo has no native customer soft-delete — this always calls
// DeleteCustomer directly. OpenMeter customers carry their own deletedAt
// marker and key-based lookups already exclude soft-deleted records (see
// openmeter.client.GetCustomer's doc comment), so a hard DELETE here is the
// correct and only necessary action; there is no equivalent "irreversible
// billing data loss" risk to gate behind an opt-in flag.
type customerLinkFinalizer struct {
	OpenMeterClient openmeter.Client
	Recorder        record.EventRecorder
}

// Finalize deletes the OpenMeter customer for account. 404s from OpenMeter
// are treated as success by DeleteCustomer, mirroring its tolerant-not-found
// behavior; any other failure keeps the finalizer in place (blocking
// deletion) and returns the error so controller-runtime's default
// rate-limited backoff retries it.
func (f *customerLinkFinalizer) Finalize(ctx context.Context, obj client.Object) (finalizer.Result, error) {
	account, ok := obj.(*billingv1alpha1.BillingAccount)
	if !ok {
		return finalizer.Result{}, fmt.Errorf("customerLinkFinalizer: object is not a BillingAccount (%T)", obj)
	}
	logger := log.FromContext(ctx)
	customerKey := string(account.UID)

	if err := f.OpenMeterClient.DeleteCustomer(ctx, customerKey); err != nil {
		if openmeter.IsTransient(err) {
			logger.Info("DeleteCustomer transient failure", "err", err.Error())
		} else {
			logger.Error(err, "DeleteCustomer failure; finalizer blocks deletion")
		}
		if f.Recorder != nil {
			f.Recorder.Eventf(account, "Warning", EventReasonDeleteFailed, "%v", err)
		}
		return finalizer.Result{}, err
	}

	logger.Info("OpenMeter customer deleted", "customerKey", customerKey)
	if f.Recorder != nil {
		f.Recorder.Eventf(account, "Normal", EventReasonDeleted,
			"OpenMeter customer %s deleted", customerKey)
	}
	return finalizer.Result{}, nil
}
