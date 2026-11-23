// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

var chargeNow = time.Date(2026, time.November, 22, 15, 0, 0, 0, time.UTC)

// fakeChargeClient serves the OpenMeter calls the Charge controller makes,
// holding at most one line, or one per charge when byUID is set.
type fakeChargeClient struct {
	openmeter.Client // unimplemented methods panic if used

	noCustomer bool
	periodEnd  *time.Time
	createErr  error

	line     *openmeter.ChargeLineRef
	byUID    map[string]*openmeter.ChargeLineRef
	created  []openmeter.ChargeLine
	invoiced []string
	// invoiceCalls is each InvoiceChargeLinesNow call's lines.
	invoiceCalls [][]string
}

func (f *fakeChargeClient) GetCustomer(_ context.Context, key openmeter.CustomerKey) (om.Customer, error) {
	if f.noCustomer {
		return om.Customer{}, openmeter.ErrCustomerNotFound
	}
	return om.Customer{Id: "01JCUSTOMER", Key: (*string)(&key)}, nil
}

func (f *fakeChargeClient) FindChargeLine(_ context.Context, _ openmeter.CustomerID, uid string) (*openmeter.ChargeLineRef, error) {
	if f.byUID != nil {
		if ref := f.byUID[uid]; ref != nil {
			found := *ref
			return &found, nil
		}
		return nil, nil
	}
	if f.line == nil {
		return nil, nil
	}
	ref := *f.line
	return &ref, nil
}

func (f *fakeChargeClient) CreateChargeLine(_ context.Context, _ openmeter.CustomerID, line openmeter.ChargeLine) (openmeter.ChargeLineRef, error) {
	if f.createErr != nil {
		return openmeter.ChargeLineRef{}, f.createErr
	}
	f.created = append(f.created, line)
	if f.byUID != nil {
		ref := &openmeter.ChargeLineRef{LineID: "line-" + line.ChargeUID, InvoiceID: "01JGATHER", InvoiceStatus: om.InvoiceStatusGathering, InvoiceAt: line.InvoiceAt}
		f.byUID[line.ChargeUID] = ref
		return *ref, nil
	}
	f.line = &openmeter.ChargeLineRef{LineID: "01JLINE", InvoiceID: "01JGATHER", InvoiceStatus: om.InvoiceStatusGathering, InvoiceAt: line.InvoiceAt}
	return *f.line, nil
}

func (f *fakeChargeClient) InvoiceChargeLinesNow(_ context.Context, _ openmeter.CustomerID, lineIDs ...string) error {
	f.invoiced = append(f.invoiced, lineIDs...)
	f.invoiceCalls = append(f.invoiceCalls, lineIDs)
	if f.byUID != nil {
		for _, ref := range f.byUID {
			if slices.Contains(lineIDs, ref.LineID) {
				ref.InvoiceID, ref.InvoiceStatus = "01JNOW", om.InvoiceStatusIssued
			}
		}
		return nil
	}
	f.line.InvoiceID, f.line.InvoiceStatus = "01JNOW", om.InvoiceStatusIssued
	return nil
}

func (f *fakeChargeClient) CurrentBillingPeriodEnd(context.Context, openmeter.CustomerKey) (time.Time, bool, error) {
	if f.periodEnd == nil {
		return time.Time{}, false, nil
	}
	return *f.periodEnd, true, nil
}

func chargeAccount(writerReady bool) *billingv1alpha1.BillingAccount {
	status := metav1.ConditionFalse
	if writerReady {
		status = metav1.ConditionTrue
	}
	return &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{Name: testAccountName, Namespace: testNamespace, UID: "acct-uid"},
		Spec:       billingv1alpha1.BillingAccountSpec{CurrencyCode: "USD"},
		Status: billingv1alpha1.BillingAccountStatus{Conditions: []metav1.Condition{{
			Type: billingv1alpha1.BillingAccountConditionInvoiceWriterReady, Status: status, Reason: "Test",
			LastTransitionTime: metav1.NewTime(chargeNow),
		}}},
	}
}

