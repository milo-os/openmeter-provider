// SPDX-License-Identifier: AGPL-3.0-only

package submission

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
)

func account(namespace, name, uid string) *billingv1alpha1.BillingAccount {
	return &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(uid)},
	}
}

func binding(namespace, name, project, accountName string, phase billingv1alpha1.BillingAccountBindingPhase) *billingv1alpha1.BillingAccountBinding {
	return &billingv1alpha1.BillingAccountBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: billingv1alpha1.BillingAccountBindingSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: accountName},
			ProjectRef:        billingv1alpha1.ProjectRef{Name: project},
		},
		Status: billingv1alpha1.BillingAccountBindingStatus{Phase: phase},
	}
}

// newResolver builds a CachedAccountResolver over a fake client carrying the
// same field indexes AddIndexes installs on the manager's cache.
func newResolver(t *testing.T, objs ...client.Object) *CachedAccountResolver {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := billingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithIndex(&billingv1alpha1.BillingAccount{}, billingAccountNameField,
			func(obj client.Object) []string { return []string{obj.GetName()} }).
		WithIndex(&billingv1alpha1.BillingAccountBinding{}, bindingProjectField,
			func(obj client.Object) []string {
				return []string{obj.(*billingv1alpha1.BillingAccountBinding).Spec.ProjectRef.Name}
			}).
		Build()
	return &CachedAccountResolver{Reader: c}
}

func TestCachedAccountResolver(t *testing.T) {
	active := billingv1alpha1.BillingAccountBindingPhaseActive
	superseded := billingv1alpha1.BillingAccountBindingPhaseSuperseded

	tests := []struct {
		name         string
		objs         []client.Object
		project      string
		account      string
		wantUID      types.UID
		wantErr      bool
		wantNotFound bool
	}{
		{
			name:    "unique account name needs no binding",
			objs:    []client.Object{account("org-a", "acme", "uid-a")},
			project: "web", account: "acme",
			wantUID: "uid-a",
		},
		{
			name: "same name in two orgs: the project's binding picks the namespace",
			objs: []client.Object{
				account("org-a", "acme", "uid-a"),
				account("org-b", "acme", "uid-b"),
				binding("org-b", "web-binding", "web", "acme", active),
			},
			project: "web", account: "acme",
			wantUID: "uid-b",
		},
		{
			name: "a superseded binding still identifies where the event was attributed",
			objs: []client.Object{
				account("org-a", "acme", "uid-a"),
				account("org-b", "acme", "uid-b"),
				binding("org-a", "web-old", "web", "acme", superseded),
				binding("org-a", "web-new", "web", "other", active),
			},
			project: "web", account: "acme",
			wantUID: "uid-a",
		},
		{
			name:    "no such account",
			objs:    []client.Object{account("org-a", "other", "uid-o")},
			project: "web", account: "acme",
			wantErr: true, wantNotFound: true,
		},
		{
			name: "same name in two orgs and no binding to tell them apart",
			objs: []client.Object{
				account("org-a", "acme", "uid-a"),
				account("org-b", "acme", "uid-b"),
			},
			project: "web", account: "acme",
			wantErr: true, wantNotFound: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uid, err := newResolver(t, tt.objs...).ResolveAccountUID(context.Background(), tt.project, tt.account)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("uid = %q, want an error", uid)
				}
				if tt.wantNotFound && !errors.Is(err, ErrAccountUnresolved) {
					t.Errorf("err = %v, want ErrAccountUnresolved", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveAccountUID: %v", err)
			}
			if uid != tt.wantUID {
				t.Errorf("uid = %q, want %q", uid, tt.wantUID)
			}
		})
	}
}

func TestAttributionTarget(t *testing.T) {
	project, accountName, err := attributionTarget("projects/web", map[string]any{billingAccountRefExtension: "acme"})
	if err != nil || project != "web" || accountName != "acme" {
		t.Errorf("got (%q, %q, %v), want (web, acme, nil)", project, accountName, err)
	}
	for name, tc := range map[string]struct {
		subject string
		ext     map[string]any
	}{
		"no prefix":       {"web", map[string]any{billingAccountRefExtension: "acme"}},
		"empty project":   {"projects/", map[string]any{billingAccountRefExtension: "acme"}},
		"no extension":    {"projects/web", nil},
		"empty extension": {"projects/web", map[string]any{billingAccountRefExtension: ""}},
	} {
		if _, _, err := attributionTarget(tc.subject, tc.ext); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
