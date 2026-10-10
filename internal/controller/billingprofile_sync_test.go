// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	om "github.com/openmeterio/openmeter/api/client/go"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

func TestDesiredBillingProfileFromAccount_NoArrangementUsesStandardTerms(t *testing.T) {
	account := baseBillingAccount()
	account.Spec.PaymentTerms = nil

	got := desiredBillingProfileFromAccount(account, "test-account-key")
	if got.NetDays != 30 || got.InvoiceFrequency != "Monthly" || got.InvoiceDayOfMonth != 1 {
		t.Errorf("desiredBillingProfileFromAccount() = %+v, want billing's standard terms (30/Monthly/1)", got)
	}
	if got.CollectionMethod != "" {
		t.Errorf("CollectionMethod = %q, want empty (inherit the default profile's)", got.CollectionMethod)
	}
	if got.AccountKey != "test-account-key" {
		t.Errorf("AccountKey = %q, want test-account-key", got.AccountKey)
	}
}

// TestDesiredBillingProfileFromAccount_StandardPaymentTermsSpec uses the
// only spec.paymentTerms billing's webhook accepts: the standard ones.
func TestDesiredBillingProfileFromAccount_StandardPaymentTermsSpec(t *testing.T) {
	account := baseBillingAccount()
	account.Spec.PaymentTerms = &billingv1alpha1.PaymentTerms{
		NetDays:           billingv1alpha1.StandardPaymentTermsNetDays,
		InvoiceFrequency:  billingv1alpha1.StandardPaymentTermsInvoiceFrequency,
		InvoiceDayOfMonth: billingv1alpha1.StandardPaymentTermsInvoiceDayOfMonth,
	}

	got := desiredBillingProfileFromAccount(account, "test-account-key")
	if got.NetDays != 30 || got.InvoiceFrequency != "Monthly" || got.InvoiceDayOfMonth != 1 || got.CollectionMethod != "" {
		t.Errorf("desiredBillingProfileFromAccount() = %+v, want the standard terms", got)
	}
}

// TestDesiredBillingProfileFromAccount_IgnoresLegacyNonStandardSpec: terms
// stored before billing locked spec.paymentTerms are not honored — only a
// BillingArrangement grants non-standard terms.
func TestDesiredBillingProfileFromAccount_IgnoresLegacyNonStandardSpec(t *testing.T) {
	account := baseBillingAccount()
	account.Spec.PaymentTerms = &billingv1alpha1.PaymentTerms{NetDays: 90, InvoiceFrequency: "Quarterly", InvoiceDayOfMonth: 15}

	got := desiredBillingProfileFromAccount(account, "test-account-key")
	if got.NetDays != 30 || got.InvoiceFrequency != "Monthly" || got.InvoiceDayOfMonth != 1 {
		t.Errorf("desiredBillingProfileFromAccount() = %+v, want the standard terms", got)
	}
}

func TestDesiredBillingProfileFromAccount_InvoiceArrangement(t *testing.T) {
	for _, tt := range []struct {
		name        string
		arrangement *billingv1alpha1.BillingAccountPaymentArrangement
		wantNetDays int32
		wantMethod  om.CollectionMethod
	}{
		{
			name: "invoice terms",
			arrangement: &billingv1alpha1.BillingAccountPaymentArrangement{
				Name: "msa", Type: billingv1alpha1.BillingArrangementTypeInvoice,
				Invoice: &billingv1alpha1.InvoiceArrangementTerms{NetDays: 45, CreditLimit: "10000"},
			},
			wantNetDays: 45,
			wantMethod:  om.CollectionMethodSendInvoice,
		},
		{
			name: "invoice terms without netDays use the CRD default",
			arrangement: &billingv1alpha1.BillingAccountPaymentArrangement{
				Name: "msa", Type: billingv1alpha1.BillingArrangementTypeInvoice,
				Invoice: &billingv1alpha1.InvoiceArrangementTerms{CreditLimit: "10000"},
			},
			wantNetDays: 30,
			wantMethod:  om.CollectionMethodSendInvoice,
		},
		{
			name: "invoice type without terms is ignored",
			arrangement: &billingv1alpha1.BillingAccountPaymentArrangement{
				Name: "msa", Type: billingv1alpha1.BillingArrangementTypeInvoice,
			},
			wantNetDays: 30,
		},
		{
			name: "unknown type is ignored",
			arrangement: &billingv1alpha1.BillingAccountPaymentArrangement{
				Name: "x", Type: "Sponsored",
				Invoice: &billingv1alpha1.InvoiceArrangementTerms{NetDays: 60},
			},
			wantNetDays: 30,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			account := baseBillingAccount()
			account.Status.PaymentArrangement = tt.arrangement

			got := desiredBillingProfileFromAccount(account, "test-account-key")
			if got.NetDays != tt.wantNetDays || got.CollectionMethod != tt.wantMethod {
				t.Errorf("NetDays/CollectionMethod = %d/%q, want %d/%q", got.NetDays, got.CollectionMethod, tt.wantNetDays, tt.wantMethod)
			}
			if got.InvoiceFrequency != "Monthly" || got.InvoiceDayOfMonth != 1 {
				t.Errorf("frequency/day = %s/%d, want Monthly/1", got.InvoiceFrequency, got.InvoiceDayOfMonth)
			}
		})
	}
}

