#!/bin/sh
# Asserts the reconcile loop has actually SETTLED on a meter: samples
# updatedAt, waits long enough to span several reconciles, and fails if it
# moved. Mirrors billingaccount's verify-converged.sh — see its own comment
# for the exact class of bug this catches (a drift check comparing against a
# value the backend silently normalizes or never actually stores, which
# looks converged in every "is the right value present" assertion while
# quietly reissuing a write forever).
#
# MeterDefinitionReconciler has no periodic resync and no cross-resource
# watch (only reconciling on a change to the MeterDefinition object itself —
# see its own SetupWithManager doc comment), so a passive sleep alone
# wouldn't exercise anything: nothing would trigger a fresh Reconcile pass
# during the wait. The caller must re-apply the MeterDefinition (even
# unchanged) immediately before invoking this script, to produce a real
# watch event and a genuine "reconcile an already-converged spec" pass —
# exactly the scenario a never-converging drift check would fail.
#
# Usage: verify-meter-converged.sh <slug>
set -eu

slug="$1"
url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/meters/${slug}"

fetch_updated_at() {
  kubectl exec -n openmeter-system openmeter-query -- wget -q -O - "$url" 2>/dev/null \
    | sed -n 's/.*"updatedAt":"\([^"]*\)".*/\1/p'
}

before="$(fetch_updated_at)"
if [ -z "$before" ]; then
  echo "could not read meter ${slug} updatedAt; cannot assert convergence"
  exit 1
fi

# Long enough to span several reconciles triggered by the caller's re-apply.
sleep 12

after="$(fetch_updated_at)"
if [ "$before" != "$after" ]; then
  echo "meter ${slug} is still being rewritten: updatedAt moved ${before} -> ${after}"
  echo "the meter drift check never converges — EnsureMeter is issuing an update on every reconcile"
  exit 1
fi

echo "converged: meter ${slug} stopped changing (updatedAt stable at ${before})"
