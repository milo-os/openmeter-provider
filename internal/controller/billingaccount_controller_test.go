// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	om "github.com/openmeterio/openmeter/api/client/go"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
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

	nameA := desiredCustomerFromAccount(orgA, openmeter.CustomerKey(orgA.UID), nil).Name
	nameB := desiredCustomerFromAccount(orgB, openmeter.CustomerKey(orgB.UID), nil).Name
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

// TestTruncateCustomerName: OpenMeter caps customer names at 256
// characters; the namespace/name fallback can exceed that, which used to be
// a permanent 400 on every reconcile.
func TestTruncateCustomerName(t *testing.T) {
	short := "org-a/acme-account"
	if got := truncateCustomerName(short); got != short {
		t.Errorf("short name changed: %q", got)
	}

	exact := strings.Repeat("a", customerNameMaxLength)
	if got := truncateCustomerName(exact); got != exact {
		t.Error("a name of exactly the limit was changed")
	}

	long := strings.Repeat("n", 63) + "/" + strings.Repeat("x", 253)
	got := truncateCustomerName(long)
	if n := utf8.RuneCountInString(got); n != customerNameMaxLength {
		t.Errorf("truncated length = %d, want %d", n, customerNameMaxLength)
	}
	if got != truncateCustomerName(long) {
		t.Error("truncation is not deterministic")
	}
	if !strings.HasPrefix(got, strings.Repeat("n", 63)+"/") {
		t.Errorf("truncated name %q lost its prefix", got)
	}
	other := strings.Repeat("n", 63) + "/" + strings.Repeat("x", 252) + "y"
	if truncateCustomerName(other) == got {
		t.Error("two long names sharing a prefix truncated to the same name")
	}

	// Counted in runes, like OpenMeter's validator, not bytes.
	multibyte := strings.Repeat("é", 300)
	if n := utf8.RuneCountInString(truncateCustomerName(multibyte)); n != customerNameMaxLength {
		t.Errorf("multibyte truncated length = %d runes, want %d", n, customerNameMaxLength)
	}

	account := baseBillingAccount()
	account.Namespace = strings.Repeat("n", 63)
	account.Name = strings.Repeat("x", 253)
	if n := utf8.RuneCountInString(desiredCustomerFromAccount(account, "uid-1", nil).Name); n > customerNameMaxLength {
		t.Errorf("desired name length = %d, want <= %d", n, customerNameMaxLength)
	}
}

// syncOpenMeterClient is an in-memory OpenMeter for the non-deleting
// Reconcile path.
type syncOpenMeterClient struct {
	openmeter.Client

	customers map[openmeter.CustomerKey]om.Customer
	// conflicting subject keys are rejected with ErrSubjectKeyConflict, as
	// if attributed to another customer.
	conflicting []string
	ensureCalls []openmeter.DesiredCustomer
	writes      int

	stripeAppData          map[openmeter.CustomerID]om.StripeCustomerAppData
	stripeAppNotConfigured bool
	stripeEnsureCalls      []string
	stripeGetCalls         int
}

func newSyncOpenMeterClient() *syncOpenMeterClient {
	return &syncOpenMeterClient{
		customers:     map[openmeter.CustomerKey]om.Customer{},
		stripeAppData: map[openmeter.CustomerID]om.StripeCustomerAppData{},
	}
}

