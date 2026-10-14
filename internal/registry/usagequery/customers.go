// SPDX-License-Identifier: AGPL-3.0-only

package usagequery

import (
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/types"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

// customerIDTTL bounds how long a billing account's OpenMeter customer id is
// reused. The customer is keyed by the account's UID and only recreated if
// it was deleted, so a stale id is rare; the TTL keeps it from lasting.
const customerIDTTL = 10 * time.Minute

// missingCustomerTTL is shorter: a new billing account gets its customer
// within moments, and its first usage should show up soon after.
const missingCustomerTTL = time.Minute

// customerLookups bounds concurrent customer lookups for one query.
const customerLookups = 8

// customerResolver turns billing accounts into OpenMeter customer ids. The
// meter query's customer filter takes only OpenMeter's own ids, while
// customers are keyed by the account's UID (see openmeter.CustomerKey).
type customerResolver struct {
	openMeter openmeter.Client
	now       func() time.Time

	mu    sync.Mutex
	cache map[types.UID]cachedCustomer
}

type cachedCustomer struct {
	// id is empty when the account has no customer yet.
	id      openmeter.CustomerID
	expires time.Time
}

func newCustomerResolver(openMeter openmeter.Client) *customerResolver {
	return &customerResolver{
		openMeter: openMeter,
		now:       time.Now,
		cache:     map[types.UID]cachedCustomer{},
	}
}

// customerIDs returns the customer ids of accounts, in order. Accounts
// without a customer (never provisioned, so no usage) are left out.
func (r *customerResolver) customerIDs(ctx context.Context, accounts []billingv1alpha1.BillingAccount) ([]openmeter.CustomerID, error) {
	ids := make([]openmeter.CustomerID, len(accounts))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(customerLookups)
	for i := range accounts {
		g.Go(func() error {
			id, err := r.customerID(gctx, accounts[i].UID)
			ids[i] = id
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	out := ids[:0]
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

func (r *customerResolver) customerID(ctx context.Context, uid types.UID) (openmeter.CustomerID, error) {
	now := r.now()

	r.mu.Lock()
	cached, ok := r.cache[uid]
	r.mu.Unlock()
	if ok && now.Before(cached.expires) {
		return cached.id, nil
	}

	var id openmeter.CustomerID
	ttl := customerIDTTL
	customer, err := r.openMeter.GetCustomer(ctx, openmeter.CustomerKey(uid))
	switch {
	case errors.Is(err, openmeter.ErrCustomerNotFound):
		ttl = missingCustomerTTL
	case err != nil:
		return "", err
	default:
		id = openmeter.CustomerID(customer.Id)
	}

	r.mu.Lock()
	r.cache[uid] = cachedCustomer{id: id, expires: now.Add(ttl)}
	r.mu.Unlock()
	return id, nil
}
