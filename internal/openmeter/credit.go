// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/alpacahq/alpacadecimal"
	omv3 "github.com/openmeterio/openmeter/api/v3"
)

const creditGrantListPageSize = 100

// DesiredCreditGrant is the controller-facing representation of a credit grant.
type DesiredCreditGrant struct {
	// Name is the display name of the grant (e.g. CreditGrant metadata.name).
	Name string
	// Amount is the credit amount as a decimal string (e.g. "500.00").
	Amount string
	// Currency defaults to "USD".
	Currency string
	// Description explains why the credit was granted.
	Description string
	// ExpiresAfter is an optional duration after effective time when the grant expires.
	ExpiresAfter *time.Duration
	// Key is the idempotency key for grant creation (CreditGrant metadata.uid).
	Key string
}

func (d DesiredCreditGrant) currency() string {
	if d.Currency == "" {
		return "USD"
	}
	return d.Currency
}

// formatDurationISO8601 converts a time.Duration to an ISO 8601 duration string (e.g. "PT720H").
func formatDurationISO8601(d time.Duration) string {
	totalSeconds := int64(d.Seconds())
	if totalSeconds <= 0 {
		return ""
	}
	hours := totalSeconds / 3600
	minutes := (totalSeconds % 3600) / 60
	seconds := totalSeconds % 60

	var b strings.Builder
	b.WriteString("PT")
	if hours > 0 {
		fmt.Fprintf(&b, "%dH", hours)
	}
	if minutes > 0 {
		fmt.Fprintf(&b, "%dM", minutes)
	}
	if seconds > 0 || (hours == 0 && minutes == 0) {
		fmt.Fprintf(&b, "%dS", seconds)
	}
	return b.String()
}

// EnsureCreditGrant creates a credit grant in OpenMeter if absent, or returns
// the existing grant matching desired.Key (the CreditGrant UID). Idempotent.
func (c *client) EnsureCreditGrant(
	ctx context.Context,
	customerID CustomerID,
	desired DesiredCreditGrant,
) (omv3.BillingCreditGrant, error) {
	if customerID == "" {
		return omv3.BillingCreditGrant{}, &PermanentError{Err: errors.New("customerID is required")}
	}
	if desired.Key == "" {
		return omv3.BillingCreditGrant{}, &PermanentError{Err: errors.New("grant key is required")}
	}
	if desired.Amount == "" {
		return omv3.BillingCreditGrant{}, &PermanentError{Err: errors.New("grant amount is required")}
	}

	path := "/customers/" + url.PathEscape(string(customerID)) + "/credits/grants"
	req := omv3.CreateCreditGrantRequest{
		Amount:        desired.Amount,
		Currency:      omv3.CreateCurrencyCode(desired.currency()),
		FundingMethod: omv3.BillingCreditFundingMethodNone,
		Key:           &desired.Key,
		Name:          desired.Name,
		Description:   &desired.Description,
	}
	if desired.ExpiresAfter != nil && *desired.ExpiresAfter > 0 {
		iso := formatDurationISO8601(*desired.ExpiresAfter)
		if iso != "" {
			req.ExpiresAfter = &iso
		}
	}

	var grant omv3.BillingCreditGrant
	status, err := c.v3.do(ctx, http.MethodPost, path, nil, req, &grant)
	if err == nil {
		return grant, nil
	}

	if status == http.StatusConflict {
		// Key already exists for this customer; find and return the existing grant.
		grants, listErr := c.ListCreditGrants(ctx, customerID)
		if listErr != nil {
			return omv3.BillingCreditGrant{}, fmt.Errorf("fetch existing grant after conflict: %w", listErr)
		}
		for _, g := range grants {
			if g.Key != nil && *g.Key == desired.Key {
				return g, nil
			}
		}
	}

	return omv3.BillingCreditGrant{}, err
}

// GetCreditGrant fetches a credit grant by customerID and grantID.
func (c *client) GetCreditGrant(
	ctx context.Context,
	customerID CustomerID,
	grantID string,
) (omv3.BillingCreditGrant, error) {
	if customerID == "" {
		return omv3.BillingCreditGrant{}, &PermanentError{Err: errors.New("customerID is required")}
	}
	if grantID == "" {
		return omv3.BillingCreditGrant{}, &PermanentError{Err: errors.New("grantID is required")}
	}

	path := "/customers/" + url.PathEscape(string(customerID)) + "/credits/grants/" + url.PathEscape(grantID)
	var grant omv3.BillingCreditGrant
	if _, err := c.v3.do(ctx, http.MethodGet, path, nil, nil, &grant); err != nil {
		return omv3.BillingCreditGrant{}, err
	}
	return grant, nil
}

