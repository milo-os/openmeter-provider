#!/usr/bin/env bash
# hack/rename.sh — rename controller-template placeholders after forking.
#
# Run once after forking:
#   chmod +x hack/rename.sh
#   ./hack/rename.sh --service-name billing --api-group billing.miloapis.com --kind BillingAccount
#
# Flags:
#   --service-name  kebab-case service name  (e.g. billing)
#   --api-group     API group                (e.g. billing.miloapis.com)
#   --kind          CamelCase CRD kind       (e.g. BillingAccount)
#   --dry-run       print planned changes without writing anything

set -euo pipefail

# ── argument parsing ──────────────────────────────────────────────────────────

SERVICE_NAME=""
API_GROUP=""
KIND=""
DRY_RUN=false

usage() {
  echo "Usage: $0 --service-name <name> --api-group <group> --kind <Kind> [--dry-run]"
  echo ""
  echo "  --service-name  kebab-case service name   e.g. billing"
  echo "  --api-group     API group FQDN             e.g. billing.miloapis.com"
  echo "  --kind          CamelCase CRD kind         e.g. BillingAccount"
  echo "  --dry-run       print changes without writing"
  exit 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --service-name) SERVICE_NAME="$2"; shift 2 ;;
    --api-group)    API_GROUP="$2";    shift 2 ;;
    --kind)         KIND="$2";         shift 2 ;;
    --dry-run)      DRY_RUN=true;      shift   ;;
    *) echo "Unknown argument: $1"; usage ;;
  esac
done

[[ -z "$SERVICE_NAME" ]] && { echo "ERROR: --service-name is required"; usage; }
[[ -z "$API_GROUP"    ]] && { echo "ERROR: --api-group is required";    usage; }
[[ -z "$KIND"         ]] && { echo "ERROR: --kind is required";         usage; }

# ── derived values ────────────────────────────────────────────────────────────

KIND_LOWER="$(echo "$KIND" | tr '[:upper:]' '[:lower:]')"
# e.g. "billing" -> "BILLING_"  /  "my-service" -> "MY_SERVICE_"
SERVICE_UPPER="$(echo "$SERVICE_NAME" | tr '[:lower:]-' '[:upper:]_')"

OLD_SERVICE="controller-template"
OLD_API_GROUP="example.miloapis.com"
OLD_KIND="Resource"
OLD_KIND_LOWER="resource"
OLD_OPERATOR="ControllerTemplateOperator"
OLD_ENV_PREFIX="CONTROLLER_TEMPLATE_API_"
OLD_MODULE="go.miloapis.com/controller-template"

NEW_OPERATOR="${KIND}Operator"
NEW_ENV_PREFIX="${SERVICE_UPPER}_API_"
NEW_MODULE="go.miloapis.com/${SERVICE_NAME}"

# ── repo root detection ───────────────────────────────────────────────────────

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

echo "Repo root: $REPO_ROOT"
echo ""
echo "Replacements planned:"
printf "  %-45s -> %s\n" "$OLD_SERVICE"     "$SERVICE_NAME"
printf "  %-45s -> %s\n" "$OLD_API_GROUP"   "$API_GROUP"
printf "  %-45s -> %s\n" "$OLD_KIND"        "$KIND"
printf "  %-45s -> %s\n" "$OLD_KIND_LOWER"  "$KIND_LOWER"
printf "  %-45s -> %s\n" "$OLD_OPERATOR"    "$NEW_OPERATOR"
printf "  %-45s -> %s\n" "$OLD_ENV_PREFIX"  "$NEW_ENV_PREFIX"
printf "  %-45s -> %s\n" "$OLD_MODULE"      "$NEW_MODULE"
echo ""

# ── pre-flight validation ─────────────────────────────────────────────────────

echo "Validating placeholder strings exist..."

