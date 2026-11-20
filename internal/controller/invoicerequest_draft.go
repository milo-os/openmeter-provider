// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
)

// organizationNamespacePrefix starts an organization's namespace, which
// ends with the organization's name.
const organizationNamespacePrefix = "organization-"

// previewContractDraft works out the first invoice of a contract that isn't
// published yet, from spec.contractDraft: its Offer, start date and billing
// day. It's priced as a published contract's would be, through Stripe when
// it can be.
func (r *InvoiceRequestReconciler) previewContractDraft(
	ctx context.Context,
	ir *billingv1alpha1.InvoiceRequest,
	account *billingv1alpha1.BillingAccount,
) (ctrl.Result, error) {
	draft := ir.Spec.ContractDraft
	var offer billingv1alpha1.Offer
	if err := r.Get(ctx, types.NamespacedName{Name: draft.OfferRef.Name}, &offer); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get offer: %w", err)
		}
		return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed,
			fmt.Sprintf("Offer %q not found", draft.OfferRef.Name))
	}
	if !contractOfferFor(&offer, account) {
		return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed,
			fmt.Sprintf("Offer %q isn't a contract for billing account %q", offer.Name, account.Name))
	}

	// A draft has no snapshot yet: billing takes one when it's published.
	if len(offer.Spec.ServicePricings) == 0 {
		snapshots, missing, err := r.draftSnapshots(ctx, &offer)
		if err != nil {
			return ctrl.Result{}, err
		}
		if missing != "" {
			return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed,
				fmt.Sprintf("ServicePricing %q of Offer %q not found", missing, offer.Name))
		}
		offer.Spec.ServicePricings = snapshots
	}

	startsAt := draft.StartsAt.UTC()
	starts := startsAt.Format(time.DateOnly)
	invoiceDay := 0
	if draft.BillingDay == billingv1alpha1.ContractBillingDayInvoiceDay {
		invoiceDay = subscriptionBillingAnchor(account).Day()
	}
	charges, total, err := firstInvoiceCharges(&offer, startsAt, invoiceDay)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, ir, billingv1alpha1.InvoiceRequestReasonFailed, err.Error())
	}
	if len(charges) == 0 {
		return ctrl.Result{}, r.succeed(ctx, ir, account.Spec.CurrencyCode, nil, "0",
			fmt.Sprintf("The draft contract has no one-time or recurring charges to invoice when it starts on %s", starts))
	}
	return r.previewFirstInvoiceResult(ctx, ir, account, charges, total,
		fmt.Sprintf("The draft contract's first invoice, sent when it starts on %s if it's published", starts))
}

// contractOfferFor reports whether an Offer is the account's own contract,
// by the labels the staff portal sets on it, so a request can't preview
// another account's.
func contractOfferFor(offer *billingv1alpha1.Offer, account *billingv1alpha1.BillingAccount) bool {
	org := strings.TrimPrefix(account.Namespace, organizationNamespacePrefix)
	return offer.Labels[billingv1alpha1.ContractBillingAccountLabel] == account.Name &&
		offer.Labels[billingv1alpha1.ContractOrganizationLabel] == org
}

// draftSnapshots are the snapshots billing would take of an Offer's
// servicePricingRefs when it's published (billing's buildSnapshots): each
// pricing with the ref's discount, or the Offer's, its quantity (1 for a
// fee without one), billing timing and periods. missing names a pricing
// that isn't there.
func (r *InvoiceRequestReconciler) draftSnapshots(
	ctx context.Context, offer *billingv1alpha1.Offer,
) ([]billingv1alpha1.ServicePricingSnapshot, string, error) {
	snapshots := make([]billingv1alpha1.ServicePricingSnapshot, 0, len(offer.Spec.ServicePricingRefs))
	for _, ref := range offer.Spec.ServicePricingRefs {
		ns := ref.Namespace
		if ns == "" {
			ns = billingv1alpha1.DefaultServicePricingNamespace
		}
		var sp billingv1alpha1.ServicePricing
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &sp); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, ref.Name, nil
			}
			return nil, "", fmt.Errorf("get service pricing %s/%s: %w", ns, ref.Name, err)
		}
		discount := ref.Discount
		if discount == nil {
			discount = offer.Spec.Discount
		}
		quantity := ref.Quantity
		if quantity == 0 && sp.Spec.ChargeType != billingv1alpha1.ChargeTypeUsage {
			quantity = 1
		}
		snapshots = append(snapshots, billingv1alpha1.ServicePricingSnapshot{
			Name:          sp.Name,
			Discount:      discount.DeepCopy(),
			Quantity:      quantity,
			BillingTiming: ref.BillingTiming,
			Periods:       ref.Periods,
			Spec:          *sp.Spec.DeepCopy(),
		})
	}
	return snapshots, "", nil
}