func (f *syncOpenMeterClient) EnsureCustomer(_ context.Context, desired openmeter.DesiredCustomer) (om.Customer, error) {
	f.ensureCalls = append(f.ensureCalls, desired)
	for _, k := range desired.SubjectKeys {
		if slices.Contains(f.conflicting, k) {
			return om.Customer{}, &openmeter.TransientError{
				Err: fmt.Errorf("%w: %s", openmeter.ErrSubjectKeyConflict, k), StatusCode: 409,
			}
		}
	}
	existing, ok := f.customers[desired.Key]
	email := desired.Email
	want := om.Customer{
		Id:               "id-" + string(desired.Key),
		Name:             desired.Name,
		PrimaryEmail:     &email,
		UsageAttribution: &om.CustomerUsageAttribution{SubjectKeys: append([]string{}, desired.SubjectKeys...)},
		UpdatedAt:        existing.UpdatedAt,
	}
	if ok && existing.Name == want.Name && derefString(existing.PrimaryEmail) == email &&
		slices.Equal(existing.UsageAttribution.SubjectKeys, want.UsageAttribution.SubjectKeys) {
		return existing, nil
	}
	f.writes++
	want.UpdatedAt = time.Date(2026, 1, 1, 0, 0, f.writes, 0, time.UTC)
	f.customers[desired.Key] = want
	return want, nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (f *syncOpenMeterClient) GetCustomer(_ context.Context, key openmeter.CustomerKey) (om.Customer, error) {
	c, ok := f.customers[key]
	if !ok {
		return om.Customer{}, openmeter.ErrCustomerNotFound
	}
	return c, nil
}

func (f *syncOpenMeterClient) EnsureBillingProfile(_ context.Context, _ string, _ openmeter.DesiredBillingProfile) (om.BillingProfile, error) {
	return om.BillingProfile{Id: "profile-1"}, nil
}

func (f *syncOpenMeterClient) UpsertBillingProfileCustomerOverride(_ context.Context, _ openmeter.CustomerID, _ string) error {
	return nil
}

func (f *syncOpenMeterClient) ListInvoices(_ context.Context, _ openmeter.CustomerID) ([]om.Invoice, error) {
	return nil, nil
}

func (f *syncOpenMeterClient) EnsureCustomerStripeAppData(_ context.Context, id openmeter.CustomerID, cus openmeter.StripeCustomerID, pm string) error {
	if f.stripeAppNotConfigured {
		return &openmeter.PermanentError{Err: fmt.Errorf("%w: no stripe app", openmeter.ErrStripeAppNotConfigured), StatusCode: 412}
	}
	f.stripeEnsureCalls = append(f.stripeEnsureCalls, string(cus)+"/"+pm)
	data := om.StripeCustomerAppData{StripeCustomerId: string(cus)}
	if pm != "" {
		data.StripeDefaultPaymentMethodId = &pm
	}
	f.stripeAppData[id] = data
	return nil
}

func (f *syncOpenMeterClient) GetCustomerStripeAppData(_ context.Context, id openmeter.CustomerID) (om.StripeCustomerAppData, error) {
	f.stripeGetCalls++
	if f.stripeAppNotConfigured {
		return om.StripeCustomerAppData{}, &openmeter.PermanentError{Err: fmt.Errorf("%w: no stripe app", openmeter.ErrStripeAppNotConfigured), StatusCode: 412}
	}
	data, ok := f.stripeAppData[id]
	if !ok {
		return om.StripeCustomerAppData{}, &openmeter.PermanentError{Err: openmeter.ErrStripeAppDataNotFound, StatusCode: 412}
	}
	return data, nil
}

// syncHarness reconciles one live BillingAccount against syncOpenMeterClient.
type syncHarness struct {
	r        *BillingAccountReconciler
	k8s      client.Client
	omc      *syncOpenMeterClient
	recorder *record.FakeRecorder
	req      reconcile.Request
}

func newSyncHarness(t *testing.T, account *billingv1alpha1.BillingAccount, objs ...client.Object) *syncHarness {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := billingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := stripev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(stripe): %v", err)
	}
	// Already carrying the finalizer, so a pass goes straight to syncing.
	account.Finalizers = []string{CustomerLinkFinalizer}
	k8s := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(append([]client.Object{account}, objs...)...).
		WithIndex(&billingv1alpha1.BillingAccountBinding{}, BindingBillingAccountRefField, func(obj client.Object) []string {
			return []string{obj.(*billingv1alpha1.BillingAccountBinding).Spec.BillingAccountRef.Name}
		}).
		Build()
	omc := newSyncOpenMeterClient()
	recorder := record.NewFakeRecorder(100)
	r := &BillingAccountReconciler{Client: k8s, OpenMeterClient: omc, Recorder: recorder}
	r.Finalizers = finalizer.NewFinalizers()
	if err := r.Finalizers.Register(CustomerLinkFinalizer, &customerLinkFinalizer{OpenMeterClient: omc, Recorder: recorder}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return &syncHarness{
		r: r, k8s: k8s, omc: omc, recorder: recorder,
		req: reconcile.Request{NamespacedName: types.NamespacedName{Namespace: account.Namespace, Name: account.Name}},
	}
}

func (h *syncHarness) reconcile(t *testing.T) reconcile.Result {
	t.Helper()
	res, err := h.r.Reconcile(context.Background(), h.req)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func (h *syncHarness) events() []string {
	var out []string
	for {
		select {
		case e := <-h.recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// TestReconcile_SyncedOnlyWhenSomethingChanged: billing's own status writes
// and the periodic requeue re-run Reconcile constantly; Synced is emitted on
// the first sync and when the remote state changes, not on every pass.
// Every successful pass requeues after syncedRequeueAfter so OpenMeter
// invoice status changes reach Milo without an unrelated event.
func TestReconcile_SyncedOnlyWhenSomethingChanged(t *testing.T) {
	account := baseBillingAccount()
	account.Spec.ContactInfo = &billingv1alpha1.BillingContactInfo{Email: "a@example.com"}
	h := newSyncHarness(t, account)

	for range 3 {
		if res := h.reconcile(t); res.RequeueAfter != syncedRequeueAfter {
			t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, syncedRequeueAfter)
		}
	}
	if n := countEvents(h.events(), EventReasonSynced); n != 1 {
		t.Errorf("%d Synced events across 3 passes with one change, want 1", n)
	}

	var current billingv1alpha1.BillingAccount
	if err := h.k8s.Get(context.Background(), h.req.NamespacedName, &current); err != nil {
		t.Fatalf("get: %v", err)
	}
	current.Spec.ContactInfo.Email = "b@example.com"
	if err := h.k8s.Update(context.Background(), &current); err != nil {
		t.Fatalf("update: %v", err)
	}
	h.reconcile(t)
	if n := countEvents(h.events(), EventReasonSynced); n != 1 {
		t.Errorf("%d Synced events after the customer changed, want 1", n)
	}
}

func activeBinding(account *billingv1alpha1.BillingAccount, project string) *billingv1alpha1.BillingAccountBinding {
	return &billingv1alpha1.BillingAccountBinding{
		ObjectMeta: metav1.ObjectMeta{Name: project, Namespace: account.Namespace},
		Spec: billingv1alpha1.BillingAccountBindingSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: account.Name},
			ProjectRef:        billingv1alpha1.ProjectRef{Name: project},
		},
		Status: billingv1alpha1.BillingAccountBindingStatus{Phase: billingv1alpha1.BillingAccountBindingPhaseActive},
	}
}

// TestReconcile_SubjectKeyConflictSyncsTheRest: a project still attributed
// to another account's customer must not block this account's customer,
// billing profile, Stripe and invoice sync. The customer is synced with the
// keys it already holds, and the conflict is retried shortly and reported
// once.
func TestReconcile_SubjectKeyConflictSyncsTheRest(t *testing.T) {
	account := baseBillingAccount()
	h := newSyncHarness(t, account, activeBinding(account, "held"), activeBinding(account, "moved"))
	email := ""
	h.omc.customers["uid-1"] = om.Customer{
		Id: "id-uid-1", Name: "org-a/acme-account", PrimaryEmail: &email,
		UsageAttribution: &om.CustomerUsageAttribution{SubjectKeys: []string{"projects/held"}},
	}
	h.omc.conflicting = []string{"projects/moved"}

	for range 2 {
		res := h.reconcile(t)
		if res.RequeueAfter != subjectKeyConflictRequeueAfter {
			t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, subjectKeyConflictRequeueAfter)
		}
	}
	last := h.omc.ensureCalls[len(h.omc.ensureCalls)-1]
	if !slices.Equal(last.SubjectKeys, []string{"projects/held"}) {
		t.Errorf("fallback subject keys = %v, want only the held one", last.SubjectKeys)
	}
	var current billingv1alpha1.BillingAccount
	if err := h.k8s.Get(context.Background(), h.req.NamespacedName, &current); err != nil {
		t.Fatalf("get: %v", err)
	}
	if current.Annotations[BillingProfileIDAnnotation] != "profile-1" {
		t.Error("billing profile not synced while a subject key conflicted")
	}
	events := h.events()
	if n := countEvents(events, EventReasonSubjectKeyConflict); n != 1 {
		t.Errorf("%d SubjectKeyConflict events across 2 passes, want 1: %v", n, events)
	}
	if n := countEvents(events, EventReasonSyncFailed); n != 0 {
		t.Errorf("%d SyncFailed events, want 0: %v", n, events)
	}

	// Once the other customer lets go, the key is attributed here.
	h.omc.conflicting = nil
	if res := h.reconcile(t); res.RequeueAfter != syncedRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v once the conflict cleared", res.RequeueAfter, syncedRequeueAfter)
	}
	if got := h.omc.customers["uid-1"].UsageAttribution.SubjectKeys; !slices.Equal(got, []string{"projects/held", "projects/moved"}) {
		t.Errorf("subject keys = %v, want both", got)
	}
}