missing=()
grep -qrl "$OLD_SERVICE"   --include="*.go" --include="*.yaml" --include="*.ts" --include="*.tsx" --include="*.mod" --include="*.json" --include="*.txt" . 2>/dev/null || missing+=("$OLD_SERVICE")
grep -qrl "$OLD_API_GROUP" --include="*.go" --include="*.yaml" --include="*.ts" --include="*.tsx" --include="*.mod" --include="*.json" --include="*.txt" . 2>/dev/null || missing+=("$OLD_API_GROUP")
grep -qrl "$OLD_KIND"      --include="*.go" --include="*.yaml" --include="*.ts" --include="*.tsx" --include="*.mod" --include="*.json" --include="*.txt" . 2>/dev/null || missing+=("$OLD_KIND")

if [[ ${#missing[@]} -gt 0 ]]; then
  echo "ERROR: The following placeholder strings were not found in any tracked text file:"
  for m in "${missing[@]}"; do
    echo "  - $m"
  done
  echo ""
  echo "This script is meant to run once immediately after forking."
  echo "If you have already renamed some placeholders, run the remaining replacements manually."
  exit 1
fi

echo "All placeholders found. Proceeding."
echo ""

# ── helpers ───────────────────────────────────────────────────────────────────

# Returns 0 if a file is binary (non-text).
is_binary() {
  local f="$1"
  # Use `file` if available; fall back to a simple null-byte check.
  if command -v file &>/dev/null; then
    file --brief --mime-encoding "$f" 2>/dev/null | grep -q "binary" && return 0 || return 1
  else
    grep -qP '\x00' "$f" 2>/dev/null && return 0 || return 1
  fi
}

# Portable in-place sed: uses perl to avoid BSD/GNU sed differences.
inplace_replace() {
  local old="$1"
  local new="$2"
  local file="$3"
  if $DRY_RUN; then
    if grep -qF "$old" "$file" 2>/dev/null; then
      echo "  [DRY-RUN] $file: s|$old|$new|g"
    fi
  else
    perl -pi -e "s|\Q$old\E|$new|g" "$file"
  fi
}

# Collect text files (skip .git, node_modules, vendor, binary files).
collect_text_files() {
  find . \
    -not \( -path './.git' -prune \) \
    -not \( -path './node_modules' -prune \) \
    -not \( -path './vendor' -prune \) \
    -not \( -path './ui/node_modules' -prune \) \
    -type f \
    | while read -r f; do
        is_binary "$f" || echo "$f"
      done
}

# ── content replacements ──────────────────────────────────────────────────────

echo "=== Step 1: Replacing content in text files ==="
echo ""

TEXT_FILES="$(collect_text_files)"

while IFS= read -r file; do
  # Order matters: longer/more-specific strings first to avoid partial matches.
  inplace_replace "$OLD_MODULE"      "$NEW_MODULE"      "$file"
  inplace_replace "$OLD_OPERATOR"    "$NEW_OPERATOR"    "$file"
  inplace_replace "$OLD_ENV_PREFIX"  "$NEW_ENV_PREFIX"  "$file"
  inplace_replace "$OLD_API_GROUP"   "$API_GROUP"       "$file"
  # Replace the CamelCase kind before lowercase to avoid double-lowering.
  inplace_replace "$OLD_KIND"        "$KIND"            "$file"
  inplace_replace "$OLD_KIND_LOWER"  "$KIND_LOWER"      "$file"
  # Service name last so it doesn't clobber partial matches in module path above.
  inplace_replace "$OLD_SERVICE"     "$SERVICE_NAME"    "$file"
done <<< "$TEXT_FILES"

echo "Content replacement done."
echo ""

# ── file/directory renames ────────────────────────────────────────────────────

echo "=== Step 2: Renaming files and directories ==="
echo ""

# Rename files whose basenames contain placeholder strings.
# Process deepest paths first so parent renames don't break child paths.
find . \
  -not \( -path './.git' -prune \) \
  -not \( -path './node_modules' -prune \) \
  -not \( -path './vendor' -prune \) \
  -not \( -path './ui/node_modules' -prune \) \
  \( -name "*${OLD_KIND_LOWER}*" -o -name "*${OLD_SERVICE}*" \) \
  | sort -r \
  | while read -r oldpath; do
      dir="$(dirname "$oldpath")"
      base="$(basename "$oldpath")"
      newbase="${base//$OLD_KIND_LOWER/$KIND_LOWER}"
      newbase="${newbase//$OLD_SERVICE/$SERVICE_NAME}"
      if [[ "$base" != "$newbase" ]]; then
        newpath="$dir/$newbase"
        if $DRY_RUN; then
          echo "  [DRY-RUN] rename: $oldpath -> $newpath"
        else
          echo "  rename: $oldpath -> $newpath"
          mv "$oldpath" "$newpath"
        fi
      fi
    done

# Rename the CRD yaml file that encodes the API group in its name.
OLD_CRD_YAML="config/base/crd/bases/${OLD_API_GROUP}_${OLD_KIND_LOWER}s.yaml"
NEW_CRD_YAML="config/base/crd/bases/${API_GROUP}_${KIND_LOWER}s.yaml"
if [[ -f "$OLD_CRD_YAML" ]]; then
  if $DRY_RUN; then
    echo "  [DRY-RUN] rename: $OLD_CRD_YAML -> $NEW_CRD_YAML"
  else
    echo "  rename: $OLD_CRD_YAML -> $NEW_CRD_YAML"
    mv "$OLD_CRD_YAML" "$NEW_CRD_YAML"
  fi
fi

# Rename the sample yaml that encodes the API group.
OLD_SAMPLE="config/samples/${OLD_API_GROUP//./_}_v1alpha1_${OLD_KIND_LOWER}.yaml"
NEW_SAMPLE="config/samples/${API_GROUP//./_}_v1alpha1_${KIND_LOWER}.yaml"
if [[ -f "$OLD_SAMPLE" ]]; then
  if $DRY_RUN; then
    echo "  [DRY-RUN] rename: $OLD_SAMPLE -> $NEW_SAMPLE"
  else
    echo "  rename: $OLD_SAMPLE -> $NEW_SAMPLE"
    mv "$OLD_SAMPLE" "$NEW_SAMPLE"
  fi
fi

echo "File/directory renaming done."
echo ""

# ── post-run validation ───────────────────────────────────────────────────────

if $DRY_RUN; then
  echo "Dry-run complete. No files were modified."
  exit 0
fi

echo "=== Step 3: Post-rename validation ==="
echo ""

LEFTOVERS=()

check_leftover() {
  local pattern="$1"
  local label="$2"
  # Search only text-friendly extensions to avoid false positives in go.sum etc.
  if grep -rl "$pattern" \
       --include="*.go" \
       --include="*.yaml" \
       --include="*.yml" \
       --include="*.ts" \
       --include="*.tsx" \
       --include="*.json" \
       --include="*.mod" \
       --include="*.txt" \
       --include="*.md" \
       . 2>/dev/null \
     | grep -qv '.git/'; then
    LEFTOVERS+=("$label ($pattern)")
  fi
}

check_leftover "$OLD_SERVICE"     "service name"
check_leftover "$OLD_API_GROUP"   "API group"
check_leftover "$OLD_OPERATOR"    "operator kind"
check_leftover "$OLD_ENV_PREFIX"  "env prefix"
check_leftover "$OLD_MODULE"      "Go module path"

if [[ ${#LEFTOVERS[@]} -gt 0 ]]; then
  echo "WARNING: Some placeholder strings were not fully replaced:"
  for l in "${LEFTOVERS[@]}"; do
    echo "  - $l"
  done
  echo ""
  echo "Run the following to find remaining occurrences:"
  echo '  grep -r "controller-template\|example\.miloapis\.com\|ControllerTemplateOperator" \'
  echo '    --include="*.go" --include="*.yaml" --include="*.ts" --include="*.tsx" .'
  echo ""
  echo "These may be in auto-generated files (zz_generated.*) that will be"
  echo "overwritten by 'task generate && task manifests'."
else
  echo "No placeholder strings remain. Rename successful."
fi

echo ""
echo "Next steps:"
echo "  1. Review the diff: git diff"
echo "  2. Regenerate code and manifests: task generate && task manifests"
echo "  3. Build and test: task build && task test"
echo "  4. Commit: git add -A && git commit -m 'rename: controller-template -> ${SERVICE_NAME}'"
