#!/usr/bin/env bash
# Package the amp MCP Agent Skill(s) as importable .skill bundles (zip archives)
# for Claude Desktop. Mirrors meko-mcp-server's packaging step.
#
# Usage: bash controller/mcp/skills/scripts/package-skill.sh
# Output: <skills-root>/amp-mcp-tools.skill
#
# No secrets, no network — this only validates and zips. Publishing the bundle
# to a marketplace is a separate, infra-gated step (see skills/README.md).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SKILLS_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SKILL_NAMES=("amp-mcp-tools")
XML_TAG_PATTERN='</?[[:alpha:]_:][[:alnum:]_.:-]*([[:blank:]][^<>]*)?[[:blank:]]*/?>'

# --- validate each skill has a non-empty, tag-free description ---
for SKILL_NAME in "${SKILL_NAMES[@]}"; do
    SKILL_DIR="$SKILLS_ROOT/$SKILL_NAME"
    [ -d "$SKILL_DIR" ] || { echo "Error: skill dir not found: $SKILL_DIR" >&2; exit 1; }
    [ -f "$SKILL_DIR/SKILL.md" ] || { echo "Error: SKILL.md not found in $SKILL_DIR" >&2; exit 1; }
    if ! grep -q '^description:[[:space:]]*' "$SKILL_DIR/SKILL.md"; then
        echo "Error: SKILL.md is missing a 'description:' field: $SKILL_NAME" >&2
        exit 1
    fi
    DESCRIPTION="$(awk '
        /^description:[[:space:]]*/ { sub(/^description:[[:space:]]*/, ""); print; exit }
    ' "$SKILL_DIR/SKILL.md")"
    if [[ -z "${DESCRIPTION//[[:space:]]/}" ]]; then
        echo "Error: SKILL.md description is empty: $SKILL_NAME" >&2
        exit 1
    fi
    if printf '%s\n' "$DESCRIPTION" | grep -Eq "$XML_TAG_PATTERN"; then
        echo "Error: SKILL.md description cannot contain XML-like tags: $SKILL_NAME" >&2
        exit 1
    fi
done

# --- zip each skill (SKILL.md + references/ when present) into a .skill file ---
for SKILL_NAME in "${SKILL_NAMES[@]}"; do
    OUTPUT="$SKILLS_ROOT/$SKILL_NAME.skill"
    rm -f "$OUTPUT"
    cd "$SKILLS_ROOT"
    INCLUDE=("$SKILL_NAME/SKILL.md")
    [ -d "$SKILL_NAME/references" ] && INCLUDE+=("$SKILL_NAME/references/")
    zip -r "$OUTPUT" "${INCLUDE[@]}" -x "*.DS_Store" "*__pycache__*"
    echo "Created $OUTPUT ($(du -h "$OUTPUT" | cut -f1))"
done
