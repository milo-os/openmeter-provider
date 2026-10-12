// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	om "github.com/openmeterio/openmeter/api/client/go"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

// partialFailureOpenMeterClient lets the customer cleanup succeed while the
// billing-profile cleanup keeps failing — the exact split observed in
// production when the profile delete was wedged.
type partialFailureOpenMeterClient struct {
	openmeter.Client // panics on any unimplemented method if called

	deleteCustomerCalls int
}

func (f *partialFailureOpenMeterClient) GetCustomer(_ context.Context, key openmeter.CustomerKey) (om.Customer, error) {
	return om.Customer{Id: "id-" + string(key)}, nil
}

func (f *partialFailureOpenMeterClient) PrepareInvoicesForCustomerDeletion(_ context.Context, _ openmeter.CustomerID) error {
	return nil
}

func (f *partialFailureOpenMeterClient) CancelSubscriptions(_ context.Context, _ openmeter.CustomerKey) error {
	return nil
}

func (f *partialFailureOpenMeterClient) ListSubscriptions(_ context.Context, _ openmeter.CustomerKey) ([]om.Subscription, error) {
	return nil, nil
}

func (f *partialFailureOpenMeterClient) ReleaseCustomerSubjects(_ context.Context, _ openmeter.CustomerKey) error {
	return nil
}

func (f *partialFailureOpenMeterClient) DeleteCustomer(_ context.Context, _ openmeter.CustomerKey) error {
	f.deleteCustomerCalls++
	return nil
}

func (f *partialFailureOpenMeterClient) DeleteBillingProfileCustomerOverride(_ context.Context, _ openmeter.CustomerID) error {
	return nil
}

func (f *partialFailureOpenMeterClient) DeleteBillingProfile(_ context.Context, _ string) error {
	return errors.New("profile is referenced by customer overrides")
}

func (f *partialFailureOpenMeterClient) CustomerIDsByKey(_ context.Context, key openmeter.CustomerKey) ([]openmeter.CustomerID, error) {
	return accountCustomerIDs(key), nil
}

func (f *partialFailureOpenMeterClient) GetBillingProfile(_ context.Context, id string) (om.BillingProfile, error) {
	return accountProfile(id), nil
}

func (f *partialFailureOpenMeterClient) FindBillingProfilesByAccountKey(_ context.Context, _ openmeter.CustomerKey) ([]om.BillingProfile, error) {
	return []om.BillingProfile{accountProfile("profile-123")}, nil
}

// accountCustomerIDs and accountProfile answer the billing-profile
// finalizer's ownership lookups for the "uid-1" account used below.
func accountCustomerIDs(key openmeter.CustomerKey) []openmeter.CustomerID {
	if key != "uid-1" {
		return nil
	}
	return []openmeter.CustomerID{"01M23AAAXB1MDSQTY0WWY9P0G8"}
}

func accountProfile(id string) om.BillingProfile {
	return om.BillingProfile{Id: id, Metadata: &om.Metadata{"billingAccountKey": "uid-1"}}
}

