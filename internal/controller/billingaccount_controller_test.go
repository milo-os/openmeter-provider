// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
)

func baseBillingAccount() *billingv1alpha1.BillingAccount {
	return &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "acme-account",
			Namespace: "org-a",
			UID:       types.UID("uid-1"),
		},
		Spec: billingv1alpha1.BillingAccountSpec{
			CurrencyCode: "USD",
		},
	}
}

// TestDesiredCustomerFromAccount_NamePrecedence guards
// BillingContactInfo.BusinessName's own documented convention: BusinessName
// wins if set, else ContactInfo.Name, else "namespace/name" as a fallback
// that's always non-empty (OpenMeter requires Name). The fallback includes
// the namespace — not just metadata.name — because BillingAccount is
// namespaced: two different orgs could each have an account literally named
// "acme-account", and bare metadata.name would make their OpenMeter
// customers indistinguishable by name.
func TestDesiredCustomerFromAccount_NamePrecedence(t *testing.T) {
	tests := []struct {
		name         string
		contactInfo  *billingv1alpha1.BillingContactInfo
		wantCustName string
	}{
		{
			name:         "no contactInfo falls back to namespace/name",
			contactInfo:  nil,
			wantCustName: "org-a/acme-account",
		},
		{
			name:         "contactInfo.name used over namespace/name",
			contactInfo:  &billingv1alpha1.BillingContactInfo{Email: "a@example.com", Name: "Jane Doe"},
			wantCustName: "Jane Doe",
		},
		{
			name:         "businessName wins over contactInfo.name",
			contactInfo:  &billingv1alpha1.BillingContactInfo{Email: "a@example.com", Name: "Jane Doe", BusinessName: "Acme Corp"},
			wantCustName: "Acme Corp",
		},
		{
			name:         "empty contactInfo.name/businessName falls back to namespace/name",
			contactInfo:  &billingv1alpha1.BillingContactInfo{Email: "a@example.com"},
			wantCustName: "org-a/acme-account",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := baseBillingAccount()
			account.Spec.ContactInfo = tt.contactInfo

			desired := desiredCustomerFromAccount(account, "uid-1", nil)
			if desired.Name != tt.wantCustName {
				t.Errorf("Name = %q, want %q", desired.Name, tt.wantCustName)
			}
		})
	}
}

// TestDesiredCustomerFromAccount_NamespaceDisambiguatesIdenticalNames is the
// concrete scenario the namespace-qualified fallback exists for: two
// BillingAccounts in different namespaces sharing the exact same
// metadata.name must not collide on the OpenMeter customer's display Name
// (their Keys already differ via UID, but an operator searching OpenMeter's
// UI by name has no namespace to disambiguate with).
func TestDesiredCustomerFromAccount_NamespaceDisambiguatesIdenticalNames(t *testing.T) {
	orgA := baseBillingAccount()
	orgA.Namespace = "org-a"
	orgA.UID = types.UID("uid-a")

	orgB := baseBillingAccount()
	orgB.Namespace = "org-b"
	orgB.UID = types.UID("uid-b")

	nameA := desiredCustomerFromAccount(orgA, string(orgA.UID), nil).Name
	nameB := desiredCustomerFromAccount(orgB, string(orgB.UID), nil).Name
	if nameA == nameB {
		t.Errorf("identically-named accounts in different namespaces produced the same customer Name %q", nameA)
	}
}

// TestDesiredCustomerFromAccount_Address guards the
// BillingContactInfo.Address -> openmeter.Address mapping, including that
// no address is reported when contactInfo (or contactInfo.address) is unset.
func TestDesiredCustomerFromAccount_Address(t *testing.T) {
	t.Run("no contactInfo means no address", func(t *testing.T) {
		account := baseBillingAccount()
		desired := desiredCustomerFromAccount(account, "uid-1", nil)
		if desired.Address != nil {
			t.Errorf("Address = %+v, want nil", desired.Address)
		}
	})

	t.Run("contactInfo without address means no address", func(t *testing.T) {
		account := baseBillingAccount()
		account.Spec.ContactInfo = &billingv1alpha1.BillingContactInfo{Email: "a@example.com"}
		desired := desiredCustomerFromAccount(account, "uid-1", nil)
		if desired.Address != nil {
			t.Errorf("Address = %+v, want nil", desired.Address)
		}
	})

	t.Run("address fields map through, Region included", func(t *testing.T) {
		account := baseBillingAccount()
		account.Spec.ContactInfo = &billingv1alpha1.BillingContactInfo{
			Email: "a@example.com",
			Address: &billingv1alpha1.BillingAddress{
				Country:    "US",
				Line1:      "1 Infinite Loop",
				Line2:      "Suite 100",
				City:       "Cupertino",
				Region:     "CA",
				PostalCode: "95014",
			},
		}
		desired := desiredCustomerFromAccount(account, "uid-1", nil)
		if desired.Address == nil {
			t.Fatal("Address is nil, want populated")
		}
		if desired.Address.Country != "US" ||
			desired.Address.Line1 != "1 Infinite Loop" ||
			desired.Address.Line2 != "Suite 100" ||
			desired.Address.City != "Cupertino" ||
			desired.Address.Region != "CA" ||
			desired.Address.PostalCode != "95014" {
			t.Errorf("Address = %+v, want full field-by-field match", desired.Address)
		}
	})
}

func bindingFor(projectName string, phase billingv1alpha1.BillingAccountBindingPhase) billingv1alpha1.BillingAccountBinding {
	return billingv1alpha1.BillingAccountBinding{
		Spec:   billingv1alpha1.BillingAccountBindingSpec{ProjectRef: billingv1alpha1.ProjectRef{Name: projectName}},
		Status: billingv1alpha1.BillingAccountBindingStatus{Phase: phase},
	}
}

// TestProjectsFromActiveBindings_PrefixesProjectSubjectKey is the
// regression test for a bug that would have silently broken usage
// attribution end to end: billing/emission/cloudevents.go's toCloudEvent
// sets a validated usage CloudEvent's subject to "projects/<name>", and
// OpenMeter attributes usage to a customer by exact string match against
// Customer.UsageAttribution.SubjectKeys (no normalization — confirmed in
// OpenMeter's own source). Syncing bare project names here would mean
// every usage event silently fails to attribute to any customer, with
// nothing erroring anywhere to surface it.
func TestProjectsFromActiveBindings_PrefixesProjectSubjectKey(t *testing.T) {
	got := projectsFromActiveBindings([]billingv1alpha1.BillingAccountBinding{
		bindingFor("project-alpha", billingv1alpha1.BillingAccountBindingPhaseActive),
	})
	want := []string{"projects/project-alpha"}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("projectsFromActiveBindings() = %v, want %v (must match billing/emission/cloudevents.go's ce.SetSubject(\"projects/\" + name) exactly)", got, want)
	}
}

func TestProjectsFromActiveBindings_IgnoresInactiveAndEmpty(t *testing.T) {
	got := projectsFromActiveBindings([]billingv1alpha1.BillingAccountBinding{
		bindingFor("project-superseded", billingv1alpha1.BillingAccountBindingPhaseSuperseded),
		bindingFor("", billingv1alpha1.BillingAccountBindingPhaseActive),
		bindingFor("project-active", billingv1alpha1.BillingAccountBindingPhaseActive),
	})
	want := []string{"projects/project-active"}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("projectsFromActiveBindings() = %v, want %v", got, want)
	}
}