func chargeObj() *billingv1alpha1.Charge {
	taxable := false
	return &billingv1alpha1.Charge{
		ObjectMeta: metav1.ObjectMeta{Name: "install", Namespace: testNamespace, UID: "charge-uid", Generation: 1},
		Spec: billingv1alpha1.ChargeSpec{
			BillingAccountRef: billingv1alpha1.BillingAccountRef{Name: testAccountName},
			ServicePricingRef: &billingv1alpha1.ChargeServicePricingRef{Name: "cross-connect-install-v1", Namespace: "milo-system"},
			Description:       "Cross-connect install",
			Quantity:          "2",
			UnitAmount:        "250.00",
			Currency:          "USD",
			DiscountPercent:   "10",
			Tax:               &billingv1alpha1.PricingTax{Taxable: &taxable},
			Note:              "Reseller, exempt",
			ServicePeriod:     &billingv1alpha1.ServicePeriod{Start: "2026-12-15", End: "2026-12-15"},
			Invoice:           billingv1alpha1.ChargeInvoiceNext,
		},
		Status: billingv1alpha1.ChargeStatus{Phase: billingv1alpha1.ChargePhasePending, Amount: "500.00", AmountDue: "450.00"},
	}
}

func newChargeReconciler(t *testing.T, omc *fakeChargeClient, objs ...client.Object) *ChargeReconciler {
	t.Helper()
	k8s := fake.NewClientBuilder().
		WithScheme(newOfferScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&billingv1alpha1.Charge{}).
		Build()
	return &ChargeReconciler{
		Client:          k8s,
		OpenMeterClient: omc,
		Recorder:        record.NewFakeRecorder(20),
		Now:             func() time.Time { return chargeNow },
	}
}

func reconcileCharge(t *testing.T, r *ChargeReconciler) (*billingv1alpha1.Charge, reconcile.Result) {
	t.Helper()
	key := types.NamespacedName{Namespace: testNamespace, Name: "install"}
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got billingv1alpha1.Charge
	if err := r.Get(context.Background(), key, &got); err != nil {
		return nil, res
	}
	return &got, res
}

func chargeReadyReason(c *billingv1alpha1.Charge) string {
	if cond := apimeta.FindStatusCondition(c.Status.Conditions, billingv1alpha1.ChargeConditionReady); cond != nil {
		return cond.Reason
	}
	return ""
}

func TestChargeWaitsForTheInvoiceWriter(t *testing.T) {
	omc := &fakeChargeClient{}
	r := newChargeReconciler(t, omc, chargeAccount(false), chargeObj())
	got, _ := reconcileCharge(t, r)
	if got.Status.Phase != billingv1alpha1.ChargePhasePending || chargeReadyReason(got) != ChargeReasonWaitingForInvoiceWriter {
		t.Fatalf("phase %s, reason %s", got.Status.Phase, chargeReadyReason(got))
	}
	if !controllerutil.ContainsFinalizer(got, ChargeFinalizer) {
		t.Error("no finalizer")
	}
	if len(omc.created) != 0 {
		t.Error("a line was created for an account not on the writer")
	}
}

func TestChargeWaitsForCustomerAndAmounts(t *testing.T) {
	r := newChargeReconciler(t, &fakeChargeClient{noCustomer: true}, chargeAccount(true), chargeObj())
	if got, res := reconcileCharge(t, r); chargeReadyReason(got) != ChargeReasonWaitingForCustomer || res.RequeueAfter == 0 {
		t.Fatalf("reason %s, requeue %s", chargeReadyReason(got), res.RequeueAfter)
	}

	noAmounts := chargeObj()
	noAmounts.Status.AmountDue = ""
	r = newChargeReconciler(t, &fakeChargeClient{}, chargeAccount(true), noAmounts)
	if got, res := reconcileCharge(t, r); chargeReadyReason(got) != ChargeReasonWaitingForAmounts || res.RequeueAfter == 0 {
		t.Fatalf("reason %s, requeue %s", chargeReadyReason(got), res.RequeueAfter)
	}
}

func chargeOnInvoice(c *billingv1alpha1.Charge) *metav1.Condition {
	return apimeta.FindStatusCondition(c.Status.Conditions, billingv1alpha1.ChargeConditionOnInvoice)
}

