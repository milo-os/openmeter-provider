// SPDX-License-Identifier: AGPL-3.0-only

package submission

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
)

// billingAccountRefExtension is the CloudEvent extension billing's usage
// consumer stamps on every validated event: the name of the BillingAccount
// the event's project was bound to when billing attributed it
// (billing internal/controller/consumer/consumer.go, attribution stage).
const billingAccountRefExtension = "billingaccountref"

// projectSubjectPrefix is how billing's emitters encode the project in the
// CloudEvent subject (billing emission/cloudevents.go: "projects/<name>").
const projectSubjectPrefix = "projects/"

// Field indexes the resolver queries through the manager's cache. Installed
// by AddIndexes.
const (
	billingAccountNameField = "submission.billingAccountName"
	bindingProjectField     = "submission.bindingProject"
)

// ErrAccountUnresolved is returned when a usage event's BillingAccount
// cannot be identified (yet). It is never a reason to drop the event: the
// account may not have reached the cache, so the message is retried.
var ErrAccountUnresolved = errors.New("billing account for usage event not resolved")

// AccountResolver identifies the BillingAccount a validated usage event is
// billed to.
type AccountResolver interface {
	// ResolveAccountUID returns the UID of the BillingAccount named
	// accountName that project was bound to. The UID is the account's
	// OpenMeter customer key.
	ResolveAccountUID(ctx context.Context, project, accountName string) (types.UID, error)
}

// CachedAccountResolver resolves accounts from the manager's informer cache.
//
// billing attributes each event to an account by name only, and
// BillingAccounts are namespaced, so the name is usually enough but not
// always unique: when several namespaces have an account with that name,
// the project's BillingAccountBinding (projects are bound in exactly one
// namespace) picks the namespace. Bindings in any phase count — a project
// that has since moved to another account is Superseded, but its binding
// still says which namespace the event was attributed in.
type CachedAccountResolver struct {
	Reader client.Reader
}

var _ AccountResolver = (*CachedAccountResolver)(nil)

// ResolveAccountUID implements AccountResolver.
//
// Reads use client.UnsafeDisableDeepCopy: the cache hands back its own
// objects instead of copying them for every usage event. That is safe only
// because nothing here modifies what it reads (only UIDs, names and
// namespaces are inspected) — mutating a returned object would corrupt the
// shared cache.
func (r *CachedAccountResolver) ResolveAccountUID(ctx context.Context, project, accountName string) (types.UID, error) {
	var accounts billingv1alpha1.BillingAccountList
	if err := r.Reader.List(ctx, &accounts, client.MatchingFields{billingAccountNameField: accountName}, client.UnsafeDisableDeepCopy); err != nil {
		return "", fmt.Errorf("list billing accounts named %q: %w", accountName, err)
	}
	switch len(accounts.Items) {
	case 0:
		return "", fmt.Errorf("%w: no BillingAccount named %q", ErrAccountUnresolved, accountName)
	case 1:
		return accounts.Items[0].UID, nil
	}

	var bindings billingv1alpha1.BillingAccountBindingList
	if err := r.Reader.List(ctx, &bindings, client.MatchingFields{bindingProjectField: project}, client.UnsafeDisableDeepCopy); err != nil {
		return "", fmt.Errorf("list billing account bindings of project %q: %w", project, err)
	}
	namespaces := map[string]struct{}{}
	for i := range bindings.Items {
		if bindings.Items[i].Spec.BillingAccountRef.Name == accountName {
			namespaces[bindings.Items[i].Namespace] = struct{}{}
		}
	}
	var match *billingv1alpha1.BillingAccount
	for i := range accounts.Items {
		if _, ok := namespaces[accounts.Items[i].Namespace]; !ok {
			continue
		}
		if match != nil {
			return "", fmt.Errorf("%w: project %q is bound to BillingAccounts named %q in several namespaces",
				ErrAccountUnresolved, project, accountName)
		}
		match = &accounts.Items[i]
	}
	if match == nil {
		return "", fmt.Errorf("%w: %d BillingAccounts are named %q and no binding of project %q says which",
			ErrAccountUnresolved, len(accounts.Items), accountName, project)
	}
	return match.UID, nil
}

// AddIndexes installs the field indexes CachedAccountResolver queries.
func AddIndexes(ctx context.Context, fi client.FieldIndexer) error {
	if err := fi.IndexField(ctx, &billingv1alpha1.BillingAccount{}, billingAccountNameField,
		func(obj client.Object) []string { return []string{obj.GetName()} },
	); err != nil {
		return fmt.Errorf("index billing accounts by name: %w", err)
	}
	if err := fi.IndexField(ctx, &billingv1alpha1.BillingAccountBinding{}, bindingProjectField,
		func(obj client.Object) []string {
			binding, ok := obj.(*billingv1alpha1.BillingAccountBinding)
			if !ok || binding.Spec.ProjectRef.Name == "" {
				return nil
			}
			return []string{binding.Spec.ProjectRef.Name}
		},
	); err != nil {
		return fmt.Errorf("index billing account bindings by project: %w", err)
	}
	return nil
}

// attributionTarget extracts what attribution needs from a validated event:
// the project (from its "projects/<name>" subject) and the billing account
// name billing attributed it to (the billingaccountref extension).
func attributionTarget(subject string, extensions map[string]any) (project, accountName string, err error) {
	project = strings.TrimPrefix(subject, projectSubjectPrefix)
	if project == subject || project == "" {
		return "", "", fmt.Errorf("subject %q is not of the form %s<project>", subject, projectSubjectPrefix)
	}
	ref, ok := extensions[billingAccountRefExtension]
	if !ok || ref == nil || fmt.Sprint(ref) == "" {
		return "", "", fmt.Errorf("missing %s extension", billingAccountRefExtension)
	}
	return project, fmt.Sprint(ref), nil
}
