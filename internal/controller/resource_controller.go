// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	examplev1alpha1 "go.miloapis.com/controller-template/api/v1alpha1"
)

const (
	resourceFinalizer = "example.miloapis.com/resource"
	ConditionTypeReady = "Ready"
)

// ResourceReconciler reconciles a Resource object.
type ResourceReconciler struct {
	client client.Client
}

// +kubebuilder:rbac:groups=example.miloapis.com,resources=resources,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=example.miloapis.com,resources=resources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=example.miloapis.com,resources=resources/finalizers,verbs=update

func (r *ResourceReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var resource examplev1alpha1.Resource
	if err := r.client.Get(ctx, req.NamespacedName, &resource); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion with finalizer
	if !resource.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &resource)
	}

	// Ensure finalizer is present
	if !controllerutil.ContainsFinalizer(&resource, resourceFinalizer) {
		controllerutil.AddFinalizer(&resource, resourceFinalizer)
		if err := r.client.Update(ctx, &resource); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Reconcile desired state
	resource.Status.Phase = examplev1alpha1.ResourcePhaseReady
	resource.Status.ObservedGeneration = resource.Generation

	apimeta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: resource.Generation,
		Reason:             "ResourceReady",
		Message:            "Resource is ready.",
	})

	if err := r.client.Status().Update(ctx, &resource); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	logger.Info("reconciled resource", "phase", resource.Status.Phase)
	return ctrl.Result{}, nil
}

func (r *ResourceReconciler) reconcileDelete(ctx context.Context, resource *examplev1alpha1.Resource) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	controllerutil.RemoveFinalizer(resource, resourceFinalizer)
	if err := r.client.Update(ctx, resource); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	logger.Info("finalized resource")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.client = mgr.GetClient()

	return ctrl.NewControllerManagedBy(mgr).
		Named("resource").
		For(&examplev1alpha1.Resource{}).
		Complete(r)
}