func TestChargeIsHeldUntilShortlyBeforeItsInvoice(t *testing.T) {
	periodEnd := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	omc := &fakeChargeClient{periodEnd: &periodEnd}
	r := newChargeReconciler(t, omc, chargeAccount(true), chargeObj())
	got, res := reconcileCharge(t, r)

	if len(omc.created) != 0 {
		t.Fatalf("created a line %s before its invoice", periodEnd.Sub(chargeNow))
	}
	until := periodEnd.Add(-DefaultChargeHold)
	if got.Status.Phase != billingv1alpha1.ChargePhaseScheduled || chargeReadyReason(got) != ChargeReasonScheduled ||
		got.Status.InvoiceAt == nil || !got.Status.InvoiceAt.Time.Equal(periodEnd) ||
		got.Status.RemovableUntil == nil || !got.Status.RemovableUntil.Time.Equal(until) {
		t.Fatalf("status = %+v", got.Status)
	}
	if c := chargeOnInvoice(got); c == nil || c.Status != metav1.ConditionFalse || c.Reason != ChargeReasonHeld {
		t.Fatalf("OnInvoice = %+v", c)
	}
	if res.RequeueAfter != chargeRecheckAfter {
		t.Errorf("requeue = %s; want the hourly recheck, as the hold ends later", res.RequeueAfter)
	}

	// Close to the end of the hold, it wakes when the hold ends.
	r.Now = func() time.Time { return until.Add(-10 * time.Minute) }
	if _, res = reconcileCharge(t, r); res.RequeueAfter != 10*time.Minute || len(omc.created) != 0 {
		t.Fatalf("requeue = %s, created %d", res.RequeueAfter, len(omc.created))
	}
}

func TestChargeGoesOnItsInvoiceWhenTheHoldEnds(t *testing.T) {
	periodEnd := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	omc := &fakeChargeClient{periodEnd: &periodEnd}
	r := newChargeReconciler(t, omc, chargeAccount(true), chargeObj())
	r.Now = func() time.Time { return periodEnd.Add(-DefaultChargeHold) }
	got, res := reconcileCharge(t, r)

	if len(omc.created) != 1 {
		t.Fatalf("created %d lines", len(omc.created))
	}
	line := omc.created[0]
	want := openmeter.ChargeLine{
		Charge: testNamespace + "/install", ChargeUID: "charge-uid", Currency: "USD", Name: "Cross-connect install",
		Amount: "450.00", Quantity: "2", UnitAmount: "250.00", DiscountPercent: "10",
		From: time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 12, 16, 0, 0, 0, 0, time.UTC),
		InvoiceAt: periodEnd, TaxCode: openmeter.NontaxableTaxCode,
	}
	if line != want {
		t.Fatalf("line =\n%+v\nwant\n%+v", line, want)
	}
	if got.Status.Phase != billingv1alpha1.ChargePhaseScheduled || got.Status.RemovableUntil != nil ||
		got.Status.InvoiceAt == nil || !got.Status.InvoiceAt.Time.Equal(periodEnd) {
		t.Fatalf("status = %+v", got.Status)
	}
	if c := chargeOnInvoice(got); c == nil || c.Status != metav1.ConditionTrue || c.Reason != ChargeReasonAdded {
		t.Fatalf("OnInvoice = %+v", c)
	}
	if res.RequeueAfter != chargeRecheckAfter {
		t.Errorf("requeue = %s", res.RequeueAfter)
	}

	// A second pass adopts the line instead of making another.
	reconcileCharge(t, r)
	if len(omc.created) != 1 {
		t.Fatalf("created %d lines after a second pass", len(omc.created))
	}
}

func TestChargeAddedInsideTheHoldGoesOnStraightAway(t *testing.T) {
	periodEnd := chargeNow.Add(3 * time.Hour)
	omc := &fakeChargeClient{periodEnd: &periodEnd}
	r := newChargeReconciler(t, omc, chargeAccount(true), chargeObj())
	got, _ := reconcileCharge(t, r)
	if len(omc.created) != 1 {
		t.Fatalf("created %d lines", len(omc.created))
	}
	if c := chargeOnInvoice(got); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("OnInvoice = %+v", c)
	}
}

func TestChargeIsNotAddedWhenItsRemovalWins(t *testing.T) {
	periodEnd := chargeNow.Add(3 * time.Hour)
	omc := &fakeChargeClient{periodEnd: &periodEnd}
	charge := chargeObj()
	charge.Finalizers = []string{ChargeFinalizer}
	k8s := fake.NewClientBuilder().
		WithScheme(newOfferScheme(t)).
		WithObjects(chargeAccount(true), charge).
		WithStatusSubresource(&billingv1alpha1.Charge{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				// The charge was removed after it was read: its removal
				// changed it, so the optimistic write conflicts.
				return apierrors.NewConflict(schema.GroupResource{Group: "billing.miloapis.com", Resource: "charges"}, obj.GetName(), errors.New("changed"))
			},
		}).
		Build()
	r := &ChargeReconciler{Client: k8s, OpenMeterClient: omc, Recorder: record.NewFakeRecorder(20), Now: func() time.Time { return chargeNow }}
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "install"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(omc.created) != 0 {
		t.Fatal("added a line although marking the charge failed")
	}
	if !res.Requeue {
		t.Error("didn't start over")
	}
}

