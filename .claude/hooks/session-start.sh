#!/usr/bin/env bash
# .claude/hooks/session-start.sh
#
# SessionStart hook: prompts the user to run /init-service on first use.
#
# On fresh clones (before rename.sh has been run), .claude/.initialized does
# not exist and this hook injects /init-service as the opening message so
# Claude guides the user through product discovery and rename automatically.
#
# After initialization, .claude/.initialized is created and this hook exits
# silently so subsequent sessions are unaffected.
#
# Make executable: chmod +x .claude/hooks/session-start.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
MARKER="$REPO_ROOT/.claude/.initialized"

echo "[init-hook] ran at $(date), initialized=$([ -f "$MARKER" ] && echo yes || echo no)" >> /tmp/claude-init-hook.log

if [[ ! -f "$MARKER" ]]; then
  printf '{"hookSpecificOutput": {"hookEventName": "UserPromptSubmit", "additionalContext": "CRITICAL: This repository has not been initialized. The .claude/.initialized marker does not exist. Before responding to the user message below, you MUST immediately invoke the init-service skill by calling Skill({skill: \"init-service\"}). Do not answer the user message first. Run the skill now."}}\n'
fi
