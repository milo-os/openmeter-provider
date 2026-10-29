// SPDX-License-Identifier: AGPL-3.0-only

// Package billingaccountbalance serves usage.billing.miloapis.com
// BillingAccountBalance from OpenMeter.
//
// A BillingAccountBalance is read-only (GET, LIST) and never stored: Get reads
// the named BillingAccount in the request namespace, queries OpenMeter for the
// customer's posted credit balance, past invoice balances, active credit
// grants, and current period forecast spend, and returns the balance live.
package billingaccountbalance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/alpacahq/alpacadecimal"
	om "github.com/openmeterio/openmeter/api/client/go"
	omv3 "github.com/openmeterio/openmeter/api/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	usagev1alpha1 "go.miloapis.com/billing/api/usage/v1alpha1"
	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
	"go.miloapis.com/openmeter-provider/internal/registry/usagequery"
)

// REST is the RESTStorage for BillingAccountBalance.
type REST struct {
	rest.TableConvertor

	milo      client.Reader
	openMeter openmeter.Client
	now       func() time.Time
}

var (
	_ rest.Storage              = &REST{}
	_ rest.Getter               = &REST{}
	_ rest.Lister               = &REST{}
	_ rest.Scoper               = &REST{}
	_ rest.SingularNameProvider = &REST{}
)

// NewREST returns the RESTStorage for BillingAccountBalance.
func NewREST(milo client.Reader, openMeter openmeter.Client) *REST {
	return &REST{
		TableConvertor: rest.NewDefaultTableConvertor(usagev1alpha1.Resource("billingaccountbalances")),
		milo:           milo,
		openMeter:      openMeter,
		now:            time.Now,
	}
}

// New returns an empty BillingAccountBalance.
func (r *REST) New() runtime.Object {
	return &usagev1alpha1.BillingAccountBalance{}
}

// NewList returns an empty BillingAccountBalanceList.
func (r *REST) NewList() runtime.Object {
	return &usagev1alpha1.BillingAccountBalanceList{}
}

// Destroy cleans up resources on shutdown.
func (r *REST) Destroy() {}

// NamespaceScoped returns true: BillingAccountBalance is read in the billing
// account's namespace.
func (r *REST) NamespaceScoped() bool {
	return true
}

// GetSingularName returns the singular resource name.
func (r *REST) GetSingularName() string {
	return "billingaccountbalance"
}

// Get answers a GET request for one billing account's balance.
func (r *REST) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	ns := request.NamespaceValue(ctx)
	if ns == "" {
		return nil, apierrors.NewBadRequest("a namespace must be specified")
	}

	var account billingv1alpha1.BillingAccount
	if err := r.milo.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &account); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, apierrors.NewNotFound(usagev1alpha1.Resource("billingaccountbalances"), name)
		}
		return nil, apierrors.NewInternalError(fmt.Errorf("reading BillingAccount %s/%s: %w", ns, name, err))
	}

	return r.computeBalance(ctx, &account)
}

// List answers a LIST request across billing accounts in a namespace.
func (r *REST) List(ctx context.Context, options *metainternalversion.ListOptions) (runtime.Object, error) {
	var list billingv1alpha1.BillingAccountList
	var listOpts []client.ListOption
	if ns := request.NamespaceValue(ctx); ns != "" {
		listOpts = append(listOpts, client.InNamespace(ns))
	}
	if options != nil && options.LabelSelector != nil && !options.LabelSelector.Empty() {
		listOpts = append(listOpts, client.MatchingLabelsSelector{Selector: options.LabelSelector})
	}
	if err := r.milo.List(ctx, &list, listOpts...); err != nil {
		return nil, apierrors.NewInternalError(fmt.Errorf("listing BillingAccounts: %w", err))
	}

	items := make([]usagev1alpha1.BillingAccountBalance, 0, len(list.Items))
	for i := range list.Items {
		account := &list.Items[i]
		if options != nil && options.FieldSelector != nil && !options.FieldSelector.Matches(fields.Set{"metadata.name": account.Name}) {
			continue
		}
		bal, err := r.computeBalance(ctx, account)
		if err != nil {
			return nil, err
		}
		items = append(items, *bal)
	}

	return &usagev1alpha1.BillingAccountBalanceList{
		Items: items,
	}, nil
}

