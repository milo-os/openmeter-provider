// SPDX-License-Identifier: AGPL-3.0-only

package usagequery

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	usagev1alpha1 "go.miloapis.com/billing/api/usage/v1alpha1"
	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
)

// ProjectGVK is Milo's Project. It is read as unstructured so this provider
// does not depend on Milo's Go module for one label and one field.
var ProjectGVK = schema.GroupVersionKind{
	Group:   "resourcemanager.miloapis.com",
	Version: "v1alpha1",
	Kind:    "Project",
}

// organizationNameLabel is set by Milo on every Project it admits, naming the
// organization that owns it.
const organizationNameLabel = "resourcemanager.miloapis.com/organization-name"

// resolvedScope is what a query may read once its scope and spec are
// combined.
type resolvedScope struct {
	// accounts are the billing accounts whose usage is read, sorted by
	// namespace and name.
	accounts []billingv1alpha1.BillingAccount
	// projects is the project_name filter; nil reads every project.
	projects []string
}

var usageQueryResource = usagev1alpha1.Resource("usagequeries")

func forbidden(format string, args ...any) error {
	return apierrors.NewForbidden(usageQueryResource, "", fmt.Errorf(format, args...))
}

// resolveScope applies the rules in billing's UsageQuery docs: the request's
// parent sets the boundary, and spec.billingAccounts and spec.projects can
// only narrow it.
//
//   - Organization: the organization's billing accounts, optionally narrowed
//     to some of them, or to some of its projects.
//   - Project: every billing account of the project's organization, always
//     filtered to the project, so a project that moved between billing
//     accounts keeps its history. billingAccounts is not accepted: project
//     members may not be allowed to see the organization's accounts.
//   - Platform: the named billing accounts, or the accounts of the named
//     projects' organizations. One of the two is required, so a typo cannot
//     scan every customer.
//
// Anything that reaches outside the scope is Forbidden, never an empty result.
func resolveScope(ctx context.Context, milo client.Reader, scope usagev1alpha1.UsageScope, spec *usagev1alpha1.UsageQuerySpec) (*resolvedScope, error) {
	switch scope.Type {
	case usagev1alpha1.UsageScopeOrganization:
		return resolveOrganizationScope(ctx, milo, scope.Name, spec)
	case usagev1alpha1.UsageScopeProject:
		return resolveProjectScope(ctx, milo, scope.Name, spec)
	case usagev1alpha1.UsageScopePlatform:
		return resolvePlatformScope(ctx, milo, spec)
	default:
		return nil, forbidden("usage cannot be queried in a %q scope", scope.Type)
	}
}

func resolveOrganizationScope(ctx context.Context, milo client.Reader, org string, spec *usagev1alpha1.UsageQuerySpec) (*resolvedScope, error) {
	namespace := usagev1alpha1.OrganizationNamespace(org)

	for _, project := range spec.Projects {
		projectOrg, err := projectOrganization(ctx, milo, project)
		if err != nil {
			return nil, err
		}
		// A project of another organization and a project that does not
		// exist answer the same, so nothing is learned about other
		// organizations.
		if projectOrg != org {
			return nil, forbidden("project %q is not in organization %q", project, org)
		}
	}

	var accounts []billingv1alpha1.BillingAccount
	if len(spec.BillingAccounts) > 0 {
		for _, ref := range spec.BillingAccounts {
			if ref.Namespace != "" && ref.Namespace != namespace {
				return nil, forbidden("billing account %s/%s is not in organization %q", ref.Namespace, ref.Name, org)
			}
			account, err := getAccount(ctx, milo, namespace, ref.Name)
			if err != nil {
				return nil, err
			}
			accounts = append(accounts, account)
		}
	} else {
		var err error
		if accounts, err = listAccounts(ctx, milo, namespace); err != nil {
			return nil, err
		}
	}

	return &resolvedScope{accounts: sortAccounts(accounts), projects: spec.Projects}, nil
}

