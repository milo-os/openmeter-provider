// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/alpacahq/alpacadecimal"
	om "github.com/openmeterio/openmeter/api/client/go"
)

func mustPrice(t *testing.T, raw string) om.RateCardUsageBasedPrice {
	t.Helper()
	var p om.RateCardUsageBasedPrice
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// graduated: 0-100 at 1, 100-1000 at 0.5, above at 0.1.
const graduatedPrice = `{"type":"tiered","mode":"graduated","tiers":[
	{"upToAmount":"100","unitPrice":{"type":"unit","amount":"1"}},
	{"upToAmount":"1000","unitPrice":{"type":"unit","amount":"0.5"}},
	{"unitPrice":{"type":"unit","amount":"0.1"}}]}`

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