// fakeBillingProfileOpenMeterClient is a minimal openmeter.Client double for
// the billing-profile sync and finalizer. It keeps a tiny model of
// OpenMeter: customers (live and soft-deleted) by key, profiles by id, and
// overrides by customer id. Unimplemented methods panic (nil embedded
// interface), so an unexpected call — e.g. GetCustomer, which cannot see
// soft-deleted customers — fails the test loudly.
type fakeBillingProfileOpenMeterClient struct {
	openmeter.Client

	// customerIDsByKey models CustomerIDsByKey (deleted customers included).
	customerIDsByKey map[openmeter.CustomerKey][]openmeter.CustomerID
	profiles         map[string]om.BillingProfile
	overrides        map[openmeter.CustomerID]string

	calls             []string
	deleteOverrideErr error
	deleteProfileErr  error

	ensureResult openmeter.EnsureBillingProfileResult
	ensureCalls  []string
}

func newFakeBillingProfileClient() *fakeBillingProfileOpenMeterClient {
	return &fakeBillingProfileOpenMeterClient{
		customerIDsByKey: map[openmeter.CustomerKey][]openmeter.CustomerID{},
		profiles:         map[string]om.BillingProfile{},
		overrides:        map[openmeter.CustomerID]string{},
	}
}

func (f *fakeBillingProfileOpenMeterClient) addProfile(id, accountKey string, isDefault bool) {
	var metadata *om.Metadata
	if accountKey != "" {
		metadata = &om.Metadata{"billingAccountKey": accountKey}
	}
	f.profiles[id] = om.BillingProfile{Id: id, Default: isDefault, Metadata: metadata}
}

func (f *fakeBillingProfileOpenMeterClient) CustomerIDsByKey(_ context.Context, key openmeter.CustomerKey) ([]openmeter.CustomerID, error) {
	f.calls = append(f.calls, "customers:"+string(key))
	return f.customerIDsByKey[key], nil
}

func (f *fakeBillingProfileOpenMeterClient) DeleteBillingProfileCustomerOverride(_ context.Context, customerID openmeter.CustomerID) error {
	f.calls = append(f.calls, "deleteOverride:"+string(customerID))
	if f.deleteOverrideErr != nil {
		return f.deleteOverrideErr
	}
	delete(f.overrides, customerID)
	return nil
}

func (f *fakeBillingProfileOpenMeterClient) GetBillingProfile(_ context.Context, id string) (om.BillingProfile, error) {
	f.calls = append(f.calls, "getProfile:"+id)
	p, ok := f.profiles[id]
	if !ok {
		return om.BillingProfile{}, fmt.Errorf("%w: %s", openmeter.ErrBillingProfileNotFound, id)
	}
	return p, nil
}

