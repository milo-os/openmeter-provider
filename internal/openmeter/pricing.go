// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alpacahq/alpacadecimal"
	om "github.com/openmeterio/openmeter/api/client/go"

	omv3 "github.com/openmeterio/openmeter/api/v3"
	"github.com/openmeterio/openmeter/openmeter/billing"
	"github.com/openmeterio/openmeter/openmeter/billing/rating"
	ratingservice "github.com/openmeterio/openmeter/openmeter/billing/rating/service"
	"github.com/openmeterio/openmeter/openmeter/productcatalog"
	"github.com/openmeterio/openmeter/pkg/currencyx"
	"github.com/openmeterio/openmeter/pkg/timeutil"
)

// UsageRateCard is one usage-based rate card of a plan version, with what it
// takes to price its usage.
type UsageRateCard struct {
	// Key is the rate card key.
	Key string
	// MeterSlug and Filters select the usage the card prices: its feature's
	// meter, narrowed by the feature's group-by filters.
	MeterSlug string
	Filters   map[string]om.FilterString
	// BillingCadence overrides the subscription's cadence when set.
	BillingCadence string
	// Price is the card's price, per pricing unit.
	Price *productcatalog.Price
	// UnitConfig converts metered usage into the pricing unit, when set.
	UnitConfig *productcatalog.UnitConfig
}

// PlanUsageRateCards returns the usage-based rate cards of the plan version
// with the given id. Flat fees are left out.
func (c *client) PlanUsageRateCards(ctx context.Context, planID string) ([]UsageRateCard, error) {
	plan, err := c.GetPlan(ctx, planID)
	if err != nil {
		return nil, fmt.Errorf("get plan %s: %w", planID, err)
	}
	if len(PlanFeatureIDs(plan)) == 0 {
		return nil, nil
	}
	// Older plan versions may reference features archived since, which
	// cannot be read by id.
	features, err := c.listFeatures(ctx, nil, true)
	if err != nil {
		return nil, fmt.Errorf("list features: %w", err)
	}
	byID := make(map[string]om.Feature, len(features))
	for _, f := range features {
		byID[f.Id] = f
	}

	var out []UsageRateCard
	for _, phase := range plan.Phases {
		for _, rc := range phase.RateCards {
			if rc.Feature == nil {
				continue
			}
			feature, ok := byID[rc.Feature.Id]
			if !ok || feature.MeterSlug == nil {
				return nil, fmt.Errorf("rate card %s: feature %s not found or has no meter", rc.Key, rc.Feature.Id)
			}
			price, err := usagePrice(rc.Price)
			if err != nil {
				return nil, fmt.Errorf("rate card %s: %w", rc.Key, err)
			}
			card := UsageRateCard{Key: rc.Key, MeterSlug: *feature.MeterSlug, Price: price}
			if feature.AdvancedMeterGroupByFilters != nil {
				card.Filters = *feature.AdvancedMeterGroupByFilters
			}
			if rc.BillingCadence != nil {
				card.BillingCadence = *rc.BillingCadence
			}
			if rc.UnitConfig != nil {
				if card.UnitConfig, err = unitConfig(*rc.UnitConfig); err != nil {
					return nil, fmt.Errorf("rate card %s: %w", rc.Key, err)
				}
			}
			out = append(out, card)
		}
	}
	return out, nil
}

