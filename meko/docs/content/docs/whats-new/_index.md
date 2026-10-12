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
    href="changelog/meko/v1.0.2/"
    title="Meko v1.0.2 - October 5, 2026"
    desc="Bulk sharing, Ask Meko in the docs, faster datapack navigation, and reliability fixes."
    cta="View release notes &rarr;"
  >}}
  {{< card
    href="changelog/installer/v2.2.0/"
    title="Installer v2.2.0 - August 31, 2026"
    desc="Kiro support, durable capture, safer memory guidance, and independent installer and skills releases."
    cta="View release notes &rarr;"
  >}}
{{< /card-group >}}

## Meko portal and platform - v1.0.2

- **[Invite a team in one step](../architecture/core-concepts/datapacks/#share-a-datapack)** - On a datapack's **Share** page, paste email addresses or upload a CSV file, choose one role, and send up to 50 invitations. Meko reports a result for each address.
- **[Ask questions from the docs](../troubleshooting/#report-an-issue)** - Open **Ask Meko** on any documentation page. Your conversation stays open while you move between pages in the same browser tab.
- **Find members and datapacks faster** - [Search a datapack's members](../architecture/core-concepts/datapacks/#share-a-datapack) by name, email address, domain, or role. In [WorkbenchLM](../guides/workbench-lm/#what-you-can-do), type to filter the datapack picker or scroll through a long list.
- **[Open a datapack in WorkbenchLM](../guides/workbench-lm/#what-you-can-do)** - On a datapack page, open the actions menu (the gear next to **Share**) and choose **Open WorkbenchLM** to start with that datapack selected.
- **Learn from production code reviews** - The [code review lab](../examples/labs/) now has four chapters, uses your datapack for cost estimates, and links to public reviews.
- **Keep conversations and memory available** - Temporary tracing failures are now retryable, oversized traces use less memory, and entity operations search only the current datapack. See how to [work with memory](../guides/working-with-memory/) and [review conversation traces](../guides/observability/).

## Installer and public skills - v2.2.0

- **Kiro IDE and CLI support** - one install configures Kiro's shared MCP and skills locations and installs durable capture hooks.
- **Capture survives delivery failures** - Claude Code, Cursor, Codex, and Kiro keep failed exchanges in a local outbox. `npx @yugabytedb/meko-mcp@latest --capture-status` shows whether capture is healthy, catching up, retrying, or needs action.
- **Codex transcripts now capture correctly** - Codex uses `codex:<repo>` ownership and holds incomplete turns for retry instead of advancing past them.
- **Safer memory guidance** - agents distinguish failed searches from empty results, keep retrieval evidence bounded, verify writes, and treat `memory_get_all` as a recent window rather than an export.
- **Independent releases** - the public skills plugin and MCP server are each now released independently from the installer and Desktop extension.

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
