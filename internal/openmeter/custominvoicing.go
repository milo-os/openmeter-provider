// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// OpenMeter's Custom Invoicing app leaves writing an invoice to us. With
// both sync hooks on, an invoice stops twice on its way to being issued:
//
//   - in draft.syncing, until DraftSynchronized says the invoice has been
//     written (its Stripe draft holds its lines);
//   - in issuing.syncing, until IssuingSynchronized says it has been
//     issued (Stripe has finalized it, and given it a number).
//
// Nothing times either out: an invoice no one answers waits for good, and
// one waiting in issuing.syncing can't be voided or deleted.
const (
	// InvoiceExtendedStatusDraftSyncing is an invoice waiting for its
	// draft to be written.
	InvoiceExtendedStatusDraftSyncing = "draft.syncing"
	// InvoiceExtendedStatusIssuingSyncing is an invoice waiting to be
	// issued.
	InvoiceExtendedStatusIssuingSyncing = "issuing.syncing"
)

// PaymentTrigger is a change to an invoice's payment that Custom Invoicing
// is told about, since it learns nothing from Stripe itself.
type PaymentTrigger = om.CustomInvoicingPaymentTrigger

const (
	PaymentTriggerPaid          = om.CustomInvoicingPaymentTriggerPaid
	PaymentTriggerFailed        = om.CustomInvoicingPaymentTriggerPaymentFailed
	PaymentTriggerOverdue       = om.CustomInvoicingPaymentTriggerPaymentOverdue
	PaymentTriggerUncollectible = om.CustomInvoicingPaymentTriggerPaymentUncollectible
	PaymentTriggerActionNeeded  = om.CustomInvoicingPaymentTriggerActionRequired
	PaymentTriggerVoid          = om.CustomInvoicingPaymentTriggerVoid
)

// LineExternalID pairs an OpenMeter invoice line (top-level or detailed)
// with the id it has where the invoice was written.
type LineExternalID struct {
	LineID     string
	ExternalID string
}

// EnsureCustomInvoicingApp returns the id of the Custom Invoicing app
// called name, installing it if there's none, with both sync hooks on.
//
// It's installed without a billing profile of its own: profiles are made
// per account (see DesiredBillingProfile.AppID). OpenMeter installs the
// app with its hooks off, and an invoice reaching the app then is issued
// without waiting for us, so they're turned on before the id is returned,
// and before any profile can use it.
func (c *client) EnsureCustomInvoicingApp(ctx context.Context, name string) (string, error) {
	app, err := c.findCustomInvoicingApp(ctx, name)
	if err != nil {
		return "", err
	}
	if app == nil {
		resp, err := c.api.MarketplaceAppInstallWithResponse(ctx, om.AppTypeCustomInvoicing, om.MarketplaceInstallRequestPayload{
			Name:                 ptr(name),
			CreateBillingProfile: ptr(false),
		})
		if err != nil {
			return "", classify(nil, nil, err)
		}
		if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
			return "", fmt.Errorf("install Custom Invoicing app %q: %w", name, err)
		}
		// The install response doesn't carry the hook settings: read the
		// app back.
		if app, err = c.findCustomInvoicingApp(ctx, name); err != nil {
			return "", err
		}
		if app == nil {
			return "", fmt.Errorf("install Custom Invoicing app %q: not listed after installing", name)
		}
	}
	if app.EnableDraftSyncHook && app.EnableIssuingSyncHook {
		return app.Id, nil
	}
	var body om.AppReplaceUpdate
	if err := body.FromCustomInvoicingAppReplaceUpdate(om.CustomInvoicingAppReplaceUpdate{
		Type:                  om.CustomInvoicingAppReplaceUpdateTypeCustomInvoicing,
		Name:                  app.Name,
		Description:           app.Description,
		Metadata:              app.Metadata,
		EnableDraftSyncHook:   true,
		EnableIssuingSyncHook: true,
	}); err != nil {
		return "", &PermanentError{Err: fmt.Errorf("encode Custom Invoicing app update: %w", err)}
	}
	resp, err := c.api.UpdateAppWithResponse(ctx, app.Id, body)
	if err != nil {
		return "", classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return "", fmt.Errorf("turn on Custom Invoicing app %s's sync hooks: %w", app.Id, err)
	}
	return app.Id, nil
}

// findCustomInvoicingApp returns the live Custom Invoicing app called
// name, the oldest if there's more than one, or nil.
func (c *client) findCustomInvoicingApp(ctx context.Context, name string) (*om.CustomInvoicingApp, error) {
	pageSize := om.PaginationPageSize(100)
	var found *om.CustomInvoicingApp
	for page := 1; ; page++ {
		pageNum := om.PaginationPage(page)
		resp, err := c.api.ListAppsWithResponse(ctx, &om.ListAppsParams{Page: &pageNum, PageSize: &pageSize})
		if err != nil {
			return nil, classify(nil, nil, err)
		}
		if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
			return nil, fmt.Errorf("list apps: %w", err)
		}
		if resp.JSON200 == nil {
			return nil, &PermanentError{Err: errors.New("list apps: empty response body")}
		}
		for _, item := range resp.JSON200.Items {
			kind, err := item.Discriminator()
			if err != nil || kind != string(om.AppTypeCustomInvoicing) {
				continue
			}
			app, err := item.AsCustomInvoicingApp()
			if err != nil || app.Name != name || app.DeletedAt != nil {
				continue
			}
			if found == nil || app.CreatedAt.Before(found.CreatedAt) {
				found = &app
			}
		}
		seen := resp.JSON200.PageSize
		if seen <= 0 {
			seen = len(resp.JSON200.Items)
		}
		if len(resp.JSON200.Items) == 0 || page*seen >= resp.JSON200.TotalCount {
			return found, nil
		}
	}
}