// TestReconcile_PersistsSucceededFinalizerWhenSiblingFails is the regression
// test for a bug review found and the production logs had been showing all
// along: "OpenMeter customer deleted" repeating for the same account, pass
// after pass.
//
// Finalizers.Finalize runs EVERY registered finalizer and can return both
// "A succeeded and was removed from the object" and "B failed". Checking the
// error before persisting threw away A's removal, so A's remote cleanup was
// re-run on every retry until B finally succeeded — re-issuing remote calls
// and re-emitting Deleted events indefinitely.
func TestReconcile_PersistsSucceededFinalizerWhenSiblingFails(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := billingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	now := metav1.Now()
	account := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "acme-account",
			Namespace:         "org-a",
			UID:               types.UID("uid-1"),
			DeletionTimestamp: &now,
			Finalizers:        []string{CustomerLinkFinalizer, BillingProfileFinalizer},
			Annotations: map[string]string{
				BillingProfileIDAnnotation:    "profile-123",
				OpenMeterCustomerIDAnnotation: "01M23AAAXB1MDSQTY0WWY9P0G8",
			},
		},
		Spec: billingv1alpha1.BillingAccountSpec{CurrencyCode: "USD"},
	}

	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(account).Build()
	omc := &partialFailureOpenMeterClient{}

	r := &BillingAccountReconciler{Client: k8s, OpenMeterClient: omc}
	r.Finalizers = finalizer.NewFinalizers()
	if err := r.Finalizers.Register(CustomerLinkFinalizer, &customerLinkFinalizer{OpenMeterClient: omc}); err != nil {
		t.Fatalf("register customer finalizer: %v", err)
	}
	if err := r.Finalizers.Register(BillingProfileFinalizer, &billingProfileFinalizer{OpenMeterClient: omc}); err != nil {
		t.Fatalf("register billing profile finalizer: %v", err)
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "acme-account", Namespace: "org-a"}}

	// The customer cleanup succeeds and the profile cleanup fails. The
	// billing-profile finalizer waits (quietly) while the customer-link
	// finalizer is still present, and pkg/finalizer runs them in random
	// order, so the failure surfaces on the first or the second pass.
	surfaced := false
	for pass := 1; pass <= 2 && !surfaced; pass++ {
		_, err := r.Reconcile(context.Background(), req)
		surfaced = err != nil
	}
	if !surfaced {
		t.Fatal("Reconcile: expected the wedged billing-profile finalizer to surface an error")
	}

	var afterFirst billingv1alpha1.BillingAccount
	if err := k8s.Get(context.Background(), req.NamespacedName, &afterFirst); err != nil {
		t.Fatalf("get after first reconcile: %v", err)
	}
	if slices.Contains(afterFirst.Finalizers, CustomerLinkFinalizer) {
		t.Errorf("finalizers = %v, want %q dropped — its cleanup succeeded, so the removal must be persisted even though a sibling failed",
			afterFirst.Finalizers, CustomerLinkFinalizer)
	}
	if !slices.Contains(afterFirst.Finalizers, BillingProfileFinalizer) {
		t.Errorf("finalizers = %v, want %q retained so its cleanup is retried", afterFirst.Finalizers, BillingProfileFinalizer)
	}

	// Second pass: the already-completed customer cleanup must NOT run again.
	callsAfterFirst := omc.deleteCustomerCalls
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("Reconcile: expected the billing-profile finalizer to still be failing")
	}
	if omc.deleteCustomerCalls != callsAfterFirst {
		t.Errorf("DeleteCustomer called %d times by the second pass (was %d) — a finalizer that already succeeded is being re-run every retry",
			omc.deleteCustomerCalls, callsAfterFirst)
	}
}

// TestReconcile_RemovesAllFinalizersWhenCleanupSucceeds is the happy path:
// with nothing failing, both finalizers come off and the object is released.
func TestReconcile_RemovesAllFinalizersWhenCleanupSucceeds(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := billingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	now := metav1.Now()
	account := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "acme-account",
			Namespace:         "org-a",
			UID:               types.UID("uid-1"),
			DeletionTimestamp: &now,
			Finalizers:        []string{CustomerLinkFinalizer, BillingProfileFinalizer},
			Annotations: map[string]string{
				BillingProfileIDAnnotation:    "profile-123",
				OpenMeterCustomerIDAnnotation: "01M23AAAXB1MDSQTY0WWY9P0G8",
			},
		},
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(account).Build()
	omc := &allSucceedOpenMeterClient{}

	r := &BillingAccountReconciler{Client: k8s, OpenMeterClient: omc}
	r.Finalizers = finalizer.NewFinalizers()
	_ = r.Finalizers.Register(CustomerLinkFinalizer, &customerLinkFinalizer{OpenMeterClient: omc})
	_ = r.Finalizers.Register(BillingProfileFinalizer, &billingProfileFinalizer{OpenMeterClient: omc})

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "acme-account", Namespace: "org-a"}}
	// The billing-profile finalizer waits for the customer-link finalizer
	// (pkg/finalizer runs them in random order), so a second pass may be
	// needed; a waiting pass is not a failure.
	for pass := 1; pass <= 2; pass++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("Reconcile pass %d: %v (a waiting finalizer must requeue quietly)", pass, err)
		}
		var cur billingv1alpha1.BillingAccount
		if err := k8s.Get(context.Background(), req.NamespacedName, &cur); apierrors.IsNotFound(err) || len(cur.Finalizers) == 0 {
			break
		}
	}

	// Once the last finalizer is dropped the API server completes the
	// deletion, so NotFound is the expected terminal state. If the object
	// does still exist, it must at least carry no finalizers.
	var after billingv1alpha1.BillingAccount
	switch err := k8s.Get(context.Background(), req.NamespacedName, &after); {
	case apierrors.IsNotFound(err):
		// Fully deleted — nothing left to assert.
	case err != nil:
		t.Fatalf("get after reconcile: %v", err)
	case len(after.Finalizers) > 0:
		t.Errorf("finalizers = %v, want all removed", after.Finalizers)
	}
}

