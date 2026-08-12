---
title: What's new
linkTitle: What's new
description: Highlights from the latest Meko releases
weight: 500
type: docs
body_class: full-width
notoc: true
---

Highlights from the most recent release on each of Meko's two tracks. For the full history, see the [Changelog](../changelog/).

{{< card-group variant="half" >}}
  {{< card
    href="changelog/meko/v0.9.5/"
    title="Meko v0.9.5 — July 31, 2026"
    desc="Token Burn savings, conversation-embedding quotas, and MCP server 2.1.0."
    cta="View release notes &rarr;"
  >}}
  {{< card
    href="changelog/installer/v2.1.0/"
    title="Installer v2.1.0 — July 28, 2026"
    desc="Reliable installs, a 23-tool MCP surface, corrected cross-agent reads, and a published skills plugin."
    cta="View release notes &rarr;"
  >}}
{{< /card-group >}}

## Meko portal and platform — v0.9.5

- **See what Meko saves you** — contextual prominence search is live in production. The Datapacks overview and each datapack detail page show the context reduction from unified retrieval across conversation, memory, and knowledge base, with a per-model estimate of the resulting cost savings.
- **Conversation-embedding quotas** — conversation search and embedding usage is governed by plan-tier quotas as the conversation-search rollout continues.
- **Claude Desktop recall works on plain questions** — the Meko skill now activates for simple recall prompts, and adding messages to a conversation from Claude Desktop no longer errors.
- **AMP cluster tools, opt-in** — cluster, branch, and compute management tools are available behind an early-access flag, prefixed with `amp_`.

## Installer, MCP server, and skills — v2.1.0

- **Cross-agent reads work as expected** — `memory_search`, `memory_get_all`, and `conversation_list` span all of your agent namespaces within a datapack, so agents no longer fan out one search per agent.
- **Production is the default for fresh installs** — with no existing Meko configuration and no `--url`, the installer points at `https://mcp.mekodata.ai/mcp`.
- **A tighter 23-tool surface** — experimental conversation-search tools stay hidden unless you enable them, and the remaining tool contracts are more precise.
- **A self-contained skills plugin** — behavioral skills, hooks, and connector metadata ship as a complete Claude Code plugin from [`yugabyte/meko-skills`](https://github.com/yugabyte/meko-skills).
- **Memory promotion asks first** — promoting to team memory previews the exact memories, explains visibility, and requires explicit confirmation.
- **Capture failures stay visible** — quota and HTTP `402`/`403` failures no longer advance the capture watermark, so the next session reports the problem instead of silently skipping turns.

> [!NOTE] Breaking change
>
> Tool-level `scope` is retired — stop sending `scope="read"`, `"write"`, or `"admin"` on MCP tool calls. Authorization comes from your authenticated identity and datapack grants instead. A temporary shim strips the argument from older clients; see [Installer v2.1.0](../changelog/installer/v2.1.0/) for the upgrade path.

## Keep up

{{< card-group variant="half" >}}
  {{< card
    href="changelog/meko/"
    title="Meko release notes"
    desc="Full release history for the Meko portal, API, and platform."
    cta="View releases &rarr;"
  >}}
  {{< card
    href="changelog/installer/"
    title="Installer release notes"
    desc="Full release history for the Installer, MCP server, and agent skills."
    cta="View releases &rarr;"
  >}}
{{< /card-group >}}
