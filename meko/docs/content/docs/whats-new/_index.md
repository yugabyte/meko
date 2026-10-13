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
    href="changelog/installer/v2.3.0/"
    title="Installer v2.3.0 - October 13, 2026"
    desc="OpenCode support, capture recovery commands, broader context search, and capture reliability fixes."
    cta="View release notes &rarr;"
  >}}
{{< /card-group >}}

## Meko portal and platform - v1.0.2

- **[Invite a team](../architecture/core-concepts/datapacks/#share-a-datapack)** - On a datapack's **Share** page, paste email addresses or upload a CSV file and choose one role to send up to 50 invitations at a time. Meko reports a result for each address.
- **Ask questions from the docs** - Open **Ask Meko** on any documentation page. Your conversation stays open while you move between pages in the same browser tab.
- **Find members and datapacks faster** - [Search datapack members](../architecture/core-concepts/datapacks/#share-a-datapack) by name, email address, domain, or role. In [WorkbenchLM](../guides/workbench-lm/), filter the datapack list.
- **[Open a datapack in WorkbenchLM](../guides/workbench-lm/#what-you-can-do)** - On a datapack page, open the actions menu (the gear next to **Share**) and choose **Open WorkbenchLM** to start with that datapack selected.
- **Learn from production code reviews** - The [code review lab](../examples/labs/) now has four chapters, uses your datapack for cost estimates, and links to public reviews.
- **Keep conversations and memory available** - Temporary tracing failures are now retryable, oversized traces use less memory, and entity operations search only the current datapack. See how to [work with memory](../guides/working-with-memory/) and [review conversation traces](../guides/observability/).

## Installer and public skills - v2.3.0

- **Connect OpenCode 1.18.x** - one install configures the MCP server and Meko skill, migrates local sessions, and captures new conversations. Restart OpenCode after installation.
- **Recover stuck capture** - inspect, stop, drain, resume, repair, or discard local capture work with `--capture-cleanup`. `stop` pauses retries and keeps unsent exchanges.
- **Search all Meko context** - agents use `context_search` for open-ended recall across personal memory, shared knowledge, and past conversations.
- **Keep project memory together** - capture and migration derive the agent ID from the repository root, so starting in `docs` or `src` no longer splits or combines unrelated project memories. A linked git worktree uses the worktree directory name.
- **See the real capture failure** - status now distinguishes quota, authentication, missing datapack, and datapack access problems. One edge-proxy rejection no longer blocks later exchanges.

> [!NOTE] OpenCode support
>
> OpenCode support covers CLI and TUI 1.18.x. It doesn't cover OpenCode Desktop, headless or remote sessions, or tool and file capture. See [Installer v2.3.0](../changelog/installer/v2.3.0/) for the full changes.

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