// TestReconcile_SubjectKeyConflictOnCreate: same, for an account whose
// customer doesn't exist yet — it is created without the conflicting keys.
func TestReconcile_SubjectKeyConflictOnCreate(t *testing.T) {
	account := baseBillingAccount()
	h := newSyncHarness(t, account, activeBinding(account, "moved"))
	h.omc.conflicting = []string{"projects/moved"}

	h.reconcile(t)
	c, ok := h.omc.customers["uid-1"]
	if !ok {
		t.Fatal("customer not created")
	}
	if len(c.UsageAttribution.SubjectKeys) != 0 {
		t.Errorf("subject keys = %v, want none yet", c.UsageAttribution.SubjectKeys)
	}
}

// paymentMethodObjects returns an Active PaymentMethod and its
// StripePaymentMethod for account.
func paymentMethodObjects(account *billingv1alpha1.BillingAccount) []client.Object {
	pm := &billingv1alpha1.PaymentMethod{
		ObjectMeta: metav1.ObjectMeta{Name: "pm-1", Namespace: account.Namespace},
		Spec:       billingv1alpha1.PaymentMethodSpec{BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: account.Name}},
		Status:     billingv1alpha1.PaymentMethodStatus{Phase: billingv1alpha1.PaymentMethodPhaseActive},
	}
	spm := &stripev1alpha1.StripePaymentMethod{
		ObjectMeta: metav1.ObjectMeta{Name: "pm-1", Namespace: account.Namespace},
		Spec:       stripev1alpha1.StripePaymentMethodSpec{PaymentMethodRef: stripev1alpha1.PaymentMethodLocalRef{Name: "pm-1"}},
		Status:     stripev1alpha1.StripePaymentMethodStatus{StripeCustomerID: "cus_1", StripePaymentMethodID: "pm_card"},
	}
	return []client.Object{pm, spm}
}