type allSucceedOpenMeterClient struct {
	openmeter.Client
}

func (f *allSucceedOpenMeterClient) GetCustomer(_ context.Context, key openmeter.CustomerKey) (om.Customer, error) {
	return om.Customer{Id: "id-" + string(key)}, nil
}
func (f *allSucceedOpenMeterClient) PrepareInvoicesForCustomerDeletion(_ context.Context, _ openmeter.CustomerID) error {
	return nil
}
func (f *allSucceedOpenMeterClient) CancelSubscriptions(_ context.Context, _ openmeter.CustomerKey) error {
	return nil
}
func (f *allSucceedOpenMeterClient) ListSubscriptions(_ context.Context, _ openmeter.CustomerKey) ([]om.Subscription, error) {
	return nil, nil
}
func (f *allSucceedOpenMeterClient) ReleaseCustomerSubjects(_ context.Context, _ openmeter.CustomerKey) error {
	return nil
}

func (f *allSucceedOpenMeterClient) DeleteCustomer(_ context.Context, _ openmeter.CustomerKey) error {
	return nil
}
func (f *allSucceedOpenMeterClient) DeleteBillingProfileCustomerOverride(_ context.Context, _ openmeter.CustomerID) error {
	return nil
}
func (f *allSucceedOpenMeterClient) DeleteBillingProfile(_ context.Context, _ string) error {
	return nil
}
func (f *allSucceedOpenMeterClient) CustomerIDsByKey(_ context.Context, key openmeter.CustomerKey) ([]openmeter.CustomerID, error) {
	return accountCustomerIDs(key), nil
}
func (f *allSucceedOpenMeterClient) GetBillingProfile(_ context.Context, id string) (om.BillingProfile, error) {
	return accountProfile(id), nil
}
func (f *allSucceedOpenMeterClient) FindBillingProfilesByAccountKey(_ context.Context, _ openmeter.CustomerKey) ([]om.BillingProfile, error) {
	return []om.BillingProfile{accountProfile("profile-123")}, nil
}

// teardownOpenMeterClient drives the customer-link finalizer's teardown
// steps and records the order they were called in.
type teardownOpenMeterClient struct {
	openmeter.Client

	getCustomerErr error
	cancelErr      error
	// subs is what ListSubscriptions returns. CancelSubscriptions marks the
	// live ones inactive, unless stayLive is set.
	subs       []om.Subscription
	stayLive   bool
	listErr    error
	prepareErr error
	releaseErr error
	deleteErr  error

	calls               []string
	deleteCustomerCalls int
}

func (f *teardownOpenMeterClient) GetCustomer(_ context.Context, key openmeter.CustomerKey) (om.Customer, error) {
	f.calls = append(f.calls, "GetCustomer")
	if f.getCustomerErr != nil {
		return om.Customer{}, f.getCustomerErr
	}
	return om.Customer{Id: "id-" + string(key)}, nil
}

func (f *teardownOpenMeterClient) CancelSubscriptions(_ context.Context, _ openmeter.CustomerKey) error {
	f.calls = append(f.calls, "CancelSubscriptions")
	if f.cancelErr != nil {
		return f.cancelErr
	}
	if !f.stayLive {
		for i := range f.subs {
			f.subs[i].Status = om.SubscriptionStatusInactive
		}
	}
	return nil
}

func (f *teardownOpenMeterClient) ListSubscriptions(_ context.Context, _ openmeter.CustomerKey) ([]om.Subscription, error) {
	f.calls = append(f.calls, "ListSubscriptions")
	return f.subs, f.listErr
}

