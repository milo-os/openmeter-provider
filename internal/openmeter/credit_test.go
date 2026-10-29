// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	omv3 "github.com/openmeterio/openmeter/api/v3"
)

func (f *fakeServer) serveCredits(w http.ResponseWriter, r *http.Request) bool {
	const prefix = "/api/v3/openmeter/customers/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		return false
	}
	customerID := parts[0]
	if parts[1] != "credits" {
		return false
	}

	writeJSON := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}

	// /api/v3/openmeter/customers/{customerId}/credits/balance
	if len(parts) == 3 && parts[2] == "balance" && r.Method == http.MethodGet {
		b, ok := f.creditBalances[customerID]
		if !ok {
			writeJSON(http.StatusOK, omv3.BillingCreditBalances{
				RetrievedAt: time.Now(),
				Balances:    []omv3.CreditBalance{},
			})
			return true
		}
		writeJSON(http.StatusOK, b)
		return true
	}

	// /api/v3/openmeter/customers/{customerId}/credits/grants
	if len(parts) == 3 && parts[2] == "grants" {
		switch r.Method {
		case http.MethodGet:
			grants := f.creditGrants[customerID]
			total := len(grants)
			writeJSON(http.StatusOK, omv3.CreditGrantPagePaginatedResponse{
				Data: grants,
				Meta: omv3.PaginatedMeta{
					Page: omv3.PageMeta{
						Size:   100,
						Number: 1,
						Total:  float32(total),
					},
				},
			})
			return true
		case http.MethodPost:
			var req omv3.CreateCreditGrantRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
				return true
			}
			// Check idempotency key
			if req.Key != nil && *req.Key != "" {
				for _, existing := range f.creditGrants[customerID] {
					if existing.Key != nil && *existing.Key == *req.Key {
						writeJSON(http.StatusConflict, map[string]string{"error": "conflict"})
						return true
					}
				}
			}

			f.creditGrantSeq++
			id := fmt.Sprintf("grant-%d", f.creditGrantSeq)
			grant := omv3.BillingCreditGrant{
				Id:            id,
				Name:          req.Name,
				Amount:        req.Amount,
				Currency:      omv3.BillingCurrencyCode(req.Currency),
				FundingMethod: req.FundingMethod,
				Key:           req.Key,
				Description:   req.Description,
				Status:        omv3.BillingCreditGrantStatusActive,
				CreatedAt:     time.Now(),
				UpdatedAt:     time.Now(),
			}
			f.creditGrants[customerID] = append(f.creditGrants[customerID], grant)
			writeJSON(http.StatusCreated, grant)
			return true
		}
	}

	// /api/v3/openmeter/customers/{customerId}/credits/grants/{grantId}
	if len(parts) == 4 && parts[2] == "grants" && r.Method == http.MethodGet {
		grantID := parts[3]
		for _, g := range f.creditGrants[customerID] {
			if g.Id == grantID {
				writeJSON(http.StatusOK, g)
				return true
			}
		}
		writeJSON(http.StatusNotFound, map[string]string{"error": "not found"})
		return true
	}

	// /api/v3/openmeter/customers/{customerId}/credits/grants/{grantId}/void
	if len(parts) == 5 && parts[2] == "grants" && parts[4] == "void" && r.Method == http.MethodPost {
		grantID := parts[3]
		for i, g := range f.creditGrants[customerID] {
			if g.Id == grantID {
				if g.Status == omv3.BillingCreditGrantStatusVoided {
					// idempotent success
					writeJSON(http.StatusOK, g)
					return true
				}
				now := time.Now()
				f.creditGrants[customerID][i].Status = omv3.BillingCreditGrantStatusVoided
				f.creditGrants[customerID][i].VoidedAt = &now
				writeJSON(http.StatusOK, f.creditGrants[customerID][i])
				return true
			}
		}
		writeJSON(http.StatusNotFound, map[string]string{"error": "not found"})
		return true
	}

	return false
}

