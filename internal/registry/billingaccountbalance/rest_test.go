// SPDX-License-Identifier: AGPL-3.0-only

package billingaccountbalance

import (
	"context"
	"testing"
	"time"

	"github.com/alpacahq/alpacadecimal"
	om "github.com/openmeterio/openmeter/api/client/go"
	omv3 "github.com/openmeterio/openmeter/api/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/endpoints/request"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	usagev1alpha1 "go.miloapis.com/billing/api/usage/v1alpha1"
	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

type fakeBalanceOpenMeterClient struct {
	openmeter.Client

	customer       om.Customer
	customerErr    error
	creditBalance  string
	creditBalErr   error
	invoices       []om.Invoice
	invoicesErr    error
	grants         []omv3.BillingCreditGrant
	grantsErr      error
	subscriptions  []om.Subscription
	subErr         error
	queryMeterRows []openmeter.MeterQueryRow
}

func (f *fakeBalanceOpenMeterClient) GetCustomer(_ context.Context, _ openmeter.CustomerKey) (om.Customer, error) {
	if f.customerErr != nil {
		return om.Customer{}, f.customerErr
	}
	return f.customer, nil
}

func (f *fakeBalanceOpenMeterClient) GetCreditBalance(_ context.Context, _ openmeter.CustomerID) (string, error) {
	if f.creditBalErr != nil {
		return "0.00", f.creditBalErr
	}
	return f.creditBalance, nil
}

func (f *fakeBalanceOpenMeterClient) ListInvoices(_ context.Context, _ openmeter.CustomerID) ([]om.Invoice, error) {
	if f.invoicesErr != nil {
		return nil, f.invoicesErr
	}
	return f.invoices, nil
}

func (f *fakeBalanceOpenMeterClient) ListCreditGrants(_ context.Context, _ openmeter.CustomerID) ([]omv3.BillingCreditGrant, error) {
	if f.grantsErr != nil {
		return nil, f.grantsErr
	}
	return f.grants, nil
}

func (f *fakeBalanceOpenMeterClient) ListSubscriptions(_ context.Context, _ openmeter.CustomerKey) ([]om.Subscription, error) {
	if f.subErr != nil {
		return nil, f.subErr
	}
	return f.subscriptions, nil
}

func (f *fakeBalanceOpenMeterClient) QueryMeter(_ context.Context, _ openmeter.MeterQuery) ([]openmeter.MeterQueryRow, error) {
	return f.queryMeterRows, nil
}

func setupScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := billingv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := usagev1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGet_Success(t *testing.T) {
	scheme := setupScheme(t)
	account := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "acme-main",
			Namespace: "org-acme",
			UID:       types.UID("uid-acme"),
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(account).Build()

	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	expires1 := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	omc := &fakeBalanceOpenMeterClient{
		customer:      om.Customer{Id: "om-cust-1"},
		creditBalance: "130.00",
		invoices: []om.Invoice{
			{
				Id:     "inv-1",
				Status: om.InvoiceStatusPaid,
				Totals: om.InvoiceTotals{Total: "200.00"},
			},
			{
				Id:     "inv-2",
				Status: om.InvoiceStatusOverdue,
				Totals: om.InvoiceTotals{Total: "75.00"},
			},
		},
		grants: []omv3.BillingCreditGrant{
			{
				Id:        "grant-1",
				Name:      "welcome-acme",
				Amount:    "500.00",
				Status:    omv3.BillingCreditGrantStatusActive,
				ExpiresAt: &expires1,
				CreatedAt: now.Add(-720 * time.Hour),
			},
			{
				Id:        "grant-2",
				Name:      "support-goodwill",
				Amount:    "50.00",
				Status:    omv3.BillingCreditGrantStatusActive,
				ExpiresAt: nil,
				CreatedAt: now.Add(-24 * time.Hour),
			},
		},
	}

	r := NewREST(cl, omc)
	r.now = func() time.Time { return now }

	ctx := request.WithNamespace(context.Background(), "org-acme")
	obj, err := r.Get(ctx, "acme-main", nil)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	bal, ok := obj.(*usagev1alpha1.BillingAccountBalance)
	if !ok {
		t.Fatalf("result is not *BillingAccountBalance: %T", obj)
	}

	if bal.Name != "acme-main" || bal.Namespace != "org-acme" {
		t.Errorf("metadata = %s/%s, want org-acme/acme-main", bal.Namespace, bal.Name)
	}
	if bal.Status.Currency != "USD" {
		t.Errorf("currency = %s, want USD", bal.Status.Currency)
	}
	if bal.Status.Balance.CreditRemaining != "130.00" {
		t.Errorf("balance.creditRemaining = %s, want 130.00", bal.Status.Balance.CreditRemaining)
	}
	if bal.Status.Balance.AmountOwed != "75.00" {
		t.Errorf("balance.amountOwed = %s, want 75.00", bal.Status.Balance.AmountOwed)
	}

	if len(bal.Status.Grants) != 2 {
		t.Fatalf("grants len = %d, want 2", len(bal.Status.Grants))
	}
	// Welcome grant: 500 total, 420 used, 80 remaining
	g1 := bal.Status.Grants[0]
	if g1.Name != "welcome-acme" || g1.Amount != "500.00" || g1.Used != "420.00" || g1.Remaining != "80.00" {
		t.Errorf("grant 0 = %+v, want welcome-acme (500/420/80)", g1)
	}
	if g1.ExpiresAt == nil || !g1.ExpiresAt.Time.Equal(expires1) {
		t.Errorf("grant 0 expiresAt = %v, want %v", g1.ExpiresAt, expires1)
	}

	// Goodwill grant: 50 total, 0 used, 50 remaining, no expiry
	g2 := bal.Status.Grants[1]
	if g2.Name != "support-goodwill" || g2.Amount != "50.00" || g2.Used != "0.00" || g2.Remaining != "50.00" {
		t.Errorf("grant 1 = %+v, want support-goodwill (50/0/50)", g2)
	}
	if g2.ExpiresAt != nil {
		t.Errorf("grant 1 expiresAt = %v, want nil", g2.ExpiresAt)
	}

	// Default forecast when no spend: spend 0, used 0, due 0, remaining 130
	if bal.Status.Forecast.EstimatedSpend != "0.00" {
		t.Errorf("forecast.estimatedSpend = %s, want 0.00", bal.Status.Forecast.EstimatedSpend)
	}
	if bal.Status.Forecast.EstimatedCreditUsed != "0.00" {
		t.Errorf("forecast.estimatedCreditUsed = %s, want 0.00", bal.Status.Forecast.EstimatedCreditUsed)
	}
	if bal.Status.Forecast.EstimatedAmountDue != "0.00" {
		t.Errorf("forecast.estimatedAmountDue = %s, want 0.00", bal.Status.Forecast.EstimatedAmountDue)
	}
	if bal.Status.Forecast.EstimatedCreditRemaining != "130.00" {
		t.Errorf("forecast.estimatedCreditRemaining = %s, want 130.00", bal.Status.Forecast.EstimatedCreditRemaining)
	}
}

func TestGet_AccountNotFound(t *testing.T) {
	scheme := setupScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewREST(cl, &fakeBalanceOpenMeterClient{})

	ctx := request.WithNamespace(context.Background(), "org-acme")
	_, err := r.Get(ctx, "nonexistent", nil)
	if err == nil || !apierrors.IsNotFound(err) {
		t.Errorf("err = %v, want NotFound", err)
	}
}

func TestGet_CustomerNotFoundInOpenMeter(t *testing.T) {
	scheme := setupScheme(t)
	account := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "new-account",
			Namespace: "org-acme",
			UID:       types.UID("uid-new"),
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(account).Build()
	omc := &fakeBalanceOpenMeterClient{
		customerErr: openmeter.ErrCustomerNotFound,
	}

	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	r := NewREST(cl, omc)
	r.now = func() time.Time { return now }

	ctx := request.WithNamespace(context.Background(), "org-acme")
	obj, err := r.Get(ctx, "new-account", nil)
	if err != nil {
		t.Fatalf("Get unexpected error: %v", err)
	}

	bal, ok := obj.(*usagev1alpha1.BillingAccountBalance)
	if !ok {
		t.Fatalf("not *BillingAccountBalance: %T", obj)
	}
	if bal.Status.Balance.CreditRemaining != "0.00" {
		t.Errorf("creditRemaining = %s, want 0.00", bal.Status.Balance.CreditRemaining)
	}
	if bal.Status.Balance.AmountOwed != "0.00" {
		t.Errorf("amountOwed = %s, want 0.00", bal.Status.Balance.AmountOwed)
	}
	if len(bal.Status.Grants) != 0 {
		t.Errorf("grants len = %d, want 0", len(bal.Status.Grants))
	}
}

func TestList_InNamespace(t *testing.T) {
	scheme := setupScheme(t)
	account1 := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "account-1",
			Namespace: "org-acme",
			UID:       types.UID("uid-1"),
		},
	}
	account2 := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "account-2",
			Namespace: "org-acme",
			UID:       types.UID("uid-2"),
		},
	}
	otherOrgAccount := &billingv1alpha1.BillingAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "account-other",
			Namespace: "org-other",
			UID:       types.UID("uid-3"),
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(account1, account2, otherOrgAccount).Build()
	omc := &fakeBalanceOpenMeterClient{
		customer:      om.Customer{Id: "om-cust-1"},
		creditBalance: "50.00",
	}

	r := NewREST(cl, omc)
	ctx := request.WithNamespace(context.Background(), "org-acme")
	listObj, err := r.List(ctx, nil)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	list, ok := listObj.(*usagev1alpha1.BillingAccountBalanceList)
	if !ok {
		t.Fatalf("not *BillingAccountBalanceList: %T", listObj)
	}
	if len(list.Items) != 2 {
		t.Fatalf("items count = %d, want 2", len(list.Items))
	}
	if list.Items[0].Name != "account-1" || list.Items[1].Name != "account-2" {
		t.Errorf("items = [%s, %s], want [account-1, account-2]", list.Items[0].Name, list.Items[1].Name)
	}
}