func (f *teardownOpenMeterClient) PrepareInvoicesForCustomerDeletion(_ context.Context, _ openmeter.CustomerID) error {
	f.calls = append(f.calls, "PrepareInvoicesForCustomerDeletion")
	return f.prepareErr
}

func (f *teardownOpenMeterClient) ReleaseCustomerSubjects(_ context.Context, _ openmeter.CustomerKey) error {
	f.calls = append(f.calls, "ReleaseCustomerSubjects")
	return f.releaseErr
}

func (f *teardownOpenMeterClient) DeleteCustomer(_ context.Context, _ openmeter.CustomerKey) error {
	f.calls = append(f.calls, "DeleteCustomer")
	f.deleteCustomerCalls++
	return f.deleteErr
}

func (f *teardownOpenMeterClient) called(method string) bool {
	return slices.Contains(f.calls, method)
}

// stubFinalizer is a sibling finalizer with a fixed outcome, so Reconcile's
// handling of the aggregate can be tested without depending on another
// finalizer's behavior.
type stubFinalizer struct {
	err error
}

func (s stubFinalizer) Finalize(context.Context, client.Object) (finalizer.Result, error) {
	return finalizer.Result{}, s.err
}

const stubFinalizerName = "test.openmeter.miloapis.com/stub"

// deletingAccountHarness is a BillingAccountReconciler over a fake cluster
// holding one deleting BillingAccount.
type deletingAccountHarness struct {
	r        *BillingAccountReconciler
	k8s      client.Client
	recorder *record.FakeRecorder
	req      reconcile.Request
}

func newDeletingAccountHarness(t *testing.T, omc openmeter.Client, annotations map[string]string, sibling *stubFinalizer) *deletingAccountHarness {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := billingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	finalizers := []string{CustomerLinkFinalizer}
	if sibling != nil {
		finalizers = append(finalizers, stubFinalizerName)
	}
	now := metav1.Now()
	account := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: "acme-account", Namespace: "org-a", UID: types.UID("uid-1"),
			DeletionTimestamp: &now,
			Finalizers:        finalizers,
			Annotations:       annotations,
		},
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(account).Build()
	recorder := record.NewFakeRecorder(100)
	r := &BillingAccountReconciler{Client: k8s, OpenMeterClient: omc, Recorder: recorder}
	r.Finalizers = finalizer.NewFinalizers()
	if err := r.Finalizers.Register(CustomerLinkFinalizer, &customerLinkFinalizer{OpenMeterClient: omc, Recorder: recorder}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if sibling != nil {
		if err := r.Finalizers.Register(stubFinalizerName, *sibling); err != nil {
			t.Fatalf("register stub: %v", err)
		}
	}
	return &deletingAccountHarness{
		r: r, k8s: k8s, recorder: recorder,
		req: reconcile.Request{NamespacedName: types.NamespacedName{Name: "acme-account", Namespace: "org-a"}},
	}
}

// reconcile runs one pass and returns its result plus the finalizers left
// on the account (nil once it's gone).
func (h *deletingAccountHarness) reconcile(t *testing.T) (reconcile.Result, []string, error) {
	t.Helper()
	res, err := h.r.Reconcile(context.Background(), h.req)
	var after billingv1alpha1.BillingAccount
	getErr := h.k8s.Get(context.Background(), h.req.NamespacedName, &after)
	if apierrors.IsNotFound(getErr) {
		return res, nil, err
	}
	if getErr != nil {
		t.Fatalf("get: %v", getErr)
	}
	return res, after.Finalizers, err
}

