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
    href="changelog/meko/v1.0.1/"
    title="Meko v1.0.1 — September 25, 2026"
    desc="Labs, datapack Analytics, WorkbenchLM tool use, and Standard on AWS Marketplace."
    cta="View release notes &rarr;"
  >}}
  {{< card
    href="changelog/installer/v2.2.0/"
    title="Installer v2.2.0 — August 31, 2026"
    desc="Kiro support, durable capture, safer memory guidance, and independent installer and skills releases."
    cta="View release notes &rarr;"
  >}}
{{< /card-group >}}

## Meko portal and platform — v1.0.1

- **Try Meko in Labs** — open **Labs** and launch **Review a PR with AgentK**. You build datapack context, watch the review, and choose which memories to keep.
- **See more on a datapack's Analytics tab** — Analytics now show context size, a memory breakdown, and recent search activity. **Your datapacks** no longer shows an account-wide efficiency tile, or a context badge of 0 on every card.
- **Let WorkbenchLM call your datapack** — Reads run immediately, as does saving a memory. Deleting a memory, promoting a memory, or updating the datapack waits until you choose **Allow**.
- **Upgrade to Standard on AWS Marketplace** — Choose **Settings > Pricing** to upgrade a Free account to Standard using AWS Marketplace ($25/month). Finish account setup in Meko after you subscribe, or the plan stays on Free.
- **Long conversations accept new messages** — Add or delete messages after a conversation's stored history exceeds its size limit. Reading or updating that conversation can still fail while the history is over the limit.

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
