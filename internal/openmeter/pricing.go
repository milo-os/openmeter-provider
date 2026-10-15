// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/alpacahq/alpacadecimal"
	om "github.com/openmeterio/openmeter/api/client/go"

	"github.com/openmeterio/openmeter/api"
	"github.com/openmeterio/openmeter/openmeter/billing"
	"github.com/openmeterio/openmeter/openmeter/billing/rating"
	ratingservice "github.com/openmeterio/openmeter/openmeter/billing/rating/service"
	productcataloghttp "github.com/openmeterio/openmeter/openmeter/productcatalog/http"
	"github.com/openmeterio/openmeter/pkg/currencyx"
	"github.com/openmeterio/openmeter/pkg/timeutil"
)

// UsageToPrice is usage of one rate card over part of one billing period.
type UsageToPrice struct {
	// Price is the rate card's usage-based price.
	Price om.RateCardUsageBasedPrice
	// Currency is the ISO 4217 code the subscription is billed in.
	Currency string
	// From is the inclusive start and To the exclusive end of the usage, all
	// within one billing period.
	From time.Time
	To   time.Time
	// Quantity is the usage between From and To.
	Quantity alpacadecimal.Decimal
	// PreLinePeriodQuantity is the usage earlier in the same billing period,
	// before From. Tiered prices continue from it.
	PreLinePeriodQuantity alpacadecimal.Decimal
}

// usageRater is OpenMeter's own rating engine, the code that prices invoice
// lines. It needs no backend.
var usageRater = ratingservice.New(ratingservice.Config{})

// PriceUsage returns what u costs, the way OpenMeter prices it on an invoice
// line, before taxes and credits. Discounts and a minimum commitment are not
// applied: the provider's plans have no discounts, and u may cover only
// part of a billing period.
//
// OpenMeter's SimulateInvoice endpoint would do the same, but in
// v1.0.0-beta.232 it creates lines without a line engine and prices every
// line at 0 (`invalid line engine type`).
func PriceUsage(u UsageToPrice) (alpacadecimal.Decimal, error) {
	// The client and server API types are generated from the same schema,
	// so the client's price decodes into the server's.
	raw, err := json.Marshal(u.Price)
	if err != nil {
		return alpacadecimal.Zero, fmt.Errorf("encode price: %w", err)
	}
	var apiPrice api.RateCardUsageBasedPrice
	if err := json.Unmarshal(raw, &apiPrice); err != nil {
		return alpacadecimal.Zero, fmt.Errorf("decode price: %w", err)
	}
	price, err := productcataloghttp.AsPrice(apiPrice)
	if err != nil {
		return alpacadecimal.Zero, fmt.Errorf("price: %w", err)
	}
	if price == nil {
		return alpacadecimal.Zero, errors.New("rate card has no price")
	}

	quantity, preQuantity := u.Quantity, u.PreLinePeriodQuantity
	line := &billing.StandardLine{
		StandardLineBase: billing.StandardLineBase{
			Currency:  currencyx.FiatCode(u.Currency),
			Period:    timeutil.ClosedPeriod{From: u.From, To: u.To},
			InvoiceAt: u.To,
		},
		UsageBased: &billing.UsageBasedLine{
			Price:                        price,
			Quantity:                     &quantity,
			MeteredQuantity:              &quantity,
			PreLinePeriodQuantity:        &preQuantity,
			MeteredPreLinePeriodQuantity: &preQuantity,
		},
	}

	result, err := usageRater.GenerateDetailedLines(line,
		rating.WithMinimumCommitmentIgnored(),
		rating.WithCreditsMutatorDisabled())
	if err != nil {
		return alpacadecimal.Zero, fmt.Errorf("rate usage: %w", err)
	}
	t := result.Totals
	return t.Amount.Add(t.ChargesTotal).Sub(t.DiscountsTotal), nil
}

// PriceNeedsEarlierUsage reports whether a price depends on the usage
// earlier in the billing period (tiered and package prices), so
// PreLinePeriodQuantity must be filled in. Unit prices do not.
func PriceNeedsEarlierUsage(p om.RateCardUsageBasedPrice) bool {
	discriminator, err := p.Discriminator()
	if err != nil {
		return true
	}
	return discriminator != string(om.UnitPriceWithCommitmentsTypeUnit)
}
