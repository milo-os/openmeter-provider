// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/alpacahq/alpacadecimal"
	omv3 "github.com/openmeterio/openmeter/api/v3"
	"github.com/openmeterio/openmeter/openmeter/productcatalog"
	"github.com/openmeterio/openmeter/pkg/models"
)

// mustPrice converts a v3 rate card price, as plans carry it.
func mustPrice(t *testing.T, raw string) *productcatalog.Price {
	t.Helper()
	var p omv3.BillingPrice
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	price, err := usagePrice(p)
	if err != nil {
		t.Fatal(err)
	}
	return price
}

// graduated: 0-100 at 1, 100-1000 at 0.5, above at 0.1.
const graduatedPrice = `{"type":"graduated","tiers":[
	{"up_to_amount":"100","unit_price":{"type":"unit","amount":"1"}},
	{"up_to_amount":"1000","unit_price":{"type":"unit","amount":"0.5"}},
	{"unit_price":{"type":"unit","amount":"0.1"}}]}`

// perGiB converts bytes (or byte-seconds) into GiB (GiB-seconds).
func perGiB(t *testing.T) *productcatalog.UnitConfig {
	t.Helper()
	uc, err := unitConfig(omv3.BillingUnitConfig{
		Operation: omv3.BillingUnitConfigOperationDivide, ConversionFactor: "1073741824",
	})
	if err != nil {
		t.Fatal(err)
	}
	return uc
}

