// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	omv3 "github.com/openmeterio/openmeter/api/v3"
	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

const (
	// CreditGrantFinalizer blocks deletion of a CreditGrant until any active
	// balance is forfeited/voided in OpenMeter during cascade deletion.
	CreditGrantFinalizer = "openmeter.miloapis.com/creditgrant"

	// CreditGrantIDAnnotation stores the OpenMeter grant ID on the CreditGrant.
	CreditGrantIDAnnotation = "openmeter.miloapis.com/grantId"

	creditGrantControllerName = "creditgrant"

	creditGrantReasonWaitingForCustomer     = "WaitingForCustomer"
	creditGrantReasonBillingAccountNotFound = "BillingAccountNotFound"
	creditGrantReasonSyncFailed             = "SyncFailed"
)

// CreditGrantReconciler syncs Milo CreditGrant resources to OpenMeter credit grants.
type CreditGrantReconciler struct {
	client.Client
	OpenMeterClient openmeter.Client
	Recorder        record.EventRecorder
}

// +kubebuilder:rbac:groups=billing.miloapis.com,resources=creditgrants,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=creditgrants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=creditgrants/finalizers,verbs=update
// +kubebuilder:rbac:groups=billing.miloapis.com,resources=billingaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile handles the creation, state reporting, voiding, and deletion of credit grants.
func (r *CreditGrantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	grant := &billingv1alpha1.CreditGrant{}
	if err := r.Get(ctx, req.NamespacedName, grant); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 1. Deletion handling (e.g. cascade GC when parent BillingAccount is deleted).
	if !grant.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(grant, CreditGrantFinalizer) {
			r.voidOnDelete(ctx, grant)
			base := grant.DeepCopy()
			controllerutil.RemoveFinalizer(grant, CreditGrantFinalizer)
			if err := r.Patch(ctx, grant, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
			}
			logger.Info("removed credit grant finalizer")
		}
		return ctrl.Result{}, nil
	}

	// 2. Add finalizer if absent.
	if !controllerutil.ContainsFinalizer(grant, CreditGrantFinalizer) {
		base := grant.DeepCopy()
		controllerutil.AddFinalizer(grant, CreditGrantFinalizer)
		if err := r.Patch(ctx, grant, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// 3. Lookup parent BillingAccount.
	account := &billingv1alpha1.BillingAccount{}
	accountKey := types.NamespacedName{
		Namespace: grant.Namespace,
		Name:      grant.Spec.BillingAccountRef.Name,
	}
	if err := r.Get(ctx, accountKey, account); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("parent BillingAccount not found", "account", accountKey)
			if _, setErr := r.patchStatusCondition(ctx, grant, billingv1alpha1.CreditGrantPhaseFailed,
				metav1.ConditionFalse, creditGrantReasonBillingAccountNotFound,
				fmt.Sprintf("parent BillingAccount %q not found in namespace %q", accountKey.Name, accountKey.Namespace)); setErr != nil {
				return ctrl.Result{}, setErr
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get parent BillingAccount: %w", err)
	}

	// 4. Lookup OpenMeter Customer.
	customer, err := r.OpenMeterClient.GetCustomer(ctx, openmeter.CustomerKey(account.UID))
	if errors.Is(err, openmeter.ErrCustomerNotFound) {
		// Waiting for the customer to be created in OpenMeter.
		logger.Info("waiting for OpenMeter customer to be created for BillingAccount", "accountUID", account.UID)
		if _, setErr := r.patchStatusCondition(ctx, grant, billingv1alpha1.CreditGrantPhasePending,
			metav1.ConditionFalse, creditGrantReasonWaitingForCustomer,
			"waiting for customer to be created in OpenMeter"); setErr != nil {
			return ctrl.Result{}, setErr
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get OpenMeter customer: %w", err)
	}

	customerID := openmeter.CustomerID(customer.Id)

	// 5. Handle voided: true.
	if grant.Spec.Voided {
		grantID := r.resolveGrantID(ctx, customerID, grant)
		if grantID != "" {
			if err := r.OpenMeterClient.VoidCreditGrant(ctx, customerID, grantID); err != nil {
				logger.Error(err, "void credit grant in OpenMeter failed", "grantID", grantID)
				return ctrl.Result{}, fmt.Errorf("void credit grant %s: %w", grantID, err)
			}
		}
		if _, setErr := r.patchStatusCondition(ctx, grant, billingv1alpha1.CreditGrantPhaseVoided,
			metav1.ConditionTrue, string(billingv1alpha1.CreditGrantPhaseVoided),
			"credit grant has been voided"); setErr != nil {
			return ctrl.Result{}, setErr
		}
		return ctrl.Result{}, nil
	}

	// 6. Ensure the grant in OpenMeter.
	var expiresAfter *time.Duration
	if grant.Spec.ExpiresAfter != nil {
		d := grant.Spec.ExpiresAfter.Duration
		expiresAfter = &d
	}

	desired := openmeter.DesiredCreditGrant{
		Name:         grant.Name,
		Amount:       grant.Spec.Amount,
		Currency:     grant.Spec.Currency,
		Description:  grant.Spec.Description,
		ExpiresAfter: expiresAfter,
		Key:          string(grant.UID),
	}

	omGrant, err := r.OpenMeterClient.EnsureCreditGrant(ctx, customerID, desired)
	if err != nil {
		logger.Error(err, "ensure credit grant failed", "desired", desired)
		if _, setErr := r.patchStatusCondition(ctx, grant, billingv1alpha1.CreditGrantPhaseFailed,
			metav1.ConditionFalse, creditGrantReasonSyncFailed,
			fmt.Sprintf("failed to sync grant to OpenMeter: %v", err)); setErr != nil {
			return ctrl.Result{}, setErr
		}
		return ctrl.Result{}, err
	}

	// Store OpenMeter grant ID on annotations if not already present.
	if grant.Annotations == nil || grant.Annotations[CreditGrantIDAnnotation] != omGrant.Id {
		base := grant.DeepCopy()
		if grant.Annotations == nil {
			grant.Annotations = map[string]string{}
		}
		grant.Annotations[CreditGrantIDAnnotation] = omGrant.Id
		if err := r.Patch(ctx, grant, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch grant annotation: %w", err)
		}
	}

	// 7. Update status from OpenMeter state.
	phase := mapCreditGrantPhase(omGrant)
	if _, err := r.updateStatusFromGrant(ctx, grant, omGrant, phase); err != nil {
		return ctrl.Result{}, err
	}

	// Schedule requeue at expiration time if active and expiring.
	if phase == billingv1alpha1.CreditGrantPhaseActive && omGrant.ExpiresAt != nil {
		requeue := time.Until(*omGrant.ExpiresAt)
		if requeue > 0 {
			return ctrl.Result{RequeueAfter: requeue}, nil
		}
	}

	return ctrl.Result{}, nil
}

// voidOnDelete attempts to void any active grant in OpenMeter before deleting the resource.
func (r *CreditGrantReconciler) voidOnDelete(ctx context.Context, grant *billingv1alpha1.CreditGrant) {
	account := &billingv1alpha1.BillingAccount{}
	accountKey := types.NamespacedName{
		Namespace: grant.Namespace,
		Name:      grant.Spec.BillingAccountRef.Name,
	}
	if err := r.Get(ctx, accountKey, account); err != nil {
		return
	}

	customer, err := r.OpenMeterClient.GetCustomer(ctx, openmeter.CustomerKey(account.UID))
	if err != nil {
		return
	}

	customerID := openmeter.CustomerID(customer.Id)
	grantID := r.resolveGrantID(ctx, customerID, grant)
	if grantID != "" {
		_ = r.OpenMeterClient.VoidCreditGrant(ctx, customerID, grantID)
	}
}

// resolveGrantID returns the OpenMeter grant ID from annotations or searches by Key (UID).
func (r *CreditGrantReconciler) resolveGrantID(
	ctx context.Context,
	customerID openmeter.CustomerID,
	grant *billingv1alpha1.CreditGrant,
) string {
	if grant.Annotations != nil && grant.Annotations[CreditGrantIDAnnotation] != "" {
		return grant.Annotations[CreditGrantIDAnnotation]
	}

	grants, err := r.OpenMeterClient.ListCreditGrants(ctx, customerID)
	if err != nil {
		return ""
	}
	for _, g := range grants {
		if g.Key != nil && *g.Key == string(grant.UID) {
			return g.Id
		}
	}
	return ""
}

// mapCreditGrantPhase translates an OpenMeter grant into a Milo CreditGrantPhase.
func mapCreditGrantPhase(omGrant omv3.BillingCreditGrant) billingv1alpha1.CreditGrantPhase {
	if omGrant.Status == omv3.BillingCreditGrantStatusVoided {
		return billingv1alpha1.CreditGrantPhaseVoided
	}
	if omGrant.ExpiresAt != nil && !omGrant.ExpiresAt.After(time.Now()) {
		return billingv1alpha1.CreditGrantPhaseExpired
	}

	switch omGrant.Status {
	case omv3.BillingCreditGrantStatusPending:
		return billingv1alpha1.CreditGrantPhasePending
	case omv3.BillingCreditGrantStatusExpired:
		return billingv1alpha1.CreditGrantPhaseExpired
	case omv3.BillingCreditGrantStatusActive:
		return billingv1alpha1.CreditGrantPhaseActive
	default:
		return billingv1alpha1.CreditGrantPhaseActive
	}
}

func (r *CreditGrantReconciler) patchStatusCondition(
	ctx context.Context,
	grant *billingv1alpha1.CreditGrant,
	phase billingv1alpha1.CreditGrantPhase,
	status metav1.ConditionStatus,
	reason, message string,
) (bool, error) {
	base := grant.DeepCopy()
	grant.Status.Phase = phase
	grant.Status.ObservedGeneration = grant.Generation
	apimeta.SetStatusCondition(&grant.Status.Conditions, metav1.Condition{
		Type:               billingv1alpha1.CreditGrantConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: grant.Generation,
	})

	if err := r.Status().Patch(ctx, grant, client.MergeFrom(base)); err != nil {
		return false, fmt.Errorf("patch CreditGrant status: %w", err)
	}
	return true, nil
}

func (r *CreditGrantReconciler) updateStatusFromGrant(
	ctx context.Context,
	grant *billingv1alpha1.CreditGrant,
	omGrant omv3.BillingCreditGrant,
	phase billingv1alpha1.CreditGrantPhase,
) (bool, error) {
	base := grant.DeepCopy()
	grant.Status.Phase = phase
	grant.Status.ObservedGeneration = grant.Generation

	if omGrant.EffectiveAt != nil {
		grant.Status.EffectiveAt = &metav1.Time{Time: *omGrant.EffectiveAt}
	} else {
		grant.Status.EffectiveAt = &metav1.Time{Time: omGrant.CreatedAt}
	}

	if omGrant.ExpiresAt != nil {
		grant.Status.ExpiresAt = &metav1.Time{Time: *omGrant.ExpiresAt}
	} else {
		grant.Status.ExpiresAt = nil
	}

	apimeta.SetStatusCondition(&grant.Status.Conditions, metav1.Condition{
		Type:               billingv1alpha1.CreditGrantConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             string(phase),
		Message:            fmt.Sprintf("credit grant is %s in OpenMeter", phase),
		ObservedGeneration: grant.Generation,
	})

	if err := r.Status().Patch(ctx, grant, client.MergeFrom(base)); err != nil {
		return false, fmt.Errorf("patch CreditGrant status from grant: %w", err)
	}
	return true, nil
}

func (r *CreditGrantReconciler) grantsForAccount(ctx context.Context, obj client.Object) []reconcile.Request {
	var list billingv1alpha1.CreditGrantList
	if err := r.List(ctx, &list,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{CreditGrantBillingAccountRefField: obj.GetName()}); err != nil {
		log.FromContext(ctx).Error(err, "list CreditGrants for BillingAccount watch")
		return nil
	}

	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: list.Items[i].Namespace,
				Name:      list.Items[i].Name,
			},
		})
	}
	return reqs
}

// SetupWithManager registers the reconciler with mgr.
func (r *CreditGrantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("openmeter-provider") //nolint:staticcheck // SA1019: GetEventRecorder (events/v1) is a larger migration.
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&billingv1alpha1.CreditGrant{}).
		Watches(&billingv1alpha1.BillingAccount{},
			handler.EnqueueRequestsFromMapFunc(r.grantsForAccount)).
		Named(creditGrantControllerName).
		Complete(r)
}