func TestChargeWithoutASubscriptionUsesTheBillingDay(t *testing.T) {
	omc := &fakeChargeClient{}
	account := chargeAccount(true)
	account.Status.Contract = &billingv1alpha1.BillingAccountContract{
		StartsAt: metav1.NewTime(time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)), BillingDayOfMonth: 30,
	}
	r := newChargeReconciler(t, omc, account, chargeObj())
	got, _ := reconcileCharge(t, r)
	if want := time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC); got.Status.InvoiceAt == nil || !got.Status.InvoiceAt.Time.Equal(want) {
		t.Fatalf("invoiceAt = %v, want %s", got.Status.InvoiceAt, want)
	}
}

func TestNextBillingDay(t *testing.T) {
	account := chargeAccount(true)
	for _, tt := range []struct {
		now  time.Time
		day  int32
		want time.Time
	}{
		{now: chargeNow, day: 1, want: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)},
		{now: time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC), day: 31, want: time.Date(2027, 2, 28, 0, 0, 0, 0, time.UTC)},
		{now: time.Date(2026, 11, 14, 23, 0, 0, 0, time.UTC), day: 15, want: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)},
	} {
		account.Spec.PaymentTerms = &billingv1alpha1.PaymentTerms{InvoiceDayOfMonth: tt.day}
		if got := nextBillingDay(account, tt.now); !got.Equal(tt.want) {
			t.Errorf("nextBillingDay(%s, day %d) = %s, want %s", tt.now, tt.day, got, tt.want)
		}
	}
}

func TestChargeInvoicedNow(t *testing.T) {
	omc := &fakeChargeClient{}
	charge := chargeObj()
	charge.Spec.Invoice = billingv1alpha1.ChargeInvoiceNow
	invoice := &billingv1alpha1.Invoice{ObjectMeta: metav1.ObjectMeta{
		Name: "acct-2026-11-abc", Namespace: testNamespace,
		Annotations: map[string]string{InvoiceKeyAnnotation: "01JNOW"},
	}}
	r := newChargeReconciler(t, omc, chargeAccount(true), charge, invoice)
	got, _ := reconcileCharge(t, r)

	if !omc.created[0].InvoiceAt.Equal(chargeNow) {
		t.Errorf("invoiceAt = %s, want now", omc.created[0].InvoiceAt)
	}
	if len(omc.invoiced) != 1 || omc.invoiced[0] != "01JLINE" {
		t.Fatalf("invoiced now: %v", omc.invoiced)
	}
	if got.Status.Phase != billingv1alpha1.ChargePhaseInvoiced || got.Status.InvoiceRef == nil ||
		got.Status.InvoiceRef.Name != "acct-2026-11-abc" {
		t.Fatalf("status = %+v", got.Status)
	}
}

// groupCharge is a charge invoiced now in an invoice group of size.
func groupCharge(name string, size int32) *billingv1alpha1.Charge {
	c := chargeObj()
	c.Name, c.UID = name, types.UID(name+"-uid")
	c.Spec.Invoice = billingv1alpha1.ChargeInvoiceNow
	c.Spec.InvoiceGroup = &billingv1alpha1.ChargeInvoiceGroup{Name: "test-period", Size: size}
	return c
}

func reconcileNamed(t *testing.T, r *ChargeReconciler, name string) (*billingv1alpha1.Charge, reconcile.Result) {
	t.Helper()
	key := types.NamespacedName{Namespace: testNamespace, Name: name}
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile %s: %v", name, err)
	}
	var got billingv1alpha1.Charge
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return &got, res
}

