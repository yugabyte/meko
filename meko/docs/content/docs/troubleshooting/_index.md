---
title: Troubleshooting
linkTitle: Troubleshooting
description: Report issues, share feedback, and get help with Meko
weight: 6000
type: docs
toc_end_level: 2
---

For questions the documentation can answer, click **Ask Meko** in the header of any page. Your conversation stays open while you move between pages in the same browser tab.

## Common problems installing and connecting

If you have problems with your setup, check the following common issues:

#### API key

Ensure you pasted your API key correctly (no whitespace). Ensure the key is not revoked or expired.

#### Node version

Ensure you have Node.js 20.18.1 or newer installed. Installer 2.2.0 and newer check this before changing client configuration.

#### Claude Desktop

Restart Claude Desktop after editing your configuration.

Use the pinned `mcp-remote@0.1.38` bridge from the [manual setup](../../integrations/connect-to-ai-agent/#manual-setup-1). Older bridge versions can fail with “Server disconnected.”

#### Agent answers but never calls Meko tools

The Meko skills are not installed; see **Connect>Manual MCP Install** in the portal.

Note that if you installed Meko using OAuth alone to authenticate (that is, you didn't use the automatic installer), your agent interacts with Meko _on demand_ only: you need to mention "Meko" in your prompt (for example, _"save this to Meko,"_ _"search my Meko knowledge base"_) for the agent to call the tools. The agent won't proactively save conversations or capture memories unless you ask. To get automatic behavior, use the [Automated install](../quick-start/#connect-your-ai-client), or write your own skills to teach your agent when to call Meko tools.

#### Claude Desktop recalls memories but doesn't save new ones automatically

Claude Desktop has no session hooks, so automatic capture relies on the model deciding to call `conversation_add_message` on its own. This is model-dependent: some models capture proactively from the installed skill alone, but Sonnet- and Opus-class models typically need an extra, always-on nudge to do it for every turn.

Add an instruction along the lines of "save our conversation to Meko periodically" to **Claude > Settings > Profile > Instructions for Claude** (kept in mind across chats). This is the one channel on Desktop that's always injected with first-party authority, so it reliably closes the gap.

You can always work around this by explicitly asking your agent to save ("save this conversation to Meko") or remember ("remember this") — those requests are never best-effort.

#### Automatic capture is behind or reports a failure

Installer 2.2.0 and newer keep failed exchanges in a local outbox and retry them without advancing the capture cursor. Check the current state:

```bash
npx @yugabytedb/meko-mcp@latest --capture-status
```

Add `--json` for machine-readable output. A healthy or catching-up state exits `0`; retrying exits `2`; a problem that needs action exits `3`. Resolve the reported authentication, quota, permission, or server problem, then start another session so capture can retry.

#### Capture keeps retrying work that you need to stop or recover

Installer 2.3.0 can inspect and clean up local capture state. First, see the affected sessions and running capture processes:

```bash
npx @yugabytedb/meko-mcp@latest --capture-cleanup status
```

To stop retries from this machine without deleting unsent exchanges:

```bash
npx @yugabytedb/meko-mcp@latest --capture-cleanup stop
```

`stop` pauses retries for every local session that still has unsent work. It also works with capture hooks installed by version 2.2.0. After you fix the server, authentication, quota, or datapack problem, choose one recovery path.

Send the unsent exchanges now with `drain --all`. Claude Code already has the connection values. For every other client, export them first:

```bash
export MEKO_MCP_URL=https://mcp.mekodata.ai/mcp
export MEKO_API_KEY=<paste-your-api-key>
npx @yugabytedb/meko-mcp@latest --capture-cleanup drain --all
```

Return the backlog to normal hook retries with `npx @yugabytedb/meko-mcp@latest --capture-cleanup resume`.

Use `repair` when status reports a failure for a session that has no work left to send. Use `discard --session <id> --yes` only when you intend to delete that session's unsent exchanges. Every action except `status` accepts `--dry-run`.

#### OpenCode doesn't show Meko or capture conversations after installation

Installer 2.3.0 supports OpenCode CLI and TUI 1.18.x. Restart OpenCode so it loads the installer-owned plugin, then check `npx @yugabytedb/meko-mcp@latest --capture-status`. Installer 2.3.0 doesn't support OpenCode Desktop, V2 preview APIs, or headless or remote sessions.

#### A Claude Code prompt with a pasted image isn't stored

Claude Code writes the image caption as its own user entry. Capture can skip the prompt because that turn has no assistant reply. Ask the agent to remember the point you need, or send the prompt again without the image.

#### Kiro doesn't capture a conversation

Kiro IDE runs workspace hooks only after you trust the workspace. Kiro CLI 2.x must also use the installer-managed `meko-default` agent. Rerun the installer with `--client kiro`, trust the workspace, and check `npx @yugabytedb/meko-mcp@latest --capture-status`.

Kiro hooks include prompts and final responses, but Kiro doesn't expose tool calls or hidden reasoning through these events.

#### Claude Code still uses old Meko skill behavior after an upgrade

Re-run the installer with an explicit current version or the `@latest` tag so `npx` doesn't reuse a stale cached package:

```sh
npx @yugabytedb/meko-mcp@latest
```

To refresh the skills inside an active Claude Code session without restarting, run:

```text
/plugin marketplace update meko-agent-skills
/plugin update meko-agent-skills@meko-agent-skills
/reload-plugins
/doctor
```

If `/reload-plugins` warns that it would re-read the conversation, rerun it as `/reload-plugins --force`. See [Updating the skills plugin](../../integrations/connect-to-ai-agent/#updating-the-skills-plugin) for the shell and auto-update paths.

The installer uses the public HTTPS marketplace at `https://github.com/yugabyte/meko-skills.git`; GitHub SSH credentials aren't required.

#### Tool calls still send a `scope` argument after upgrading to v2.1.0

Tool-level `scope` (`"read"`, `"write"`, `"admin"`) was retired. Stop sending it in custom integrations, and rerun the installer or refresh the skills plugin so hooks and skills stop sending it too. A temporary compatibility shim currently strips the argument from older clients, but that shim will be removed in a future release. This is unrelated to installer flags such as `--scope user|project`.

#### SessionStart reports a persistent capture failure on an older installer

Upgrade to installer 2.2.0, then use `npx @yugabytedb/meko-mcp@latest --capture-status` for the current failure and queued-exchange count. Quota failures and HTTP `402`/`403` responses keep the capture cursor in place so no transcript range is silently marked as saved.

## Report an issue

To report a bug, use the Meko Discord. Join the community, post in the relevant channel, and the team or other community members will respond.

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