func resolveProjectScope(ctx context.Context, milo client.Reader, project string, spec *usagev1alpha1.UsageQuerySpec) (*resolvedScope, error) {
	if len(spec.BillingAccounts) > 0 {
		return nil, forbidden("spec.billingAccounts cannot be set in a project; the project's usage is read from every billing account of its organization")
	}
	for _, p := range spec.Projects {
		if p != project {
			return nil, forbidden("a query in project %q can only read that project's usage", project)
		}
	}

	org, err := projectOrganization(ctx, milo, project)
	if err != nil {
		return nil, err
	}
	accounts, err := listAccounts(ctx, milo, usagev1alpha1.OrganizationNamespace(org))
	if err != nil {
		return nil, err
	}

	return &resolvedScope{accounts: sortAccounts(accounts), projects: []string{project}}, nil
}

func resolvePlatformScope(ctx context.Context, milo client.Reader, spec *usagev1alpha1.UsageQuerySpec) (*resolvedScope, error) {
	if len(spec.BillingAccounts) == 0 && len(spec.Projects) == 0 {
		return nil, apierrors.NewBadRequest("a platform query must set spec.billingAccounts or spec.projects")
	}

	var accounts []billingv1alpha1.BillingAccount
	if len(spec.BillingAccounts) > 0 {
		for _, ref := range spec.BillingAccounts {
			if ref.Namespace == "" {
				return nil, apierrors.NewBadRequest(fmt.Sprintf("billing account %q needs a namespace in a platform query", ref.Name))
			}
			account, err := getAccount(ctx, milo, ref.Namespace, ref.Name)
			if err != nil {
				return nil, err
			}
			accounts = append(accounts, account)
		}
	} else {
		seen := map[types.UID]bool{}
		for _, project := range spec.Projects {
			org, err := projectOrganization(ctx, milo, project)
			if err != nil {
				return nil, err
			}
			orgAccounts, err := listAccounts(ctx, milo, usagev1alpha1.OrganizationNamespace(org))
			if err != nil {
				return nil, err
			}
			for _, account := range orgAccounts {
				if !seen[account.UID] {
					seen[account.UID] = true
					accounts = append(accounts, account)
				}
			}
		}
	}

	return &resolvedScope{accounts: sortAccounts(accounts), projects: spec.Projects}, nil
}

// projectOrganization returns the organization that owns project. A project
// that does not exist is Forbidden, the same answer as a project outside the
// caller's scope.
func projectOrganization(ctx context.Context, milo client.Reader, project string) (string, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(ProjectGVK)
	if err := milo.Get(ctx, client.ObjectKey{Name: project}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return "", forbidden("project %q is not in scope", project)
		}
		return "", apierrors.NewInternalError(fmt.Errorf("reading project %q: %w", project, err))
	}

	if org := obj.GetLabels()[organizationNameLabel]; org != "" {
		return org, nil
	}
	// Projects admitted before Milo set the label still name their owner.
	kind, _, _ := unstructured.NestedString(obj.Object, "spec", "ownerRef", "kind")
	name, _, _ := unstructured.NestedString(obj.Object, "spec", "ownerRef", "name")
	if kind == "Organization" && name != "" {
		return name, nil
	}
	return "", apierrors.NewInternalError(fmt.Errorf("project %q does not name its organization", project))
}

func getAccount(ctx context.Context, milo client.Reader, namespace, name string) (billingv1alpha1.BillingAccount, error) {
	var account billingv1alpha1.BillingAccount
	if err := milo.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &account); err != nil {
		if apierrors.IsNotFound(err) {
			return account, apierrors.NewNotFound(billingv1alpha1.GroupVersion.WithResource("billingaccounts").GroupResource(), name)
		}
		return account, apierrors.NewInternalError(fmt.Errorf("reading billing account %s/%s: %w", namespace, name, err))
	}
	return account, nil
}

func listAccounts(ctx context.Context, milo client.Reader, namespace string) ([]billingv1alpha1.BillingAccount, error) {
	var list billingv1alpha1.BillingAccountList
	if err := milo.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, apierrors.NewInternalError(fmt.Errorf("listing billing accounts in %s: %w", namespace, err))
	}
	return list.Items, nil
}

func sortAccounts(accounts []billingv1alpha1.BillingAccount) []billingv1alpha1.BillingAccount {
	slices.SortFunc(accounts, func(a, b billingv1alpha1.BillingAccount) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return accounts
}