func TestChargeInvoiceGroupGoesOutTogether(t *testing.T) {
	omc := &fakeChargeClient{byUID: map[string]*openmeter.ChargeLineRef{}}
	gpu := groupCharge("gpu", 2)
	r := newChargeReconciler(t, omc, chargeAccount(true), gpu)

	got, _ := reconcileNamed(t, r, "gpu")
	if got.Status.Phase != billingv1alpha1.ChargePhasePending || chargeReadyReason(got) != ChargeReasonWaitingForInvoiceGroup {
		t.Fatalf("alone: phase %s, reason %s", got.Status.Phase, chargeReadyReason(got))
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, billingv1alpha1.ChargeConditionReady); !strings.Contains(cond.Message, "1 of its 2 charges") {
		t.Errorf("message = %q", cond.Message)
	}
	if len(omc.created) != 0 {
		t.Fatalf("a line was added before the group was complete: %+v", omc.created)
	}

	if err := r.Create(context.Background(), groupCharge("cpu", 2)); err != nil {
		t.Fatal(err)
	}
	if reqs := r.invoiceGroupSiblings(context.Background(), groupCharge("cpu", 2)); len(reqs) != 1 || reqs[0].Name != "gpu" {
		t.Errorf("siblings of cpu = %v, want gpu", reqs)
	}
	// gpu gets its line, then waits for cpu's.
	_, res := reconcileNamed(t, r, "gpu")
	if len(omc.created) != 1 || len(omc.invoiceCalls) != 0 || res.RequeueAfter == 0 {
		t.Fatalf("gpu: created %d, invoiced %v, requeue %s", len(omc.created), omc.invoiceCalls, res.RequeueAfter)
	}
	// cpu gets its line, and both go on one invoice.
	reconcileNamed(t, r, "cpu")
	if len(omc.invoiceCalls) != 1 || !slices.Equal(omc.invoiceCalls[0], []string{"line-cpu-uid", "line-gpu-uid"}) {
		t.Fatalf("invoice calls = %v, want one with both lines", omc.invoiceCalls)
	}
	reconcileNamed(t, r, "gpu")
	if len(omc.invoiceCalls) != 1 {
		t.Fatalf("gpu invoiced again: %v", omc.invoiceCalls)
	}
}

func TestChargeInvoiceGroupLeavesOutARefusedCharge(t *testing.T) {
	omc := &fakeChargeClient{byUID: map[string]*openmeter.ChargeLineRef{}}
	refused := groupCharge("cpu", 2)
	refused.Status.Phase = billingv1alpha1.ChargePhaseFailed
	r := newChargeReconciler(t, omc, chargeAccount(true), groupCharge("gpu", 2), refused)

	reconcileNamed(t, r, "gpu")
	if len(omc.invoiceCalls) != 1 || !slices.Equal(omc.invoiceCalls[0], []string{"line-gpu-uid"}) {
		t.Fatalf("invoice calls = %v, want gpu's line without the refused charge", omc.invoiceCalls)
	}
}

func TestChargeInvoiceGroupStaysClosedOnceSending(t *testing.T) {
	omc := &fakeChargeClient{byUID: map[string]*openmeter.ChargeLineRef{
		"gpu-uid": {LineID: "line-gpu-uid", InvoiceID: "01JGATHER", InvoiceStatus: om.InvoiceStatusGathering},
	}}
	// Three were added; one was removed after gpu got its line.
	sending := groupCharge("gpu", 3)
	sending.Status.Conditions = []metav1.Condition{{Type: billingv1alpha1.ChargeConditionOnInvoice, Status: metav1.ConditionTrue, Reason: ChargeReasonAdding}}
	r := newChargeReconciler(t, omc, chargeAccount(true), sending, groupCharge("cpu", 3))

	reconcileNamed(t, r, "cpu")
	if len(omc.invoiceCalls) != 1 || !slices.Equal(omc.invoiceCalls[0], []string{"line-cpu-uid", "line-gpu-uid"}) {
		t.Fatalf("invoice calls = %v, want the two left on one invoice", omc.invoiceCalls)
	}
}

func TestChargeWaitsForItsInvoiceProjection(t *testing.T) {
	omc := &fakeChargeClient{line: &openmeter.ChargeLineRef{LineID: "01JLINE", InvoiceID: "01JISSUED", InvoiceStatus: om.InvoiceStatusIssued}}
	r := newChargeReconciler(t, omc, chargeAccount(true), chargeObj())
	got, res := reconcileCharge(t, r)
	if got.Status.Phase == billingv1alpha1.ChargePhaseInvoiced || res.RequeueAfter == 0 {
		t.Fatalf("phase %s, requeue %s", got.Status.Phase, res.RequeueAfter)
	}
}

func TestChargeRefusedByOpenMeter(t *testing.T) {
	omc := &fakeChargeClient{createErr: &openmeter.PermanentError{Err: errors.New("invalid period")}}
	charge := chargeObj()
	charge.Spec.Invoice = billingv1alpha1.ChargeInvoiceNow
	r := newChargeReconciler(t, omc, chargeAccount(true), charge)
	got, _ := reconcileCharge(t, r)
	if got.Status.Phase != billingv1alpha1.ChargePhaseFailed || chargeReadyReason(got) != ChargeReasonRefused {
		t.Fatalf("status = %+v", got.Status)
	}
	// Not on an invoice, so staff can remove it.
	if c := chargeOnInvoice(got); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("OnInvoice = %+v", c)
	}
}

