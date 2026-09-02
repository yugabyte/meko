---
title: What's new
linkTitle: What's new
description: Highlights from the latest Meko releases
weight: 500
type: docs
body_class: full-width
notoc: true
---

Highlights from recent Meko releases. For the full history, see the [Changelog](../changelog/).

{{< card-group variant="half" >}}
  {{< card
    href="changelog/meko/v0.9.6/"
    title="Meko v0.9.6 — August 13, 2026"
    desc="Token Burn savings, live usage, artifacts, and knowledge-base status."
    cta="View release notes &rarr;"
  >}}
  {{< card
    href="changelog/installer/v2.2.0/"
    title="Installer v2.2.0 — August 31, 2026"
    desc="Kiro support, durable capture, safer memory guidance, and independent installer and skills releases."
    cta="View release notes &rarr;"
  >}}
{{< /card-group >}}

## Meko portal and platform — v0.9.5–v0.9.6

- **Track usage against your plan** — a dedicated Usage page shows live consumed versus plan limits for datapacks, conversations, workbench tokens, knowledge-base storage, and MCP memory and search activity.
- **Find generated files and follow indexing progress** — each datapack now has an Artifacts page, while knowledge-base files show their progress from queued through ready.
- **See what Meko saves you** — contextual prominence search is live in production. The Datapacks overview and each datapack detail page show the context reduction from unified retrieval across conversation, memory, and knowledge base, with a per-model estimate of the resulting cost savings.
- **Conversation-embedding quotas** — conversation search and embedding usage is governed by plan-tier quotas as the conversation-search rollout continues.
- **Claude Desktop recall works on plain questions** — the Meko skill now activates for simple recall prompts, and adding messages to a conversation from Claude Desktop no longer errors.
- **AMP cluster tools, opt-in** — cluster, branch, and compute management tools are available behind an early-access flag, prefixed with `amp_`.

## Installer and public skills — v2.2.0

- **Kiro IDE and CLI support** — one install configures Kiro's shared MCP and skills locations and installs durable capture hooks.
- **Capture survives delivery failures** — Claude Code, Cursor, Codex, and Kiro keep failed exchanges in a local outbox. `npx @yugabytedb/meko-mcp@latest --capture-status` shows whether capture is healthy, catching up, retrying, or needs action.
- **Codex transcripts now capture correctly** — Codex uses `codex:<repo>` ownership and holds incomplete turns for retry instead of advancing past them.
- **Safer memory guidance** — agents distinguish failed searches from empty results, keep retrieval evidence bounded, verify writes, and treat `memory_get_all` as a recent window rather than an export.
- **Independent releases** — the public skills plugin and MCP server are each now released independently from the installer and Desktop extension.

> [!NOTE] Upgrade Claude Desktop's Node.js runtime
>
> Installer 2.2.0 requires Node.js 20.18.1 or newer and pins `mcp-remote@0.1.38`. See [Installer v2.2.0](../changelog/installer/v2.2.0/) for the full changes.

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
    desc="Full release history for the installer and public agent skills."
    cta="View releases &rarr;"
  >}}
{{< /card-group >}}