// events drains the recorder.
func (h *deletingAccountHarness) events() []string {
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

func countEvents(events []string, reason string) int {
	n := 0
	for _, e := range events {
		if strings.Contains(e, " "+reason+" ") {
			n++
		}
	}
	return n
}

func reconcileDeletingAccount(t *testing.T, omc openmeter.Client) (reconcile.Result, []string, error) {
	t.Helper()
	return newDeletingAccountHarness(t, omc, nil, nil).reconcile(t)
}

func outstandingErr(detail string) error {
	return fmt.Errorf("%w: %s", openmeter.ErrInvoicesOutstanding, detail)
}

// TestCustomerLinkFinalizer_TeardownOrder pins the order the teardown steps
// run in — each one depends on the previous (see Finalize's doc).
func TestCustomerLinkFinalizer_TeardownOrder(t *testing.T) {
	omc := &teardownOpenMeterClient{subs: []om.Subscription{{Id: "sub-1", Status: om.SubscriptionStatusActive}}}

	_, finalizers, err := reconcileDeletingAccount(t, omc)
	if err != nil || len(finalizers) != 0 {
		t.Fatalf("err = %v, finalizers = %v; want released", err, finalizers)
	}
	want := []string{
		"GetCustomer",
		"CancelSubscriptions",
		"ListSubscriptions",
		"PrepareInvoicesForCustomerDeletion",
		"ReleaseCustomerSubjects",
		"DeleteCustomer",
	}
	if !slices.Equal(omc.calls, want) {
		t.Errorf("calls = %v, want %v", omc.calls, want)
	}
}

// TestCustomerLinkFinalizer_WaitsForOutstandingInvoices: invoices with a
// balance hold the finalizer without it being an error — but only after the
// subscriptions are canceled and the subject keys released, so the customer
// neither keeps billing nor keeps the projects from moving while it waits.
func TestCustomerLinkFinalizer_WaitsForOutstandingInvoices(t *testing.T) {
	omc := &teardownOpenMeterClient{prepareErr: outstandingErr("inv-1 (issued, total 12.50 USD)")}

	res, finalizers, err := reconcileDeletingAccount(t, omc)

	if err != nil {
		t.Errorf("Reconcile error = %v, want none: waiting on invoices is not a failure", err)
	}
	if res.RequeueAfter != permanentRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, permanentRequeueAfter)
	}
	if !slices.Contains(finalizers, CustomerLinkFinalizer) {
		t.Error("finalizer released while invoices are outstanding")
	}
	if omc.deleteCustomerCalls != 0 {
		t.Errorf("DeleteCustomer called %d times, want 0", omc.deleteCustomerCalls)
	}
	if !omc.called("CancelSubscriptions") || !omc.called("ReleaseCustomerSubjects") {
		t.Errorf("calls = %v, want subscriptions canceled and subject keys released while waiting", omc.calls)
	}
}

// TestCustomerLinkFinalizer_WaitingForInvoicesEventOnlyOnChange: the wait
// can last weeks at one poll per 5 minutes; a Warning per poll (what it
// used to emit) buried everything else. One Normal event per distinct set
// of outstanding invoices.
func TestCustomerLinkFinalizer_WaitingForInvoicesEventOnlyOnChange(t *testing.T) {
	omc := &teardownOpenMeterClient{prepareErr: outstandingErr("inv-1 (issued, total 12.50 USD)")}
	h := newDeletingAccountHarness(t, omc, nil, nil)

	for range 3 {
		if _, _, err := h.reconcile(t); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	events := h.events()
	if n := countEvents(events, EventReasonWaitingForInvoices); n != 1 {
		t.Errorf("%d WaitingForInvoices events across 3 unchanged polls, want 1: %v", n, events)
	}
	if n := countEvents(events, EventReasonDeleteFailed); n != 0 {
		t.Errorf("%d DeleteFailed events while merely waiting, want 0: %v", n, events)
	}

	omc.prepareErr = outstandingErr("inv-1 (overdue, total 12.50 USD)")
	if _, _, err := h.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if n := countEvents(h.events(), EventReasonWaitingForInvoices); n != 1 {
		t.Errorf("%d WaitingForInvoices events after the outstanding set changed, want 1", n)
	}
}

// TestCustomerLinkFinalizer_WaitsForLiveSubscriptions: with a subscription
// still live after the cancel, nothing downstream runs — invoicing pending
// lines then would cut a premature invoice, and OpenMeter refuses to change
// subject keys or delete the customer anyway.
func TestCustomerLinkFinalizer_WaitsForLiveSubscriptions(t *testing.T) {
	omc := &teardownOpenMeterClient{
		subs:     []om.Subscription{{Id: "sub-1", Status: om.SubscriptionStatusScheduled}},
		stayLive: true,
	}

	res, finalizers, err := reconcileDeletingAccount(t, omc)
	if err != nil {
		t.Errorf("Reconcile error = %v, want none: waiting is not a failure", err)
	}
	if res.RequeueAfter != transientRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, transientRequeueAfter)
	}
	if !slices.Contains(finalizers, CustomerLinkFinalizer) {
		t.Error("finalizer released with a live subscription")
	}
	for _, m := range []string{"PrepareInvoicesForCustomerDeletion", "ReleaseCustomerSubjects", "DeleteCustomer"} {
		if omc.called(m) {
			t.Errorf("%s called while a subscription is still live (calls %v)", m, omc.calls)
		}
	}
}