func deletingCharge() *billingv1alpha1.Charge {
	c := chargeObj()
	now := metav1.NewTime(chargeNow)
	c.DeletionTimestamp = &now
	c.Finalizers = []string{ChargeFinalizer}
	return c
}

func TestRemovingAHeldCharge(t *testing.T) {
	omc := &fakeChargeClient{}
	charge := deletingCharge()
	charge.Status.Phase = billingv1alpha1.ChargePhaseScheduled
	charge.Status.Conditions = []metav1.Condition{{Type: billingv1alpha1.ChargeConditionOnInvoice, Status: metav1.ConditionFalse, Reason: ChargeReasonHeld}}
	r := newChargeReconciler(t, omc, chargeAccount(true), charge)
	if got, _ := reconcileCharge(t, r); got != nil {
		t.Fatalf("charge still there: %+v", got.Finalizers)
	}
	if len(omc.created) != 0 || chargeHasEvent(r, EventReasonChargeAlreadyInvoiced) {
		t.Fatal("a held charge's removal touched OpenMeter or warned")
	}
}

func TestRemovingAChargeOnItsInvoiceLeavesTheLine(t *testing.T) {
	// Only reachable when billing's webhook lets it through: the
	// namespace is going while the account isn't yet.
	omc := &fakeChargeClient{line: &openmeter.ChargeLineRef{LineID: "01JLINE", InvoiceID: "01JGATHER", InvoiceStatus: om.InvoiceStatusGathering}}
	charge := deletingCharge()
	charge.Status.Phase = billingv1alpha1.ChargePhaseScheduled
	charge.Status.Conditions = []metav1.Condition{{Type: billingv1alpha1.ChargeConditionOnInvoice, Status: metav1.ConditionTrue, Reason: ChargeReasonAdded}}
	r := newChargeReconciler(t, omc, chargeAccount(true), charge)
	if got, _ := reconcileCharge(t, r); got != nil {
		t.Fatal("charge still there")
	}
	if omc.line == nil {
		t.Fatal("the line went")
	}
	if !chargeHasEvent(r, EventReasonChargeAlreadyInvoiced) {
		t.Error("no ChargeAlreadyInvoiced event")
	}
}

func TestRemovingAChargeWithItsAccount(t *testing.T) {
	account := chargeAccount(true)
	now := metav1.NewTime(chargeNow)
	account.DeletionTimestamp = &now
	account.Finalizers = []string{"test"}

	t.Run("on its invoice: billed on the final invoice", func(t *testing.T) {
		omc := &fakeChargeClient{line: &openmeter.ChargeLineRef{LineID: "01JLINE", InvoiceID: "01JGATHER", InvoiceStatus: om.InvoiceStatusGathering}}
		charge := deletingCharge()
		charge.Status.Phase = billingv1alpha1.ChargePhaseScheduled
		charge.Status.Conditions = []metav1.Condition{{Type: billingv1alpha1.ChargeConditionOnInvoice, Status: metav1.ConditionTrue, Reason: ChargeReasonAdded}}
		r := newChargeReconciler(t, omc, account.DeepCopy(), charge)
		if got, _ := reconcileCharge(t, r); got != nil {
			t.Fatal("charge still there")
		}
		if omc.line == nil || chargeHasEvent(r, EventReasonChargeNotBilled) {
			t.Fatal("the line went, or it said the charge wasn't billed")
		}
	})

	t.Run("held: not billed, and says so", func(t *testing.T) {
		omc := &fakeChargeClient{}
		charge := deletingCharge()
		charge.Status.Phase = billingv1alpha1.ChargePhaseScheduled
		charge.Status.Conditions = []metav1.Condition{{Type: billingv1alpha1.ChargeConditionOnInvoice, Status: metav1.ConditionFalse, Reason: ChargeReasonHeld}}
		r := newChargeReconciler(t, omc, account.DeepCopy(), charge)
		if got, _ := reconcileCharge(t, r); got != nil {
			t.Fatal("charge still there")
		}
		if !chargeHasEvent(r, EventReasonChargeNotBilled) {
			t.Error("no ChargeNotBilled event")
		}
	})
}

func chargeHasEvent(r *ChargeReconciler, reason string) bool {
	rec := r.Recorder.(*record.FakeRecorder)
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, reason) {
				return true
			}
		default:
			return false
		}
	}
}
