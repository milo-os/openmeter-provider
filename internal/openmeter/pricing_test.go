// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/alpacahq/alpacadecimal"
	omv3 "github.com/openmeterio/openmeter/api/v3"
	"github.com/openmeterio/openmeter/openmeter/productcatalog"
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