func (f *fakeBillingProfileOpenMeterClient) FindBillingProfilesByAccountKey(_ context.Context, key openmeter.CustomerKey) ([]om.BillingProfile, error) {
	f.calls = append(f.calls, "findProfiles:"+string(key))
	var out []om.BillingProfile
	for _, id := range sortedProfileIDs(f.profiles) {
		p := f.profiles[id]
		if openmeter.BillingProfileOwnershipMismatch(p, key) == "" {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeBillingProfileOpenMeterClient) DeleteBillingProfile(_ context.Context, id string) error {
	f.calls = append(f.calls, "deleteProfile:"+id)
	if f.deleteProfileErr != nil {
		return f.deleteProfileErr
	}
	for _, profileID := range f.overrides {
		if profileID == id {
			return errors.New("profile is referenced by customer overrides")
		}
	}
	delete(f.profiles, id)
	return nil
}

func (f *fakeBillingProfileOpenMeterClient) EnsureBillingProfile(_ context.Context, existingID string, _ openmeter.DesiredBillingProfile) (openmeter.EnsureBillingProfileResult, error) {
	f.ensureCalls = append(f.ensureCalls, existingID)
	return f.ensureResult, nil
}

func (f *fakeBillingProfileOpenMeterClient) UpsertBillingProfileCustomerOverride(_ context.Context, customerID openmeter.CustomerID, profileID string) error {
	f.overrides[customerID] = profileID
	return nil
}

func sortedProfileIDs[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

const (
	testCustomerID = openmeter.CustomerID("01M23AAAXB1MDSQTY0WWY9P0G8")
	testAccountKey = openmeter.CustomerKey("uid-1")
)

// profileCleanupAccount is an account whose customer-link cleanup has already
// finished (only BillingProfileFinalizer remains).
func profileCleanupAccount(annotations map[string]string) *billingv1alpha1.BillingAccount {
	account := baseBillingAccount()
	account.Finalizers = []string{BillingProfileFinalizer}
	account.Annotations = annotations
	return account
}

func accountWithProfileAnnotations() *billingv1alpha1.BillingAccount {
	return profileCleanupAccount(map[string]string{
		BillingProfileIDAnnotation:    "profile-123",
		OpenMeterCustomerIDAnnotation: string(testCustomerID),
	})
}

// fakeWithAccountProfile models a normal account: its customer, its
// profile, and the override linking them.
func fakeWithAccountProfile() *fakeBillingProfileOpenMeterClient {
	fake := newFakeBillingProfileClient()
	fake.customerIDsByKey[testAccountKey] = []openmeter.CustomerID{testCustomerID}
	fake.addProfile("profile-123", string(testAccountKey), false)
	fake.overrides[testCustomerID] = "profile-123"
	return fake
}

// TestBillingProfileFinalizer_WaitsForCustomerLink: OpenMeter resolves the
// customer's billing profile through the override when the customer-link
// finalizer invoices the pending lines, so nothing may be deleted while
// that finalizer is still on the object (pkg/finalizer runs the two in
// random order).
func TestBillingProfileFinalizer_WaitsForCustomerLink(t *testing.T) {
	fake := fakeWithAccountProfile()
	f := &billingProfileFinalizer{OpenMeterClient: fake}
	account := accountWithProfileAnnotations()
	account.Finalizers = []string{CustomerLinkFinalizer, BillingProfileFinalizer}

	_, err := f.Finalize(context.Background(), account)
	if !errors.Is(err, errWaitingForCustomerLink) {
		t.Fatalf("Finalize error = %v, want errWaitingForCustomerLink", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("OpenMeter calls = %v, want none while the customer link is still being cleaned up", fake.calls)
	}
}

func TestBillingProfileFinalizer_DeletesOverrideThenProfile(t *testing.T) {
	fake := fakeWithAccountProfile()
	f := &billingProfileFinalizer{OpenMeterClient: fake}

	if _, err := f.Finalize(context.Background(), accountWithProfileAnnotations()); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	overrideAt := slices.Index(fake.calls, "deleteOverride:"+string(testCustomerID))
	profileAt := slices.Index(fake.calls, "deleteProfile:profile-123")
	if overrideAt < 0 || profileAt < 0 || overrideAt > profileAt {
		t.Errorf("calls = %v, want the override deleted, then the profile", fake.calls)
	}
	if len(fake.profiles) != 0 || len(fake.overrides) != 0 {
		t.Errorf("left behind profiles=%v overrides=%v", fake.profiles, fake.overrides)
	}
}

// TestBillingProfileFinalizer_DeletesOverrideEvenAfterCustomerIsGone: by
// the time this finalizer runs, the customer-link finalizer has deleted
// the OpenMeter customer. OpenMeter does not cascade that to the override,
// which keeps blocking the profile's deletion ("profile is referenced by
// customer overrides"), and GetCustomer can no longer see the customer.
// The fake therefore models the customer as soft-deleted — reachable only
// through CustomerIDsByKey (deleted customers included), GetCustomer
// unimplemented — and the override still in place.
func TestBillingProfileFinalizer_DeletesOverrideEvenAfterCustomerIsGone(t *testing.T) {
	fake := newFakeBillingProfileClient()
	const deletedCustomer = openmeter.CustomerID("01DELETEDCUSTOMER000000000")
	fake.customerIDsByKey[testAccountKey] = []openmeter.CustomerID{deletedCustomer}
	fake.addProfile("profile-123", string(testAccountKey), false)
	fake.overrides[deletedCustomer] = "profile-123"
	f := &billingProfileFinalizer{OpenMeterClient: fake}

	account := profileCleanupAccount(map[string]string{
		BillingProfileIDAnnotation:    "profile-123",
		OpenMeterCustomerIDAnnotation: string(deletedCustomer),
	})
	if _, err := f.Finalize(context.Background(), account); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if _, ok := fake.overrides[deletedCustomer]; ok {
		t.Error("override of the soft-deleted customer survived")
	}
	if _, ok := fake.profiles["profile-123"]; ok {
		t.Error("profile survived")
	}
}

// TestBillingProfileFinalizer_OrphanWithoutAnnotations: the account was
// deleted after its profile was created but before the annotations were
// persisted. The profile and override are found by the account key
// instead of being orphaned.
func TestBillingProfileFinalizer_OrphanWithoutAnnotations(t *testing.T) {
	fake := fakeWithAccountProfile()
	f := &billingProfileFinalizer{OpenMeterClient: fake}

	if _, err := f.Finalize(context.Background(), profileCleanupAccount(nil)); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if len(fake.profiles) != 0 || len(fake.overrides) != 0 {
		t.Errorf("orphaned profiles=%v overrides=%v", fake.profiles, fake.overrides)
	}
}

// TestBillingProfileFinalizer_IgnoresTamperedAnnotations is the M2
// regression for the deletion path: the account's users can edit its
// annotations, so pointing them at another account's customer or profile
// (or the org default profile) must not delete those.
func TestBillingProfileFinalizer_IgnoresTamperedAnnotations(t *testing.T) {
	for _, tt := range []struct {
		name          string
		profileTarget string
	}{
		{"another account's profile", "victim-profile"},
		{"the default profile", "profile_default"},
		{"an untagged profile", "untagged-profile"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := fakeWithAccountProfile()
			const victimCustomer = openmeter.CustomerID("01VICTIM000000000000000000")
			fake.customerIDsByKey["victim-uid"] = []openmeter.CustomerID{victimCustomer}
			fake.addProfile("victim-profile", "victim-uid", false)
			fake.addProfile("profile_default", "", true)
			fake.addProfile("untagged-profile", "", false)
			fake.overrides[victimCustomer] = "victim-profile"
			recorder := record.NewFakeRecorder(10)
			f := &billingProfileFinalizer{OpenMeterClient: fake, Recorder: recorder}

			account := profileCleanupAccount(map[string]string{
				BillingProfileIDAnnotation:    tt.profileTarget,
				OpenMeterCustomerIDAnnotation: string(victimCustomer),
			})
			if _, err := f.Finalize(context.Background(), account); err != nil {
				t.Fatalf("Finalize: %v", err)
			}

			if _, ok := fake.overrides[victimCustomer]; !ok {
				t.Error("deleted the victim customer's override")
			}
			if _, ok := fake.profiles[tt.profileTarget]; !ok {
				t.Errorf("deleted %s", tt.profileTarget)
			}
			if _, ok := fake.profiles["profile-123"]; ok {
				t.Error("the account's own profile (found by key) was not deleted")
			}
			if _, ok := fake.overrides[testCustomerID]; ok {
				t.Error("the account's own override was not deleted")
			}
			events := drainProfileEvents(recorder)
			if len(events) != 2 || !strings.Contains(strings.Join(events, "\n"), "Warning "+EventReasonBillingProfileDrift) {
				t.Errorf("events = %v, want a Warning for each refused annotation", events)
			}
		})
	}
}

func TestBillingProfileFinalizer_NothingToCleanUpIsNoop(t *testing.T) {
	fake := newFakeBillingProfileClient()
	f := &billingProfileFinalizer{OpenMeterClient: fake}
	if _, err := f.Finalize(context.Background(), profileCleanupAccount(nil)); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	for _, call := range fake.calls {
		if strings.HasPrefix(call, "delete") {
			t.Errorf("unexpected delete call %s", call)
		}
	}
}

func TestBillingProfileFinalizer_OverrideFailureKeepsProfile(t *testing.T) {
	fake := fakeWithAccountProfile()
	fake.deleteOverrideErr = &openmeter.TransientError{Err: errors.New("unavailable")}
	f := &billingProfileFinalizer{OpenMeterClient: fake}

	if _, err := f.Finalize(context.Background(), accountWithProfileAnnotations()); err == nil {
		t.Fatal("Finalize error = nil, want the override failure")
	}
	if slices.Contains(fake.calls, "deleteProfile:profile-123") {
		t.Error("deleted the profile although its override could not be removed")
	}
}

func drainProfileEvents(recorder *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case e := <-recorder.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}

// TestReconcileBillingProfile_WarnsAndReannotatesOnRefusedProfile: when
// EnsureBillingProfile refuses the annotated profile, the reconcile emits a
// Warning and replaces the annotation with the account's real profile.
func TestReconcileBillingProfile_WarnsAndReannotatesOnRefusedProfile(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := billingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	account := baseBillingAccount()
	account.Annotations = map[string]string{BillingProfileIDAnnotation: "victim-profile"}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(account).Build()

	omc := newFakeBillingProfileClient()
	omc.ensureResult = openmeter.EnsureBillingProfileResult{
		Profile:               om.BillingProfile{Id: "own-profile"},
		IgnoredExistingID:     "victim-profile",
		IgnoredExistingReason: "billing profile victim-profile belongs to billing account victim-uid",
		AppsDrift:             "apps differ",
	}
	recorder := record.NewFakeRecorder(10)
	r := &BillingAccountReconciler{Client: k8s, OpenMeterClient: omc, Recorder: recorder}

	var stored billingv1alpha1.BillingAccount
	if err := k8s.Get(context.Background(), types.NamespacedName{Name: account.Name, Namespace: account.Namespace}, &stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := r.reconcileBillingProfile(context.Background(), &stored, testAccountKey, testCustomerID); err != nil {
		t.Fatalf("reconcileBillingProfile: %v", err)
	}

	if err := k8s.Get(context.Background(), types.NamespacedName{Name: account.Name, Namespace: account.Namespace}, &stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := stored.Annotations[BillingProfileIDAnnotation]; got != "own-profile" {
		t.Errorf("annotation = %q, want own-profile", got)
	}
	if got := stored.Annotations[OpenMeterCustomerIDAnnotation]; got != string(testCustomerID) {
		t.Errorf("customer annotation = %q, want %s", got, testCustomerID)
	}
	if omc.overrides[testCustomerID] != "own-profile" {
		t.Errorf("override = %q, want own-profile", omc.overrides[testCustomerID])
	}
	events := drainProfileEvents(recorder)
	if len(events) != 2 {
		t.Fatalf("events = %v, want a Warning for the refused annotation and one for app drift", events)
	}
	for _, e := range events {
		if !strings.HasPrefix(e, "Warning "+EventReasonBillingProfileDrift) {
			t.Errorf("event %q, want a %s Warning", e, EventReasonBillingProfileDrift)
		}
	}
}
