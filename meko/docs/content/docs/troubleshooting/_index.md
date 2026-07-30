---
title: Troubleshooting
linkTitle: Troubleshooting
description: Report issues, share feedback, and get help with Meko
weight: 6000
type: docs
---

## Common problems installing and connecting

If you have problems with your setup, check the following common issues:

- API key.

    Ensure you pasted your API key correctly (no whitespace). Ensure the key is not revoked or expired.

- Node version.

    Ensure you have the correct version of Node (Node.js 18.0 or newer) installed.

- Claude Desktop.
    - Restart is required after editing your configuration.
    - `--allow-http` flag is required in [manual setup](../../integrations/connect-to-ai-agent/#manual-setup-1).

- Agent answers but never calls Meko tools.

    The Meko skills are not installed; see **Connect>Manual MCP Install** in the portal.

    Note that if you installed Meko using OAuth alone to authenticate (that is, you didn't use the automatic installer), your agent interacts with Meko _on demand_ only: you need to mention "Meko" in your prompt (for example, _"save this to Meko,"_ _"search my Meko knowledge base"_) for the agent to call the tools. The agent won't proactively save conversations or capture memories unless you ask. To get automatic behavior, use the [Automated install](../quick-start/#connect-your-ai-client), or write your own skills to teach your agent when to call Meko tools.

- Claude Desktop recalls memories but doesn't save new ones automatically.

    Claude Desktop has no session hooks, so automatic capture relies on the model deciding to call `conversation_add_message` on its own. This is model-dependent: some models capture proactively from the installed skill alone, but Sonnet- and Opus-class models typically need an extra, always-on nudge to do it for every turn.

    Add an instruction along the lines of "save our conversation to Meko periodically" to **Claude > Settings > Profile > Instructions for Claude** (kept in mind across chats). This is the one channel on Desktop that's always injected with first-party authority, so it reliably closes the gap.

    You can always work around this by explicitly asking your agent to save ("save this conversation to Meko") or remember ("remember this") — those requests are never best-effort.

- Cursor or Codex misses conversations during automatic capture.

    In v2.1.0, Cursor's ordinary archive and new-chat workflows don't reliably emit the lifecycle events the capture hooks expect. Codex hooks run, but the capture worker doesn't yet parse Codex `response_item` transcript records. Explicitly ask Meko to save important conversations or remember critical facts; don't rely on complete automatic capture in these clients.

- Claude Code still uses old Meko skill behavior after an upgrade.

    Re-run the installer with an explicit current version or the `@latest` tag so `npx` doesn't reuse a stale cached package:

    ```sh
    npx @yugabytedb/meko-mcp@latest
    ```

    To refresh the skills inside an active Claude Code session without restarting, run:

    ```text
    /plugin marketplace update meko-agent-skills
    /reload-plugins
    /doctor
    ```

    The installer now uses the public HTTPS marketplace at `https://github.com/yugabyte/meko-skills.git`; GitHub SSH credentials aren't required.

- Tool calls still send a `scope` argument after upgrading to v2.1.0.

    Tool-level `scope` (`"read"`, `"write"`, `"admin"`) was retired. Stop sending it in custom integrations, and rerun the installer or refresh the skills plugin so hooks and skills stop sending it too. A temporary compatibility shim currently strips the argument from older clients, but that shim will be removed in a future release. This is unrelated to installer flags such as `--scope user|project`.

- SessionStart reports a persistent capture failure.

    Quota failures and HTTP `402`/`403` responses deliberately keep the capture watermark in place so no transcript range is silently marked as saved. Resolve the quota, billing, permission, or authentication problem and retry; a successful capture clears the warning.

## Report an issue

The fastest way to get help and report bugs is the Meko Discord. Join the community, post in the relevant channel, and the team or other community members will respond.

[Join the Meko Discord &rarr;](https://discord.gg/GWcSST7GqZ)

When reporting an issue, include:

- What you were trying to do
- What you expected to happen
- What actually happened
- Any error messages from your agent or the Meko portal
- The name of your datapack (not the API key) if the issue is datapack-specific

## Share feedback

While you're signed into the [Meko portal](https://cloud.mekodata.ai/), use the in-portal feedback control to send detailed feedback. The portal automatically attaches diagnostic information about your session, which makes it easier for the team to reproduce and triage what you're seeing.

The portal feedback path is the right one for:

- Feature requests
- Usability or UX issues
- Suggestions for documentation or examples
- Anything you want to share that isn't a blocking bug
