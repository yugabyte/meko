#!/usr/bin/env python3
"""Fail when the public MCP docs drift from the documented tool surface.

The live MCP tools live in yugabyte/meko-mcp-server; this check only guards the
public Hugo docs in this repo. It asserts:

1. Every tool row in reference/mcp-server.md is counted, and the "N tools"
   claim matches that count (HTML comments are ignored, so commented-out
   db_* tools do not count).
2. Required companion guides for documented tool prefixes exist and are linked
   from guides/_index.md and static/llms.txt.

When a new tool prefix ships in the reference without a guide (the MEKO-298
gap for artifact_*), this script fails the PR.
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

DOCS_ROOT = Path(__file__).resolve().parents[1]
REF = DOCS_ROOT / "content" / "docs" / "reference" / "mcp-server.md"
GUIDES_INDEX = DOCS_ROOT / "content" / "docs" / "guides" / "_index.md"
LLMS = DOCS_ROOT / "static" / "llms.txt"

# Tool-name prefixes that must have a how-to guide when any matching tool is
# listed in the reference. Update this when adding a new public tool family.
REQUIRED_GUIDES = (
    {
        "prefix": "memory_",
        "path": DOCS_ROOT / "content" / "docs" / "guides" / "working-with-memory.md",
        "href": "guides/working-with-memory/",
        "llms": "/guides/working-with-memory/",
    },
    {
        "prefix": "knowledgebase_",
        "path": DOCS_ROOT / "content" / "docs" / "guides" / "working-with-knowledge-bases.md",
        "href": "guides/working-with-knowledge-bases/",
        "llms": "/guides/working-with-knowledge-bases/",
    },
    {
        "prefix": "artifact_",
        "path": DOCS_ROOT / "content" / "docs" / "guides" / "working-with-artifacts.md",
        "href": "guides/working-with-artifacts/",
        "llms": "/guides/working-with-artifacts/",
    },
)

TOOL_ROW = re.compile(r"^\|\s*`([a-z][a-z0-9_]*)`\s*\|")
COUNT_CLAIM = re.compile(r"provides\s+(\d+)\s+tools\s+by\s+default", re.I)
HTML_COMMENT = re.compile(r"<!--.*?-->", re.S)


def strip_html_comments(text: str) -> str:
    return HTML_COMMENT.sub("", text)


def documented_tools(ref_text: str) -> list[str]:
    tools: list[str] = []
    for line in strip_html_comments(ref_text).splitlines():
        m = TOOL_ROW.match(line)
        if m:
            tools.append(m.group(1))
    return tools


def main() -> int:
    errors: list[str] = []

    ref = REF.read_text(encoding="utf-8")
    tools = documented_tools(ref)
    if not tools:
        errors.append(f"no tool rows found in {REF.relative_to(DOCS_ROOT)}")

    claim = COUNT_CLAIM.search(strip_html_comments(ref))
    if not claim:
        errors.append('missing "provides N tools by default" claim in mcp-server.md')
    elif int(claim.group(1)) != len(tools):
        errors.append(
            f"tool count claim is {claim.group(1)} but reference lists {len(tools)} "
            f"tools: {', '.join(tools)}"
        )

    guides_index = GUIDES_INDEX.read_text(encoding="utf-8")
    llms = LLMS.read_text(encoding="utf-8")

    for guide in REQUIRED_GUIDES:
        matching = [t for t in tools if t.startswith(guide["prefix"])]
        if not matching:
            continue
        path: Path = guide["path"]
        if not path.is_file():
            errors.append(
                f"tools {matching} are documented but missing guide {path.relative_to(DOCS_ROOT)}"
            )
            continue
        if guide["href"] not in guides_index:
            errors.append(f"guides/_index.md must link {guide['href']} (required by {matching})")
        if guide["llms"] not in llms:
            errors.append(f"static/llms.txt must link {guide['llms']} (required by {matching})")

    if errors:
        print("MCP docs check failed:", file=sys.stderr)
        for e in errors:
            print(f"  - {e}", file=sys.stderr)
        return 1

    print(f"MCP docs OK: {len(tools)} tools, required guides present")
    return 0


if __name__ == "__main__":
    sys.exit(main())