func TestEnsureCreditGrant(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	customerID := CustomerID("cust-1")

	desired := DesiredCreditGrant{
		Name:        "welcome",
		Amount:      "50.00",
		Currency:    "USD",
		Description: "signup credit",
		Key:         "uid-grant-1",
	}

	grant, err := c.EnsureCreditGrant(ctx, customerID, desired)
	if err != nil {
		t.Fatalf("EnsureCreditGrant failed: %v", err)
	}
	if grant.Name != "welcome" || grant.Amount != "50.00" || grant.Status != omv3.BillingCreditGrantStatusActive {
		t.Errorf("unexpected grant: %+v", grant)
	}

	// Conflict recovery returns existing
	grant2, err := c.EnsureCreditGrant(ctx, customerID, desired)
	if err != nil {
		t.Fatalf("EnsureCreditGrant conflict retry failed: %v", err)
	}
	if grant2.Id != grant.Id {
		t.Errorf("grant ID = %s, want %s", grant2.Id, grant.Id)
	}
	if len(f.creditGrants[string(customerID)]) != 1 {
		t.Errorf("expected 1 grant, got %d", len(f.creditGrants[string(customerID)]))
	}
}

func TestVoidCreditGrant(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	customerID := CustomerID("cust-1")

	desired := DesiredCreditGrant{
		Name:     "goodwill",
		Amount:   "10.00",
		Currency: "USD",
		Key:      "uid-grant-2",
	}
	grant, err := c.EnsureCreditGrant(ctx, customerID, desired)
	if err != nil {
		t.Fatalf("EnsureCreditGrant failed: %v", err)
	}

	if err := c.VoidCreditGrant(ctx, customerID, grant.Id); err != nil {
		t.Fatalf("VoidCreditGrant failed: %v", err)
	}

	if f.creditGrants[string(customerID)][0].Status != omv3.BillingCreditGrantStatusVoided {
		t.Errorf("status = %s, want voided", f.creditGrants[string(customerID)][0].Status)
	}

	// Idempotent void succeeds
	if err := c.VoidCreditGrant(ctx, customerID, grant.Id); err != nil {
		t.Errorf("VoidCreditGrant retry failed: %v", err)
	}

	// Non-existent grant returns nil (treated as already gone)
	if err := c.VoidCreditGrant(ctx, customerID, "non-existent"); err != nil {
		t.Errorf("VoidCreditGrant on missing grant failed: %v", err)
	}
}

func TestVoidAllCustomerGrants(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	customerID := CustomerID("cust-1")

	_, _ = c.EnsureCreditGrant(ctx, customerID, DesiredCreditGrant{Name: "g1", Amount: "10.00", Key: "k1"})
	_, _ = c.EnsureCreditGrant(ctx, customerID, DesiredCreditGrant{Name: "g2", Amount: "20.00", Key: "k2"})

	if err := c.VoidAllCustomerGrants(ctx, customerID); err != nil {
		t.Fatalf("VoidAllCustomerGrants failed: %v", err)
	}

	for _, g := range f.creditGrants[string(customerID)] {
		if g.Status != omv3.BillingCreditGrantStatusVoided {
			t.Errorf("grant %s status = %s, want voided", g.Id, g.Status)
		}
	}
}

func TestGetCreditBalance(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	customerID := CustomerID("cust-1")

	// Empty balances returns 0.00
	bal, err := c.GetCreditBalance(ctx, customerID)
	if err != nil {
		t.Fatalf("GetCreditBalance failed: %v", err)
	}
	if bal != "0.00" {
		t.Errorf("balance = %s, want 0.00", bal)
	}

	// Set balance
	f.creditBalances[string(customerID)] = omv3.BillingCreditBalances{
		RetrievedAt: time.Now(),
		Balances: []omv3.CreditBalance{
			{
				Currency: "USD",
				Settled:  "130.50",
				Live:     "130.50",
			},
		},
	}

	bal, err = c.GetCreditBalance(ctx, customerID)
	if err != nil {
		t.Fatalf("GetCreditBalance with balance failed: %v", err)
	}
	if bal != "130.50" {
		t.Errorf("balance = %s, want 130.50", bal)
	}
}

func TestFormatDurationISO8601(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, ""},
		{720 * time.Hour, "PT720H"},
		{2160 * time.Hour, "PT2160H"},
		{90 * time.Minute, "PT1H30M"},
		{45 * time.Second, "PT45S"},
	}
	for _, tt := range tests {
		got := formatDurationISO8601(tt.d)
		if got != tt.want {
			t.Errorf("formatDurationISO8601(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
