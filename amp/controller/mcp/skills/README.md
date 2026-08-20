# amp MCP Agent Skills

Agent Skills for the amp controller MCP server — behavioral guides that teach an
AI harness (Claude Code / Claude Desktop / Cursor) how to drive amp's MCP tools
correctly. This mirrors how `meko-mcp-server` ships skills alongside its tools.

The skill **content** is the `SKILL.md` under each skill directory here; the
files below only handle **delivery** (getting a harness to auto-discover it).

```
controller/mcp/skills/
  amp-mcp-tools/SKILL.md          # the skill (all 21 tools, async model, gotchas)
  .claude-plugin-desktop/plugin.json   # Claude Desktop bundle manifest
  scripts/package-skill.sh        # build a .skill bundle for Desktop
  README.md                       # this file
```

## How each harness loads it

### Claude Code — in this repo (zero install)
`.claude/skills/amp-mcp-tools` (repo root) symlinks to this skill, so Claude
Code auto-discovers it for anyone working in the yb-amp repo. Nothing to do.

### Claude Code — as an installable plugin (anywhere)
The repo-root `.claude-plugin/marketplace.json` publishes this as a plugin.
Install it from any cluster:

```
/plugin marketplace add yugabyte/amp
/plugin install amp-agent-skills
```

### Claude Desktop — importable bundle
Build a `.skill` bundle and import it via Desktop → Settings → Skills:

```bash
bash controller/mcp/skills/scripts/package-skill.sh
# → controller/mcp/skills/amp-mcp-tools.skill  (git-ignored)
```

### Cursor / other harnesses
No skill auto-discovery yet. Either paste `amp-mcp-tools/SKILL.md` into the
cluster's rules/pinned-context file, or rely on the amp MCP server's in-band
`instructions` (`controller/mcp/src/server_instructions.py`), which every MCP
client receives automatically.

## What's intentionally NOT here yet

Automated **publish** of the plugin/bundle to a marketplace on every change
(meko-mcp-server does this via a `publish-skills` workflow). It needs org infra
— a GitHub App + secrets + a target marketplace — so it's deferred until that's
provisioned. Until then, the skill is loadable via the three paths above.
