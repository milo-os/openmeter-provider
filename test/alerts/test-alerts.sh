#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# Find promtool in PATH or local bin/
if command -v promtool >/dev/null 2>&1; then
  PROMTOOL="promtool"
elif [[ -x "$root/bin/promtool" ]]; then
  PROMTOOL="$root/bin/promtool"
else
  echo "promtool not found; installing into $root/bin..."
  "$root/hack/install-promtool.sh" "$root/bin"
  PROMTOOL="$root/bin/promtool"
fi

if ! command -v yq >/dev/null 2>&1; then
  echo "❌ yq is required to extract rule specs" >&2
  exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

yq '.spec' "$root/config/components/prometheus/alerts.yaml" > "$work/alerts.rules.yaml"
cp "$root/test/alerts/alerts.test.yaml" "$work/alerts.test.yaml"

cd "$work"

echo "Checking Prometheus alert rules syntax..."
"$PROMTOOL" check rules alerts.rules.yaml

echo "Running alert rule unit tests..."
"$PROMTOOL" test rules alerts.test.yaml

echo "✅ All alert tests passed!"