// ListInvoicesAwaitingSync returns the customer's invoices waiting on a
// Custom Invoicing hook (draft.syncing or issuing.syncing), with their
// lines and detailed lines. They're any app's: callers check
// InvoiceInvoicingApp.
func (c *client) ListInvoicesAwaitingSync(ctx context.Context, customerID CustomerID) ([]om.Invoice, error) {
	return c.listInvoicesWith(ctx, customerID, func(p *om.ListInvoicesParams) {
		p.ExtendedStatuses = &om.InvoiceListParamsExtendedStatuses{
			InvoiceExtendedStatusDraftSyncing, InvoiceExtendedStatusIssuingSyncing,
		}
		p.Expand = &om.InvoiceListParamsExpand{om.InvoiceExpandLines}
	})
}

// InvoiceInvoicingApp is the id of the app that invoices inv, from its
// workflow snapshot, or "".
func InvoiceInvoicingApp(inv om.Invoice) string {
	if inv.Workflow.Apps == nil {
		return ""
	}
	refs, err := inv.Workflow.Apps.AsBillingProfileAppReferences()
	if err != nil {
		return ""
	}
	return refs.Invoicing.Id
}

// DraftSynchronized tells Custom Invoicing that the invoice's draft has
// been written: externalID is where (the Stripe invoice), and lines map its
// lines to their items there. OpenMeter refuses it unless the invoice is in
// draft.syncing, and refuses all of it if any line id isn't the invoice's.
func (c *client) DraftSynchronized(ctx context.Context, invoiceID, externalID string, lines []LineExternalID) error {
	mapping := make([]om.CustomInvoicingLineExternalIdMapping, 0, len(lines))
	for _, l := range lines {
		mapping = append(mapping, om.CustomInvoicingLineExternalIdMapping{LineId: l.LineID, ExternalId: l.ExternalID})
	}
	resp, err := c.api.AppCustomInvoicingDraftSynchronizedWithResponse(ctx, invoiceID, om.CustomInvoicingDraftSynchronizedRequest{
		Invoicing: &om.CustomInvoicingSyncResult{
			ExternalId:      ptr(externalID),
			LineExternalIds: &mapping,
		},
	})
	if err != nil {
		return classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return fmt.Errorf("draft synchronized for invoice %s: %w", invoiceID, err)
	}
	return nil
}

// IssuingSynchronized tells Custom Invoicing that the invoice has been
// issued as number, by the Stripe invoice paymentID. Without a number
// OpenMeter would make up its own, so it's required.
func (c *client) IssuingSynchronized(ctx context.Context, invoiceID, number, paymentID string, sentAt *time.Time) error {
	if number == "" {
		return &PermanentError{Err: fmt.Errorf("issuing synchronized for invoice %s: no invoice number", invoiceID)}
	}
	resp, err := c.api.AppCustomInvoicingIssuingSynchronizedWithResponse(ctx, invoiceID, om.CustomInvoicingFinalizedRequest{
		Invoicing: &om.CustomInvoicingFinalizedInvoicingRequest{
			InvoiceNumber:    ptr(om.InvoiceNumber(number)),
			SentToCustomerAt: sentAt,
		},
		Payment: &om.CustomInvoicingFinalizedPaymentRequest{ExternalId: ptr(paymentID)},
	})
	if err != nil {
		return classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return fmt.Errorf("issuing synchronized for invoice %s: %w", invoiceID, err)
	}
	return nil
}

// UpdatePaymentStatus tells Custom Invoicing what happened to the
// invoice's payment.
func (c *client) UpdatePaymentStatus(ctx context.Context, invoiceID string, trigger PaymentTrigger) error {
	resp, err := c.api.AppCustomInvoicingUpdatePaymentStatusWithResponse(ctx, invoiceID,
		om.CustomInvoicingUpdatePaymentStatusRequest{Trigger: trigger})
	if err != nil {
		return classify(nil, nil, err)
	}
	if err := classify(resp.HTTPResponse, resp.Body, nil); err != nil {
		return fmt.Errorf("payment status %s for invoice %s: %w", trigger, invoiceID, err)
	}
	return nil
}

// DeleteInvoice deletes an invoice that hasn't been issued, such as a
// draft with nothing on it to bill.
func (c *client) DeleteInvoice(ctx context.Context, invoiceID string) error {
	return c.deleteInvoice(ctx, invoiceID)
}