func withDefaultPaymentMethod(account *billingv1alpha1.BillingAccount, status metav1.ConditionStatus) {
	account.Spec.DefaultPaymentMethodRef = &billingv1alpha1.DefaultPaymentMethodRef{Name: "pm-1"}
	account.Status.Conditions = []metav1.Condition{{
		Type:   billingv1alpha1.BillingAccountConditionDefaultPaymentMethodReady,
		Status: status, Reason: "Test", LastTransitionTime: metav1.Now(),
	}}
}

func TestReconcile_SyncsStripeAppData(t *testing.T) {
	account := baseBillingAccount()
	withDefaultPaymentMethod(account, metav1.ConditionTrue)
	h := newSyncHarness(t, account, paymentMethodObjects(account)...)

	h.reconcile(t)
	if !slices.Equal(h.omc.stripeEnsureCalls, []string{"cus_1/pm_card"}) {
		t.Errorf("stripe upserts = %v, want cus_1/pm_card", h.omc.stripeEnsureCalls)
	}
}

// TestReconcile_ClearsStaleDefaultPaymentMethod: once the default payment
// method is no longer usable, the stored one (a card stripe-provider has
// detached) must be cleared, keeping the Stripe customer.
func TestReconcile_ClearsStaleDefaultPaymentMethod(t *testing.T) {
	account := baseBillingAccount()
	withDefaultPaymentMethod(account, metav1.ConditionFalse)
	h := newSyncHarness(t, account, paymentMethodObjects(account)...)
	pm := "pm_detached"
	h.omc.stripeAppData["id-uid-1"] = om.StripeCustomerAppData{StripeCustomerId: "cus_1", StripeDefaultPaymentMethodId: &pm}

	h.reconcile(t)
	if !slices.Equal(h.omc.stripeEnsureCalls, []string{"cus_1/"}) {
		t.Errorf("stripe upserts = %v, want the payment method cleared on cus_1", h.omc.stripeEnsureCalls)
	}
	h.reconcile(t)
	if len(h.omc.stripeEnsureCalls) != 1 {
		t.Errorf("stripe upserts = %v, want no further write once cleared", h.omc.stripeEnsureCalls)
	}
}