func TestCustomerLinkFinalizer_CancelFailureIsAnError(t *testing.T) {
	omc := &teardownOpenMeterClient{cancelErr: &openmeter.PermanentError{Err: errors.New("boom"), StatusCode: 400}}

	_, finalizers, err := reconcileDeletingAccount(t, omc)
	if err == nil {
		t.Error("Reconcile error = nil, want the cancel failure surfaced")
	}
	if !slices.Contains(finalizers, CustomerLinkFinalizer) {
		t.Error("finalizer released after a failed cancel")
	}
	if omc.called("PrepareInvoicesForCustomerDeletion") {
		t.Error("invoices prepared although the subscriptions could not be canceled")
	}
}

func TestCustomerLinkFinalizer_DeletesCustomerOnceInvoicesSettle(t *testing.T) {
	omc := &teardownOpenMeterClient{}

	if _, finalizers, err := reconcileDeletingAccount(t, omc); err != nil || len(finalizers) != 0 {
		t.Fatalf("err = %v, finalizers = %v; want released", err, finalizers)
	}
	if omc.deleteCustomerCalls != 1 {
		t.Errorf("DeleteCustomer called %d times, want 1", omc.deleteCustomerCalls)
	}
}

func TestCustomerLinkFinalizer_CustomerAlreadyGone(t *testing.T) {
	omc := &teardownOpenMeterClient{getCustomerErr: openmeter.ErrCustomerNotFound}

	if _, finalizers, err := reconcileDeletingAccount(t, omc); err != nil || len(finalizers) != 0 {
		t.Fatalf("err = %v, finalizers = %v; want released", err, finalizers)
	}
	if omc.deleteCustomerCalls != 0 {
		t.Errorf("DeleteCustomer called %d times, want 0", omc.deleteCustomerCalls)
	}
}

// TestCustomerLinkFinalizer_PreparationFailureIsAnError: a real failure
// invoicing the pending lines must not release the subject keys — the
// final period's usage hasn't been invoiced yet, and a new invoice computes
// usage from the customer's subject keys at that point.
func TestCustomerLinkFinalizer_PreparationFailureIsAnError(t *testing.T) {
	omc := &teardownOpenMeterClient{prepareErr: &openmeter.TransientError{Err: errors.New("unavailable")}}

	_, finalizers, err := reconcileDeletingAccount(t, omc)
	if err == nil {
		t.Error("Reconcile error = nil, want the preparation failure surfaced for backoff")
	}
	if !slices.Contains(finalizers, CustomerLinkFinalizer) {
		t.Error("finalizer released after a failed preparation")
	}
	if omc.called("ReleaseCustomerSubjects") {
		t.Error("subject keys released before the pending lines were invoiced")
	}
}

// TestCustomerLinkFinalizer_ForceDelete: the escape hatch releases the
// finalizer even when the teardown is wedged, after a best-effort
// subscription cancel (the annotation is customer-writable, so it must never
// leave a subscription billing a deleted account), and says so.
func TestCustomerLinkFinalizer_ForceDelete(t *testing.T) {
	omc := &teardownOpenMeterClient{
		getCustomerErr: &openmeter.PermanentError{Err: errors.New("wedged"), StatusCode: 400},
		cancelErr:      &openmeter.TransientError{Err: errors.New("unavailable")},
	}
	h := newDeletingAccountHarness(t, omc, map[string]string{ForceDeleteAnnotation: "true"}, nil)

	_, finalizers, err := h.reconcile(t)
	if err != nil || len(finalizers) != 0 {
		t.Fatalf("err = %v, finalizers = %v; want released", err, finalizers)
	}
	if want := []string{"CancelSubscriptions"}; !slices.Equal(omc.calls, want) {
		t.Errorf("OpenMeter calls = %v, want %v (cancel attempted, nothing else)", omc.calls, want)
	}
	if n := countEvents(h.events(), EventReasonForceDeleted); n != 1 {
		t.Errorf("%d ForceDeleted events, want 1", n)
	}
}

