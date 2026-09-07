// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	examplev1alpha1 "go.miloapis.com/controller-template/api/v1alpha1"
)

var resourceLog = logf.Log.WithName("resource-webhook")

// SetupWebhookWithManager registers the Resource webhook with the manager.
func SetupWebhookWithManager(mgr ctrl.Manager) error {
	webhook := &resourceWebhook{}

	return ctrl.NewWebhookManagedBy(mgr).
		For(&examplev1alpha1.Resource{}).
		WithDefaulter(webhook).
		WithValidator(webhook).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-example-miloapis-com-v1alpha1-resource,mutating=true,failurePolicy=fail,sideEffects=None,groups=example.miloapis.com,resources=resources,verbs=create;update,versions=v1alpha1,name=mresource.kb.io,admissionReviewVersions=v1

// +kubebuilder:webhook:path=/validate-example-miloapis-com-v1alpha1-resource,mutating=false,failurePolicy=fail,sideEffects=None,groups=example.miloapis.com,resources=resources,verbs=create;update;delete,versions=v1alpha1,name=vresource.kb.io,admissionReviewVersions=v1

type resourceWebhook struct{}

var _ admission.CustomDefaulter = &resourceWebhook{}
var _ admission.CustomValidator = &resourceWebhook{}

// Default implements admission.CustomDefaulter.
func (r *resourceWebhook) Default(ctx context.Context, obj runtime.Object) error {
	resource, ok := obj.(*examplev1alpha1.Resource)
	if !ok {
		return fmt.Errorf("unexpected type %T", obj)
	}

	resourceLog.Info("defaulting", "name", resource.GetName())

	// TODO: add defaulting logic here

	return nil
}

// ValidateCreate implements admission.CustomValidator.
func (r *resourceWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	resource, ok := obj.(*examplev1alpha1.Resource)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}

	resourceLog.Info("validating create", "name", resource.GetName())

	// TODO: add validation logic here

	return nil, nil
}

// ValidateUpdate implements admission.CustomValidator.
func (r *resourceWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	_, ok := oldObj.(*examplev1alpha1.Resource)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", oldObj)
	}

	newResource, ok := newObj.(*examplev1alpha1.Resource)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", newObj)
	}

	resourceLog.Info("validating update", "name", newResource.GetName())

	// TODO: add validation logic here

	return nil, nil
}

// ValidateDelete implements admission.CustomValidator.
func (r *resourceWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	resource, ok := obj.(*examplev1alpha1.Resource)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}

	resourceLog.Info("validating delete", "name", resource.GetName())

	// TODO: add validation logic here

	return nil, nil
}