func (r *REST) computeBalance(ctx context.Context, account *billingv1alpha1.BillingAccount) (*usagev1alpha1.BillingAccountBalance, error) {
	now := r.now().UTC()
	customerKey := openmeter.CustomerKey(account.UID)

	// 1. Look up OpenMeter customer
	cust, err := r.openMeter.GetCustomer(ctx, customerKey)
	if errors.Is(err, openmeter.ErrCustomerNotFound) {
		periodStart, periodEnd, _ := usagequery.AccountBillingPeriod(ctx, r.openMeter, customerKey, now)
		return defaultBalance(account, now, periodStart, periodEnd), nil
	}
	if err != nil {
		return nil, apierrors.NewInternalError(fmt.Errorf("reading customer for %s/%s: %w", account.Namespace, account.Name, err))
	}

	customerID := openmeter.CustomerID(cust.Id)

	// 2. Query credit balance
	creditRemaining, err := r.openMeter.GetCreditBalance(ctx, customerID)
	if err != nil {
		return nil, apierrors.NewInternalError(fmt.Errorf("reading credit balance for %s/%s: %w", account.Namespace, account.Name, err))
	}
	creditRemainingDec, err := alpacadecimal.NewFromString(creditRemaining)
	if err != nil {
		creditRemainingDec = alpacadecimal.Zero
	}

	// 3. Query invoices for amountOwed
	invoices, err := r.openMeter.ListInvoices(ctx, customerID)
	if err != nil {
		return nil, apierrors.NewInternalError(fmt.Errorf("listing invoices for %s/%s: %w", account.Namespace, account.Name, err))
	}
	amountOwedDec := computeAmountOwed(invoices)

	// 4. Query credit grants
	grants, err := r.openMeter.ListCreditGrants(ctx, customerID)
	if err != nil {
		return nil, apierrors.NewInternalError(fmt.Errorf("listing credit grants for %s/%s: %w", account.Namespace, account.Name, err))
	}
	balanceGrants := computeBalanceGrants(grants, creditRemainingDec)

	// 5. Billing period and forecast
	periodStart, periodEnd, err := usagequery.AccountBillingPeriod(ctx, r.openMeter, customerKey, now)
	if err != nil {
		return nil, apierrors.NewInternalError(fmt.Errorf("determining billing period for %s/%s: %w", account.Namespace, account.Name, err))
	}

	estimatedSpendDec, err := usagequery.CalculateAccountSpend(ctx, r.milo, r.openMeter, account, customerID, periodStart, now)
	if err != nil {
		return nil, apierrors.NewInternalError(fmt.Errorf("calculating estimated spend for %s/%s: %w", account.Namespace, account.Name, err))
	}

	estimatedCreditUsedDec := alpacadecimal.Min(estimatedSpendDec, creditRemainingDec)
	estimatedAmountDueDec := estimatedSpendDec.Sub(estimatedCreditUsedDec)
	estimatedCreditRemainingDec := creditRemainingDec.Sub(estimatedCreditUsedDec)

	return &usagev1alpha1.BillingAccountBalance{
		ObjectMeta: metav1.ObjectMeta{
			Name:              account.Name,
			Namespace:         account.Namespace,
			CreationTimestamp: metav1.NewTime(now),
		},
		Status: usagev1alpha1.BillingAccountBalanceStatus{
			RetrievedAt: metav1.NewTime(now),
			Currency:    "USD",
			Balance: usagev1alpha1.AccountBalanceSummary{
				CreditRemaining: creditRemainingDec.StringFixed(2),
				AmountOwed:      amountOwedDec.StringFixed(2),
			},
			Forecast: usagev1alpha1.AccountBalanceForecast{
				PeriodStart:              metav1.NewTime(periodStart),
				PeriodEnd:                metav1.NewTime(periodEnd),
				EstimatedSpend:           estimatedSpendDec.StringFixed(2),
				EstimatedCreditUsed:      estimatedCreditUsedDec.StringFixed(2),
				EstimatedAmountDue:       estimatedAmountDueDec.StringFixed(2),
				EstimatedCreditRemaining: estimatedCreditRemainingDec.StringFixed(2),
			},
			Grants: balanceGrants,
		},
	}, nil
}

