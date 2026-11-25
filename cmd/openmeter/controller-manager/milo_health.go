// SPDX-License-Identifier: AGPL-3.0-only

package controllermanager

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// reconcileTimeout bounds every reconcile, so a request to Milo that
	// never returns is cancelled and retried rather than holding its
	// controller's one worker for good. A billing account's reconcile
	// makes many OpenMeter calls of up to 30 seconds each, so it's
	// generous.
	reconcileTimeout = 10 * time.Minute

	// miloCheckInterval is how often the Milo check asks Milo.
	miloCheckInterval = 20 * time.Second
	// miloCheckTimeout bounds each of its requests.
	miloCheckTimeout = 10 * time.Second
	// miloUnreachableAfter is how long Milo must go unanswered before
	// liveness fails and the kubelet restarts the pod. Long enough that
	// a blip or a Milo rollout doesn't restart it.
	miloUnreachableAfter = 2 * time.Minute
)

// miloCheck is a liveness check on the connection the controllers use.
// The leader's Lease is on the local cluster, not Milo, so a replica keeps
// it while every request on the controllers' connection hangs: it stays
// leader and writes no invoices. The check reads a namespace through the
// controllers' client, uncached, and fails once nothing has come back for
// miloUnreachableAfter.
//
// The request runs in the background on miloCheckInterval, and the probe
// only reports what it last saw: a probe that waited on a slow Milo would
// outlast the kubelet's probe timeout and restart a healthy pod.
type miloCheck struct {
	reader client.Reader
	now    func() time.Time

	mu       sync.Mutex
	lastOK   time.Time
	lastFail error
}

func newMiloCheck(reader client.Reader) *miloCheck {
	c := &miloCheck{reader: reader, now: time.Now}
	// Count from start, so a replica that never reaches Milo fails too.
	c.lastOK = c.now()
	return c
}

// Start asks Milo every miloCheckInterval until ctx ends. It's a
// manager.Runnable.
func (c *miloCheck) Start(ctx context.Context) error {
	ticker := time.NewTicker(miloCheckInterval)
	defer ticker.Stop()
	for {
		c.probe(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// NeedLeaderElection is false: every replica checks its own connection,
// since a standby takes over with the one it has.
func (c *miloCheck) NeedLeaderElection() bool { return false }

// probe asks Milo once and records whether it answered.
func (c *miloCheck) probe(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, miloCheckTimeout)
	defer cancel()
	err := c.reader.Get(ctx, types.NamespacedName{Name: metav1.NamespaceDefault}, &corev1.Namespace{})

	c.mu.Lock()
	defer c.mu.Unlock()
	// Any answer from Milo, a refusal included, shows the connection works.
	if err == nil || isAPIAnswer(err) {
		c.lastOK, c.lastFail = c.now(), nil
		return
	}
	c.lastFail = err
}

// Check is a healthz.Checker. It doesn't call Milo, so it answers at once.
func (c *miloCheck) Check(*http.Request) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	since := c.now().Sub(c.lastOK)
	if since <= miloUnreachableAfter {
		return nil
	}
	if c.lastFail != nil {
		return fmt.Errorf("no answer from Milo for %s: %w", since.Round(time.Second), c.lastFail)
	}
	return fmt.Errorf("no answer from Milo for %s", since.Round(time.Second))
}

// isAPIAnswer reports whether err is a response from the API server rather
// than a failure to reach it.
func isAPIAnswer(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	return !apierrors.IsTimeout(err) && !apierrors.IsServerTimeout(err)
}
