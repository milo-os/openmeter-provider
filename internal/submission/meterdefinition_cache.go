// SPDX-License-Identifier: AGPL-3.0-only

package submission

import (
	"context"
	"fmt"
	"sync"

	toolscache "k8s.io/client-go/tools/cache"
	runtimecache "sigs.k8s.io/controller-runtime/pkg/cache"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
)

// MeterDefinitionCache maintains a thread-safe in-memory set of meter names
// (spec.meterName) currently in Published or Deprecated phase. Entries in
// other phases (Draft, Retired) are removed.
//
// Simpler than amberflo-provider's equivalent cache: OpenMeter's meter is
// resolved server-side from a CloudEvent's Type field, which EnsureMeter
// (see meter.go) already sets to spec.meterName directly — there is no
// separate "API name" to translate to, only a validity check.
type MeterDefinitionCache struct {
	mu    sync.RWMutex
	names map[string]struct{}
}

// NewMeterDefinitionCache registers event handlers on the MeterDefinition
// informer and returns a cache ready to use once the manager cache syncs.
func NewMeterDefinitionCache(ctx context.Context, c runtimecache.Cache) (*MeterDefinitionCache, error) {
	mc := &MeterDefinitionCache{
		names: make(map[string]struct{}),
	}

	informer, err := c.GetInformer(ctx, &billingv1alpha1.MeterDefinition{})
	if err != nil {
		return nil, fmt.Errorf("getting MeterDefinition informer: %w", err)
	}

	if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if md, ok := obj.(*billingv1alpha1.MeterDefinition); ok {
				mc.upsert(md)
			}
		},
		UpdateFunc: func(_, newObj any) {
			if md, ok := newObj.(*billingv1alpha1.MeterDefinition); ok {
				mc.upsert(md)
			}
		},
		DeleteFunc: func(obj any) {
			if tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
				obj = tombstone.Obj
			}
			if md, ok := obj.(*billingv1alpha1.MeterDefinition); ok {
				mc.delete(md)
			}
		},
	}); err != nil {
		return nil, fmt.Errorf("adding MeterDefinition event handler: %w", err)
	}

	return mc, nil
}

func (m *MeterDefinitionCache) upsert(md *billingv1alpha1.MeterDefinition) {
	if md.Spec.Phase != billingv1alpha1.PhasePublished && md.Spec.Phase != billingv1alpha1.PhaseDeprecated {
		m.delete(md)
		return
	}
	m.mu.Lock()
	m.names[md.Spec.MeterName] = struct{}{}
	m.mu.Unlock()
}

func (m *MeterDefinitionCache) delete(md *billingv1alpha1.MeterDefinition) {
	m.mu.Lock()
	delete(m.names, md.Spec.MeterName)
	m.mu.Unlock()
}

// IsValid reports whether meterName belongs to a MeterDefinition currently
// in Published or Deprecated phase.
func (m *MeterDefinitionCache) IsValid(meterName string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.names[meterName]
	return ok
}
