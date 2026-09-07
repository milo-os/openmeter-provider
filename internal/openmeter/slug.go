// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"strings"
)

// meterSlug derives the OpenMeter meter slug from the canonical reverse-DNS
// MeterDefinition.meterName (D2).
//
// OpenMeter slugs must match `^[a-z0-9]+(?:_[a-z0-9]+)*$` — lowercase
// alphanumeric segments joined by underscores, with no dots, slashes, or
// hyphens. Milo meterNames are reverse-DNS paths such as
// "compute.miloapis.com/instance/cpu-seconds".
//
// The transformation is deterministic and 1:1 for a given input: every '.'
// and '/' is replaced with '_' and the result is lowercased. This makes the
// mapping stable across reconciles (CRD → slug) and reverses easily enough to
// rebuild on ingest (eventType → slug). The original meterName is always
// carried verbatim in the meter's eventType, so event routing never depends on
// the slug encoding.
//
// The deduplication collision case is handled by the caller: if two distinct
// meterNames could map to the same slug (e.g. "a/b.c" vs "a.b/c"), the
// second is detectable via GetMeter and the reconciler surfaces it rather than
// silently overwriting.
func MeterSlug(meterName string) string {
	replacer := strings.NewReplacer(".", "_", "/", "_")
	return replacer.Replace(strings.ToLower(meterName))
}
