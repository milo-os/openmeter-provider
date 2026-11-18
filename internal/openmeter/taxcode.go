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

	omv3 "github.com/openmeterio/openmeter/api/v3"
)

// StripeNontaxableTaxCode is Stripe's tax code for a product that's never
// taxed. A pricing that isn't taxable gets it, so Stripe Tax charges no tax
// on its lines wherever the customer is.
const StripeNontaxableTaxCode = "txcd_00000000"

// taxCodePageSize is how many tax codes one page of the listing holds.
const taxCodePageSize = 100

// taxCodeKey is the key of the OpenMeter tax code for a Stripe tax code.
// OpenMeter keys are lower case letters and digits joined by single
// underscores, at most 64 characters.
func taxCodeKey(stripeCode string) string {
	var b strings.Builder
	b.WriteString("stripe")
	pendingSep := true
	for _, r := range strings.ToLower(stripeCode) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pendingSep {
				b.WriteByte('_')
				pendingSep = false
			}
			b.WriteRune(r)
			continue
		}
		pendingSep = true
	}
	key := b.String()
	if len(key) > 64 {
		key = strings.TrimRight(key[:64], "_")
	}
	return key
}

// EnsureTaxCode returns the id of the OpenMeter tax code that maps to
// stripeCode in the Stripe app, creating one if there's none. A rate card
// refers to its tax code by this id.
func (c *client) EnsureTaxCode(ctx context.Context, stripeCode string) (string, error) {
	if stripeCode == "" {
		return "", &PermanentError{Err: errors.New("a Stripe tax code is required")}
	}
	if id, err := c.findTaxCode(ctx, stripeCode); err != nil || id != "" {
		return id, err
	}

	var created omv3.BillingTaxCode
	status, err := c.v3.do(ctx, http.MethodPost, "/tax-codes", nil, omv3.CreateTaxCodeRequest{
		Key:  taxCodeKey(stripeCode),
		Name: stripeCode,
		AppMappings: []omv3.BillingTaxCodeAppMapping{{
			AppType: omv3.BillingAppTypeStripe,
			TaxCode: stripeCode,
		}},
	}, &created)
	if err == nil {
		return created.Id, nil
	}
	// Another reconcile created it first.
	if status == http.StatusConflict {
		if id, findErr := c.findTaxCode(ctx, stripeCode); findErr != nil || id != "" {
			return id, findErr
		}
	}
	return "", fmt.Errorf("create tax code %q: %w", stripeCode, err)
}

// findTaxCode returns the id of the live tax code mapping to stripeCode in
// the Stripe app, or "" when there's none.
func (c *client) findTaxCode(ctx context.Context, stripeCode string) (string, error) {
	for page := 1; ; page++ {
		query := url.Values{
			"page[number]": {strconv.Itoa(page)},
			"page[size]":   {strconv.Itoa(taxCodePageSize)},
		}
		var resp omv3.TaxCodePagePaginatedResponse
		if _, err := c.v3.do(ctx, http.MethodGet, "/tax-codes", query, nil, &resp); err != nil {
			return "", fmt.Errorf("list tax codes: %w", err)
		}
		for _, tc := range resp.Data {
			if tc.DeletedAt != nil {
				continue
			}
			for _, m := range tc.AppMappings {
				if m.AppType == omv3.BillingAppTypeStripe && m.TaxCode == stripeCode {
					return tc.Id, nil
				}
			}
		}
		if len(resp.Data) < taxCodePageSize {
			return "", nil
		}
	}
}