func computeAmountOwed(invoices []om.Invoice) alpacadecimal.Decimal {
	totalDue := alpacadecimal.Zero
	for _, inv := range invoices {
		switch inv.Status {
		case om.InvoiceStatusIssued, om.InvoiceStatusPaymentProcessing, om.InvoiceStatusOverdue, om.InvoiceStatusUncollectible:
			if dec, err := alpacadecimal.NewFromString(inv.Totals.Total); err == nil {
				totalDue = totalDue.Add(dec)
			}
		default:
			// Paid, Voided, Draft, Gathering, Issuing do not add to amount owed.
		}
	}
	return totalDue
}

func computeBalanceGrants(grants []omv3.BillingCreditGrant, creditRemaining alpacadecimal.Decimal) []usagev1alpha1.BillingAccountBalanceGrant {
	var active []omv3.BillingCreditGrant
	for _, g := range grants {
		if g.Status == omv3.BillingCreditGrantStatusActive {
			active = append(active, g)
		}
	}
	if len(active) == 0 {
		return nil
	}

	// Sort soonest-expiring first. If neither expires, sort by earlier creation time.
	slices.SortStableFunc(active, func(a, b omv3.BillingCreditGrant) int {
		if a.ExpiresAt != nil && b.ExpiresAt != nil {
			if a.ExpiresAt.Before(*b.ExpiresAt) {
				return -1
			}
			if a.ExpiresAt.After(*b.ExpiresAt) {
				return 1
			}
			return a.CreatedAt.Compare(b.CreatedAt)
		}
		if a.ExpiresAt != nil && b.ExpiresAt == nil {
			return -1
		}
		if a.ExpiresAt == nil && b.ExpiresAt != nil {
			return 1
		}
		return a.CreatedAt.Compare(b.CreatedAt)
	})

	totalGranted := alpacadecimal.Zero
	for _, g := range active {
		if dec, err := alpacadecimal.NewFromString(string(g.Amount)); err == nil {
			totalGranted = totalGranted.Add(dec)
		}
	}

	usedTotal := totalGranted.Sub(creditRemaining)
	if usedTotal.IsNegative() {
		usedTotal = alpacadecimal.Zero
	}

	res := make([]usagev1alpha1.BillingAccountBalanceGrant, len(active))
	for i, g := range active {
		amountDec, _ := alpacadecimal.NewFromString(string(g.Amount))
		grantUsed := alpacadecimal.Min(amountDec, usedTotal)
		grantRem := amountDec.Sub(grantUsed)
		usedTotal = usedTotal.Sub(grantUsed)

		var expAt *metav1.Time
		if g.ExpiresAt != nil {
			t := g.ExpiresAt.Truncate(time.Second)
			expAt = &metav1.Time{Time: t}
		}

		res[i] = usagev1alpha1.BillingAccountBalanceGrant{
			Name:      g.Name,
			Amount:    amountDec.StringFixed(2),
			Used:      grantUsed.StringFixed(2),
			Remaining: grantRem.StringFixed(2),
			ExpiresAt: expAt,
		}
	}
	return res
}

func defaultBalance(account *billingv1alpha1.BillingAccount, now, periodStart, periodEnd time.Time) *usagev1alpha1.BillingAccountBalance {
	return &usagev1alpha1.BillingAccountBalance{
		ObjectMeta: metav1.ObjectMeta{
			Name:              account.Name,
			Namespace:         account.Namespace,
			CreationTimestamp: metav1.NewTime(now),
		},
		Status: usagev1alpha1.BillingAccountBalanceStatus{
			RetrievedAt: metav1.NewTime(now),
			Currency:    "USD",
			Balance: usagev1alpha1.AccountBalanceSummary{
				CreditRemaining: "0.00",
				AmountOwed:      "0.00",
			},
			Forecast: usagev1alpha1.AccountBalanceForecast{
				PeriodStart:              metav1.NewTime(periodStart),
				PeriodEnd:                metav1.NewTime(periodEnd),
				EstimatedSpend:           "0.00",
				EstimatedCreditUsed:      "0.00",
				EstimatedAmountDue:       "0.00",
				EstimatedCreditRemaining: "0.00",
			},
		},
	}
}
