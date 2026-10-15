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
    href="changelog/meko/v1.0.3/"
    title="Meko v1.0.3 - October 9, 2026"
    desc="Projects, tier-limit upgrades, read-only viewers, and safer datapack setup."
    cta="View release notes &rarr;"
  >}}
  {{< card
    href="changelog/installer/v2.3.0/"
    title="Installer v2.3.0 - October 13, 2026"
    desc="OpenCode support, capture recovery commands, broader context search, and capture reliability fixes."
    cta="View release notes &rarr;"
  >}}
{{< /card-group >}}

## Meko portal and platform - v1.0.3

- **[Group datapacks in a project](../architecture/core-concepts/datapacks/#group-datapacks-in-a-project)** - Open **Projects**, click **New Project**, and organize datapacks in folders.
- **[Reach a tier limit](../architecture/core-concepts/datapacks/#reach-a-tier-limit)** - A banner now reports when you exceed your limits. To upgrade, on Free, choose **Upgrade to Standard**, and on Standard, choose **Talk to our team**.
- **[What a viewer can do](../architecture/core-concepts/datapacks/#what-a-viewer-can-do)** - A **Viewer** can search the datapack. Creating or changing a conversation or memory returns `read_only_access`.
- **[Create a datapack](../architecture/core-concepts/datapacks/#create-a-datapack)** - If tracing setup fails, creation stops and nothing is left behind. You can create more than one datapack at the same time, up to your plan's datapack cap.
- **[Delete a datapack](../architecture/core-concepts/datapacks/#delete-a-datapack)** - You can now delete a datapack even when tracing is unavailable.
- **[Agentic coding review](../examples/labs/#agentic-coding-review-adlc)** - When the review comes back as malformed JSON, the lab shows the review text. It does not show **No blocking issues found**.

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