func TestComputeBalanceGrants(t *testing.T) {
	now := time.Now()
	expSoon := now.Add(24 * time.Hour)
	expLater := now.Add(720 * time.Hour)

	grants := []omv3.BillingCreditGrant{
		{
			Id:        "g-noexp",
			Name:      "no-expiry",
			Amount:    "50.00",
			Status:    omv3.BillingCreditGrantStatusActive,
			ExpiresAt: nil,
			CreatedAt: now.Add(-10 * time.Hour),
		},
		{
			Id:        "g-later",
			Name:      "expires-later",
			Amount:    "100.00",
			Status:    omv3.BillingCreditGrantStatusActive,
			ExpiresAt: &expLater,
			CreatedAt: now.Add(-20 * time.Hour),
		},
		{
			Id:        "g-soon",
			Name:      "expires-soon",
			Amount:    "200.00",
			Status:    omv3.BillingCreditGrantStatusActive,
			ExpiresAt: &expSoon,
			CreatedAt: now.Add(-30 * time.Hour),
		},
		{
			Id:        "g-voided",
			Name:      "voided-grant",
			Amount:    "50.00",
			Status:    omv3.BillingCreditGrantStatusVoided,
			ExpiresAt: &expSoon,
		},
	}

	// Total active granted = 200 + 100 + 50 = 350.
	// Suppose creditRemaining = 120.
	// Used total = 350 - 120 = 230.
	// Order of drawdown:
	// 1. expires-soon (200): all 200 used -> used: 200.00, remaining: 0.00 (usedTotal left: 30)
	// 2. expires-later (100): 30 used -> used: 30.00, remaining: 70.00 (usedTotal left: 0)
	// 3. no-expiry (50): 0 used -> used: 0.00, remaining: 50.00
	creditRemaining := alpacadecimal.NewFromInt(120)
	res := computeBalanceGrants(grants, creditRemaining)

	if len(res) != 3 {
		t.Fatalf("computeBalanceGrants len = %d, want 3", len(res))
	}

	if res[0].Name != "expires-soon" || res[0].Used != "200.00" || res[0].Remaining != "0.00" {
		t.Errorf("res[0] = %+v, want expires-soon used 200, remaining 0", res[0])
	}
	if res[1].Name != "expires-later" || res[1].Used != "30.00" || res[1].Remaining != "70.00" {
		t.Errorf("res[1] = %+v, want expires-later used 30, remaining 70", res[1])
	}
	if res[2].Name != "no-expiry" || res[2].Used != "0.00" || res[2].Remaining != "50.00" {
		t.Errorf("res[2] = %+v, want no-expiry used 0, remaining 50", res[2])
	}
}