// TestCustomerLinkFinalizer_ForceDeleteRequiresTrue: any other value is not
// an opt-in.
func TestCustomerLinkFinalizer_ForceDeleteRequiresTrue(t *testing.T) {
	omc := &teardownOpenMeterClient{prepareErr: outstandingErr("inv-1")}
	h := newDeletingAccountHarness(t, omc, map[string]string{ForceDeleteAnnotation: "yes"}, nil)

	if _, finalizers, _ := h.reconcile(t); !slices.Contains(finalizers, CustomerLinkFinalizer) {
		t.Error("finalizer released for a force-delete value other than \"true\"")
	}
}

// TestReconcile_WaitingDoesNotMaskSiblingFailure: a waiting finalizer next
// to a failing one must surface the failure. The old check —
// errors.Is(aggregate, ErrInvoicesOutstanding) — matched any member and
// swallowed the sibling's error.
func TestReconcile_WaitingDoesNotMaskSiblingFailure(t *testing.T) {
	omc := &teardownOpenMeterClient{prepareErr: outstandingErr("inv-1")}
	h := newDeletingAccountHarness(t, omc, nil, &stubFinalizer{err: errors.New("profile delete failed")})

	_, _, err := h.reconcile(t)
	if err == nil {
		t.Fatal("Reconcile error = nil, want the sibling finalizer's failure")
	}
	if !strings.Contains(err.Error(), "profile delete failed") {
		t.Errorf("err = %v, want it to carry the sibling's failure", err)
	}
}

// TestReconcile_AllFinalizersWaitingRequeuesQuietly: every finalizer
// waiting is a quiet requeue at the shortest interval.
func TestReconcile_AllFinalizersWaitingRequeuesQuietly(t *testing.T) {
	omc := &teardownOpenMeterClient{prepareErr: outstandingErr("inv-1")}
	sibling := &stubFinalizer{err: fmt.Errorf("wrapped: %w",
		NewFinalizerWaiting("WaitingForCustomerLink", 30*time.Second, errors.New("customer still linked")))}
	h := newDeletingAccountHarness(t, omc, nil, sibling)

	res, _, err := h.reconcile(t)
	if err != nil {
		t.Fatalf("Reconcile error = %v, want none", err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %v, want the shortest wait (30s)", res.RequeueAfter)
	}
}

func TestFinalizerWaitRequeue(t *testing.T) {
	waitA := NewFinalizerWaiting("A", time.Minute, nil)
	waitB := fmt.Errorf("finalizer %q failed: %w", "b", NewFinalizerWaiting("B", 10*time.Second, errors.New("x")))
	waitNoInterval := NewFinalizerWaiting("C", 0, nil)
	failure := errors.New("real failure")

	tests := []struct {
		name      string
		err       error
		wantOK    bool
		wantAfter time.Duration
	}{
		{name: "nil", err: nil, wantOK: false},
		{name: "plain failure", err: failure, wantOK: false},
		{name: "single waiting", err: waitA, wantOK: true, wantAfter: time.Minute},
		{name: "aggregate of waits takes the shortest", err: kerrors.NewAggregate([]error{waitA, waitB}), wantOK: true, wantAfter: 10 * time.Second},
		{name: "aggregate with a failure", err: kerrors.NewAggregate([]error{waitA, failure}), wantOK: false},
		{name: "joined waits", err: errors.Join(waitA, waitB), wantOK: true, wantAfter: 10 * time.Second},
		{name: "joined with a failure", err: errors.Join(waitA, failure), wantOK: false},
		{name: "wait wrapping a joined failure is still a wait", err: NewFinalizerWaiting("D", time.Second, errors.Join(failure)), wantOK: true, wantAfter: time.Second},
		{name: "zero interval defaults", err: waitNoInterval, wantOK: true, wantAfter: transientRequeueAfter},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			after, _, ok := finalizerWaitRequeue(tt.err)
			if ok != tt.wantOK || after != tt.wantAfter {
				t.Errorf("finalizerWaitRequeue = (%v, %v), want (%v, %v)", after, ok, tt.wantAfter, tt.wantOK)
			}
		})
	}
}