// TestReconcile_UnknownPaymentMethodStateLeavesStripeDataAlone: a missing
// or Unknown DefaultPaymentMethodReady isn't evidence the card is gone.
func TestReconcile_UnknownPaymentMethodStateLeavesStripeDataAlone(t *testing.T) {
	account := baseBillingAccount()
	withDefaultPaymentMethod(account, metav1.ConditionUnknown)
	h := newSyncHarness(t, account, paymentMethodObjects(account)...)
	pm := "pm_card"
	h.omc.stripeAppData["id-uid-1"] = om.StripeCustomerAppData{StripeCustomerId: "cus_1", StripeDefaultPaymentMethodId: &pm}

	h.reconcile(t)
	if len(h.omc.stripeEnsureCalls) != 0 || h.omc.stripeGetCalls != 0 {
		t.Errorf("stripe upserts = %v, reads = %d; want neither", h.omc.stripeEnsureCalls, h.omc.stripeGetCalls)
	}
}

// TestReconcile_NoStripeCustomerKnownIsANoop: no payment method and nothing
// stored in OpenMeter — nothing to write.
func TestReconcile_NoStripeCustomerKnownIsANoop(t *testing.T) {
	h := newSyncHarness(t, baseBillingAccount())

	h.reconcile(t)
	if len(h.omc.stripeEnsureCalls) != 0 {
		t.Errorf("stripe upserts = %v, want none", h.omc.stripeEnsureCalls)
	}
	if n := countEvents(h.events(), EventReasonStripeCustomerMissing); n != 0 {
		t.Errorf("%d StripeCustomerMissing events for a card account, want 0", n)
	}
}

// TestReconcile_InvoiceArrangementWithoutStripeCustomerWarns: an
// invoice-billed account with no Stripe customer can't be invoiced through
// OpenMeter's Stripe app, and nothing here can fix that — it is reported
// once, not failed.
func TestReconcile_InvoiceArrangementWithoutStripeCustomerWarns(t *testing.T) {
	account := baseBillingAccount()
	account.Status.PaymentArrangement = &billingv1alpha1.BillingAccountPaymentArrangement{
		Name: "msa", Type: billingv1alpha1.BillingArrangementTypeInvoice,
	}
	h := newSyncHarness(t, account)

	for range 2 {
		if res := h.reconcile(t); res.RequeueAfter != syncedRequeueAfter {
			t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, syncedRequeueAfter)
		}
	}
	events := h.events()
	if n := countEvents(events, EventReasonStripeCustomerMissing); n != 1 {
		t.Errorf("%d StripeCustomerMissing events across 2 passes, want 1: %v", n, events)
	}
	if n := countEvents(events, EventReasonSynced); n != 1 {
		t.Errorf("%d Synced events, want 1: %v", n, events)
	}
}

// TestReconcile_StripeAppNotConfiguredDoesNotFailTheSync: a billing profile
// without a Stripe app (dev/sandbox) used to fail every sync with a 412 and
// suppress Synced. It is reported once instead.
func TestReconcile_StripeAppNotConfiguredDoesNotFailTheSync(t *testing.T) {
	account := baseBillingAccount()
	withDefaultPaymentMethod(account, metav1.ConditionTrue)
	h := newSyncHarness(t, account, paymentMethodObjects(account)...)
	h.omc.stripeAppNotConfigured = true

	for range 2 {
		if res := h.reconcile(t); res.RequeueAfter != syncedRequeueAfter {
			t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, syncedRequeueAfter)
		}
	}
	events := h.events()
	if n := countEvents(events, EventReasonStripeAppNotConfigured); n != 1 {
		t.Errorf("%d StripeAppNotConfigured events across 2 passes, want 1: %v", n, events)
	}
	if n := countEvents(events, EventReasonSynced); n != 1 {
		t.Errorf("%d Synced events, want 1: %v", n, events)
	}
	if n := countEvents(events, EventReasonSyncFailed); n != 0 {
		t.Errorf("%d SyncFailed events, want 0: %v", n, events)
	}
}
