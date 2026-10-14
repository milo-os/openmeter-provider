// SPDX-License-Identifier: AGPL-3.0-only

// Package usagequery serves usage.billing.miloapis.com UsageQuery from
// OpenMeter.
//
// A UsageQuery is create-only and never stored: Create reads the request's
// scope from the parent context Milo forwards, resolves the billing accounts
// that scope may read, queries OpenMeter, and returns the query with status
// filled in. Milo has already authenticated the caller and authorized
// usagequeries.create in that parent before the request gets here.
package usagequery

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	usagev1alpha1 "go.miloapis.com/billing/api/usage/v1alpha1"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

// REST is the RESTStorage for UsageQuery.
type REST struct {
	// milo reads BillingAccounts, MeterDefinitions and Projects from Milo.
	milo      client.Reader
	openMeter openmeter.Client
	customers *customerResolver
}

var (
	_ rest.Storage              = &REST{}
	_ rest.Creater              = &REST{}
	_ rest.Scoper               = &REST{}
	_ rest.SingularNameProvider = &REST{}
)

// NewREST returns the RESTStorage for UsageQuery. milo should be a cached
// reader: every query lists billing accounts and meter definitions.
func NewREST(milo client.Reader, openMeter openmeter.Client) *REST {
	return &REST{
		milo:      milo,
		openMeter: openMeter,
		customers: newCustomerResolver(openMeter),
	}
}

// New returns an empty UsageQuery.
func (r *REST) New() runtime.Object {
	return &usagev1alpha1.UsageQuery{}
}

// Destroy cleans up resources on shutdown.
func (r *REST) Destroy() {}

// NamespaceScoped returns false: the scope comes from the request's parent,
// not from a namespace.
func (r *REST) NamespaceScoped() bool {
	return false
}

// GetSingularName returns the singular resource name.
func (r *REST) GetSingularName() string {
	return "usagequery"
}

// Create runs the query and returns it with status filled in.
func (r *REST) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, _ *metav1.CreateOptions) (runtime.Object, error) {
	query, ok := obj.(*usagev1alpha1.UsageQuery)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("not a UsageQuery: %T", obj))
	}
	query = query.DeepCopy()

	if createValidation != nil {
		if err := createValidation(ctx, query); err != nil {
			return nil, err
		}
	}

	query.SetDefaults()
	if errs := query.Validate(); len(errs) > 0 {
		return nil, apierrors.NewInvalid(usagev1alpha1.SchemeGroupVersion.WithKind("UsageQuery").GroupKind(), query.Name, errs)
	}

	user, ok := request.UserFrom(ctx)
	if !ok || user == nil {
		return nil, apierrors.NewUnauthorized("missing user in request context")
	}
	scope, err := usagev1alpha1.ScopeFromUserExtra(user.GetExtra())
	if err != nil {
		return nil, apierrors.NewForbidden(usageQueryResource, "", err)
	}

	resolved, err := resolveScope(ctx, r.milo, scope, &query.Spec)
	if err != nil {
		return nil, err
	}

	if scope.Type == usagev1alpha1.UsageScopePlatform {
		// Only staff can query the platform; keep a trail of who read what.
		klog.InfoS("Platform usage query",
			"user", user.GetName(),
			"billingAccounts", len(resolved.accounts),
			"projects", resolved.projects,
			"meters", query.Spec.Meters,
			"startTime", query.Spec.StartTime.Time,
			"endTime", query.Spec.EndTime.Time)
	}

	plans, err := planMeters(ctx, r.milo, &query.Spec)
	if err != nil {
		return nil, err
	}

	customers, err := r.customers.customerIDs(ctx, resolved.accounts)
	if err != nil {
		return nil, apierrors.NewServiceUnavailable(fmt.Sprintf("looking up OpenMeter customers: %v", err))
	}

	meters, err := (&executor{
		openMeter: r.openMeter,
		spec:      &query.Spec,
		customers: customers,
		projects:  resolved.projects,
	}).run(ctx, plans)
	if err != nil {
		return nil, err
	}

	query.Status = usagev1alpha1.UsageQueryStatus{
		Scope:  scope,
		Meters: meters,
	}
	// A project query reads every billing account of its organization, which
	// project members may not be allowed to see, so it does not name them.
	if scope.Type == usagev1alpha1.UsageScopeProject {
		return query, nil
	}
	for _, account := range resolved.accounts {
		query.Status.BillingAccounts = append(query.Status.BillingAccounts, usagev1alpha1.BillingAccountReference{
			Namespace: account.Namespace,
			Name:      account.Name,
		})
	}
	return query, nil
}