func TestPriceUsage(t *testing.T) {
	from := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		price    string
		quantity string
		earlier  string
		want     string
	}{
		{"unit price", `{"type":"unit","amount":"0.000001"}`, "4300000", "0", "4.3"},
		{"unit price ignores earlier usage", `{"type":"unit","amount":"2"}`, "3", "500", "6"},
		{"graduated from the first tier", graduatedPrice, "150", "0", "125"},
		{"graduated continues from earlier usage", graduatedPrice, "150", "950", "35"},
		{"graduated past every bound", graduatedPrice, "150", "5000", "15"},
		{"no usage", graduatedPrice, "0", "0", "0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PriceUsage(UsageToPrice{
				Price:                 mustPrice(t, tt.price),
				Currency:              "USD",
				From:                  from,
				To:                    from.Add(72 * time.Hour),
				Quantity:              alpacadecimal.RequireFromString(tt.quantity),
				PreLinePeriodQuantity: alpacadecimal.RequireFromString(tt.earlier),
			})
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(alpacadecimal.RequireFromString(tt.want)) {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

// TestPriceUsage_ConvertsUsage pins pricing in the pricing unit: usage
// metered in bytes is priced per GiB, tier bounds included, the way the
// invoice prices it (verified against OpenMeter: 3,600 GiB-s of memory and
// 300 GiB of egress invoiced $5.02).
func TestPriceUsage_ConvertsUsage(t *testing.T) {
	const gib = 1 << 30
	// Egress: the first 200 GiB free, then 0.05 per GiB up to 10 TiB.
	egress := `{"type":"graduated","tiers":[
		{"up_to_amount":"200","unit_price":{"type":"unit","amount":"0"}},
		{"up_to_amount":"10240","unit_price":{"type":"unit","amount":"0.05"}},
		{"unit_price":{"type":"unit","amount":"0.04"}}]}`
	from := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name              string
		price             string
		quantity, earlier int64
		want              string
	}{
		{"memory per GiB-second", `{"type":"unit","amount":"0.0000045"}`, 3600 * gib, 0, "0.02"},
		{"egress tiers in GiB", egress, 300 * gib, 0, "5"},
		{"egress continues from earlier GiB", egress, 150 * gib, 100 * gib, "2.5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PriceUsage(UsageToPrice{
				Price:                 mustPrice(t, tt.price),
				UnitConfig:            perGiB(t),
				Currency:              "USD",
				From:                  from,
				To:                    from.Add(time.Hour),
				Quantity:              alpacadecimal.NewFromInt(tt.quantity),
				PreLinePeriodQuantity: alpacadecimal.NewFromInt(tt.earlier),
			})
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(alpacadecimal.RequireFromString(tt.want)) {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestPriceUsage_RoundsToTheCurrency(t *testing.T) {
	from := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	got, err := PriceUsage(UsageToPrice{
		Price:    mustPrice(t, `{"type":"unit","amount":"0.001"}`),
		Currency: "USD",
		From:     from, To: from.Add(time.Hour),
		Quantity: alpacadecimal.RequireFromString("1234"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 1.234 USD is invoiced as 1.23.
	if got.String() != "1.23" {
		t.Errorf("got %s, want 1.23", got)
	}
}

func TestPriceNeedsEarlierUsage(t *testing.T) {
	if PriceNeedsEarlierUsage(mustPrice(t, `{"type":"unit","amount":"1"}`)) {
		t.Error("a unit price does not depend on earlier usage")
	}
	if !PriceNeedsEarlierUsage(mustPrice(t, graduatedPrice)) {
		t.Error("a tiered price depends on earlier usage")
	}
}

func TestPriceUsage_Discounts(t *testing.T) {
	from := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	pctDiscount := func(pct float64) *productcatalog.Discounts {
		return &productcatalog.Discounts{
			Percentage: &productcatalog.PercentageDiscount{
				Percentage: models.NewPercentage(pct),
			},
		}
	}
	tests := []struct {
		name      string
		price     string
		quantity  string
		discounts *productcatalog.Discounts
		want      string
	}{
		{
			name:      "unit price 15% discount",
			price:     `{"type":"unit","amount":"1"}`,
			quantity:  "10", // 10 * 1 = 10, net 15% discount = 8.5
			discounts: pctDiscount(15),
			want:      "8.5",
		},
		{
			name:      "unit price 100% discount",
			price:     `{"type":"unit","amount":"1"}`,
			quantity:  "10", // 10 * 1 = 10, net 100% discount = 0
			discounts: pctDiscount(100),
			want:      "0",
		},
		{
			name:      "unit price 0% discount",
			price:     `{"type":"unit","amount":"1"}`,
			quantity:  "10",
			discounts: pctDiscount(0),
			want:      "10",
		},
		{
			name:      "graduated price 15% discount",
			price:     graduatedPrice,
			quantity:  "150", // 100*1 + 50*0.5 = 125, 125 * 0.85 = 106.25
			discounts: pctDiscount(15),
			want:      "106.25",
		},
		{
			name:      "graduated price 100% discount",
			price:     graduatedPrice,
			quantity:  "150",
			discounts: pctDiscount(100),
			want:      "0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PriceUsage(UsageToPrice{
				Price:     mustPrice(t, tt.price),
				Currency:  "USD",
				From:      from,
				To:        from.Add(time.Hour),
				Quantity:  alpacadecimal.RequireFromString(tt.quantity),
				Discounts: tt.discounts,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(alpacadecimal.RequireFromString(tt.want)) {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

// TestPriceUsage_DiscountsWithEarlierUsage prices the usage of the
// usagequery-spend e2e suite's discounted meters: tiers (1 up to 10, 0.5 up
// to 100) continuing from the usage earlier in the billing period, and a flat
// price, each net of the Offer's discount.
func TestPriceUsage_DiscountsWithEarlierUsage(t *testing.T) {
	from := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	pct := func(p int) *productcatalog.Discounts {
		return &productcatalog.Discounts{
			Percentage: &productcatalog.PercentageDiscount{Percentage: models.NewPercentage(p)},
		}
	}
	const storage = `{"type":"graduated","tiers":[
		{"up_to_amount":"10","unit_price":{"type":"unit","amount":"1"}},
		{"up_to_amount":"100","unit_price":{"type":"unit","amount":"0.5"}},
		{"unit_price":{"type":"unit","amount":"0.1"}}]}`
	const backup = `{"type":"unit","amount":"0.2"}`
	tests := []struct {
		name      string
		price     string
		quantity  int64
		earlier   int64
		discounts *productcatalog.Discounts
		want      string
	}{
		// 10 x 1 + 5 x 0.5 = 12.5, less 20%.
		{"graduated 20% from the start", storage, 15, 0, pct(20), "10"},
		// Continues after 5 units: 5 x 1 + 5 x 0.5 = 7.5, less 20%.
		{"graduated 20% continuing after earlier usage", storage, 10, 5, pct(20), "6"},
		// The same usage without the discount, for the list price.
		{"graduated list price continuing after earlier usage", storage, 10, 5, nil, "7.5"},
		// 40 x 0.2 = 8 and 30 x 0.2 = 6, less 100%.
		{"flat 100% whole", backup, 40, 0, pct(100), "0"},
		{"flat 100% from W", backup, 30, 10, pct(100), "0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PriceUsage(UsageToPrice{
				Price:                 mustPrice(t, tt.price),
				Currency:              "USD",
				From:                  from,
				To:                    from.Add(time.Hour),
				Quantity:              alpacadecimal.NewFromInt(tt.quantity),
				PreLinePeriodQuantity: alpacadecimal.NewFromInt(tt.earlier),
				Discounts:             tt.discounts,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(alpacadecimal.RequireFromString(tt.want)) {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}