// ListCreditGrants returns every credit grant for customerID.
func (c *client) ListCreditGrants(
	ctx context.Context,
	customerID CustomerID,
) ([]omv3.BillingCreditGrant, error) {
	if customerID == "" {
		return nil, &PermanentError{Err: errors.New("customerID is required")}
	}

	path := "/customers/" + url.PathEscape(string(customerID)) + "/credits/grants"
	var all []omv3.BillingCreditGrant
	for page := 1; ; page++ {
		q := url.Values{
			"page[size]":   {strconv.Itoa(creditGrantListPageSize)},
			"page[number]": {strconv.Itoa(page)},
		}
		var rsp omv3.CreditGrantPagePaginatedResponse
		if _, err := c.v3.do(ctx, http.MethodGet, path, q, nil, &rsp); err != nil {
			return nil, err
		}
		all = append(all, rsp.Data...)
		if len(rsp.Data) == 0 || page*creditGrantListPageSize >= int(rsp.Meta.Page.Total) {
			return all, nil
		}
	}
}

// VoidCreditGrant voids an active credit grant in OpenMeter, forfeiting any
// remaining balance. Idempotent: voiding an already-voided grant succeeds.
func (c *client) VoidCreditGrant(
	ctx context.Context,
	customerID CustomerID,
	grantID string,
) error {
	if customerID == "" {
		return &PermanentError{Err: errors.New("customerID is required")}
	}
	if grantID == "" {
		return &PermanentError{Err: errors.New("grantID is required")}
	}

	path := "/customers/" + url.PathEscape(string(customerID)) + "/credits/grants/" + url.PathEscape(grantID) + "/void"
	paymentAdj := omv3.BillingCreditGrantVoidPaymentAdjustmentNone
	body := omv3.VoidCreditGrantRequest{
		PaymentAdjustment: &paymentAdj,
	}

	status, err := c.v3.do(ctx, http.MethodPost, path, nil, body, nil)
	if err == nil || status == http.StatusNotFound {
		return nil
	}
	if status == http.StatusConflict {
		// A conflict can occur if the grant is already voided, expired, or fully consumed.
		// Check the grant status; if not active, consider it voided/done.
		if g, getErr := c.GetCreditGrant(ctx, customerID, grantID); getErr == nil {
			if g.Status == omv3.BillingCreditGrantStatusVoided || g.Status == omv3.BillingCreditGrantStatusExpired {
				return nil
			}
		}
	}
	return err
}

// VoidAllCustomerGrants voids every active credit grant for customerID.
func (c *client) VoidAllCustomerGrants(ctx context.Context, customerID CustomerID) error {
	grants, err := c.ListCreditGrants(ctx, customerID)
	if err != nil {
		return err
	}

	var errs []error
	for _, g := range grants {
		if g.Status == omv3.BillingCreditGrantStatusActive {
			if err := c.VoidCreditGrant(ctx, customerID, g.Id); err != nil {
				errs = append(errs, fmt.Errorf("void grant %s: %w", g.Id, err))
			}
		}
	}
	return errors.Join(errs...)
}

// GetCustomerCreditBalances returns the full credit balance breakdown for customerID.
func (c *client) GetCustomerCreditBalances(
	ctx context.Context,
	customerID CustomerID,
) (omv3.BillingCreditBalances, error) {
	if customerID == "" {
		return omv3.BillingCreditBalances{}, &PermanentError{Err: errors.New("customerID is required")}
	}

	path := "/customers/" + url.PathEscape(string(customerID)) + "/credits/balance"
	var rsp omv3.BillingCreditBalances
	status, err := c.v3.do(ctx, http.MethodGet, path, nil, nil, &rsp)
	if status == http.StatusNotFound {
		return omv3.BillingCreditBalances{RetrievedAt: time.Now()}, nil
	}
	if err != nil {
		return omv3.BillingCreditBalances{}, err
	}
	return rsp, nil
}

// GetCreditBalance returns the customer's posted credit remaining balance,
// formatted with 2 decimal places (e.g. "130.00").
func (c *client) GetCreditBalance(ctx context.Context, customerID CustomerID) (string, error) {
	balances, err := c.GetCustomerCreditBalances(ctx, customerID)
	if err != nil {
		return "0.00", err
	}

	for _, b := range balances.Balances {
		if string(b.Currency) == "USD" {
			dec, err := alpacadecimal.NewFromString(string(b.Settled))
			if err == nil {
				return dec.StringFixed(2), nil
			}
			return string(b.Settled), nil
		}
	}
	if len(balances.Balances) > 0 {
		dec, err := alpacadecimal.NewFromString(string(balances.Balances[0].Settled))
		if err == nil {
			return dec.StringFixed(2), nil
		}
		return string(balances.Balances[0].Settled), nil
	}

	return "0.00", nil
}