// usagePrice converts the usage prices this provider writes (unit and
// graduated) into OpenMeter's rating model.
func usagePrice(p omv3.BillingPrice) (*productcatalog.Price, error) {
	disc, err := p.Discriminator()
	if err != nil {
		return nil, fmt.Errorf("read price type: %w", err)
	}
	switch disc {
	case string(omv3.BillingPriceUnitTypeUnit):
		unit, err := p.AsBillingPriceUnit()
		if err != nil {
			return nil, err
		}
		amount, err := alpacadecimal.NewFromString(unit.Amount)
		if err != nil {
			return nil, fmt.Errorf("unit price amount %q: %w", unit.Amount, err)
		}
		return productcatalog.NewPriceFrom(productcatalog.UnitPrice{Amount: amount}), nil
	case string(omv3.BillingPriceGraduatedTypeGraduated):
		graduated, err := p.AsBillingPriceGraduated()
		if err != nil {
			return nil, err
		}
		tiers := make([]productcatalog.PriceTier, 0, len(graduated.Tiers))
		for _, t := range graduated.Tiers {
			var tier productcatalog.PriceTier
			if t.UpToAmount != nil {
				upTo, err := alpacadecimal.NewFromString(*t.UpToAmount)
				if err != nil {
					return nil, fmt.Errorf("tier upToAmount %q: %w", *t.UpToAmount, err)
				}
				tier.UpToAmount = &upTo
			}
			if t.UnitPrice != nil {
				amount, err := alpacadecimal.NewFromString(t.UnitPrice.Amount)
				if err != nil {
					return nil, fmt.Errorf("tier unit price %q: %w", t.UnitPrice.Amount, err)
				}
				tier.UnitPrice = &productcatalog.PriceTierUnitPrice{Amount: amount}
			}
			if t.FlatPrice != nil {
				amount, err := alpacadecimal.NewFromString(t.FlatPrice.Amount)
				if err != nil {
					return nil, fmt.Errorf("tier flat price %q: %w", t.FlatPrice.Amount, err)
				}
				tier.FlatPrice = &productcatalog.PriceTierFlatPrice{Amount: amount}
			}
			tiers = append(tiers, tier)
		}
		return productcatalog.NewPriceFrom(productcatalog.TieredPrice{
			Mode:  productcatalog.GraduatedTieredPrice,
			Tiers: tiers,
		}), nil
	}
	return nil, fmt.Errorf("unsupported usage price type %q", disc)
}

// unitConfig converts a rate card's unit conversion into OpenMeter's rating
// model.
func unitConfig(uc omv3.BillingUnitConfig) (*productcatalog.UnitConfig, error) {
	factor, err := alpacadecimal.NewFromString(uc.ConversionFactor)
	if err != nil {
		return nil, fmt.Errorf("unit conversion factor %q: %w", uc.ConversionFactor, err)
	}
	out := &productcatalog.UnitConfig{
		Operation:        productcatalog.UnitConfigOperation(uc.Operation),
		ConversionFactor: factor,
		DisplayUnit:      uc.DisplayUnit,
	}
	if uc.Rounding != nil {
		out.Rounding = productcatalog.UnitConfigRoundingMode(*uc.Rounding)
	}
	if uc.Precision != nil {
		out.Precision = *uc.Precision
	}
	return out, out.Validate()
}

// UsageToPrice is usage of one rate card over part of one billing period.
type UsageToPrice struct {
	// Price is the rate card's price and UnitConfig its unit conversion.
	Price      *productcatalog.Price
	UnitConfig *productcatalog.UnitConfig
	// Currency is the ISO 4217 code the subscription is billed in.
	Currency string
	// From is the inclusive start and To the exclusive end of the usage, all
	// within one billing period.
	From time.Time
	To   time.Time
	// Quantity is the metered usage between From and To.
	Quantity alpacadecimal.Decimal
	// PreLinePeriodQuantity is the metered usage earlier in the same billing
	// period, before From. Tiered prices continue from it.
	PreLinePeriodQuantity alpacadecimal.Decimal
}

// usageRater is OpenMeter's own rating engine, the code that prices invoice
// lines, unit conversion included. It needs no backend.
var usageRater = ratingservice.New(ratingservice.Config{UnitConfigEnabled: true})

// PriceUsage returns what u costs, the way OpenMeter prices it on an invoice
// line, before taxes and credits. Discounts and a minimum commitment are not
// applied: the provider's plans have no discounts, and u may cover only
// part of a billing period.
//
// OpenMeter's SimulateInvoice endpoint would do the same, but in
// v1.0.0-beta.232 it creates lines without a line engine and prices every
// line at 0 (`invalid line engine type`).
func PriceUsage(u UsageToPrice) (alpacadecimal.Decimal, error) {
	if u.Price == nil {
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
			Price:                        u.Price,
			Quantity:                     &quantity,
			MeteredQuantity:              &quantity,
			PreLinePeriodQuantity:        &preQuantity,
			MeteredPreLinePeriodQuantity: &preQuantity,
			UnitConfig:                   u.UnitConfig,
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
func PriceNeedsEarlierUsage(p *productcatalog.Price) bool {
	return p == nil || p.Type() != productcatalog.UnitPriceType
}
