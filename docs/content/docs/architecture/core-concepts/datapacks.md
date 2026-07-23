---
title: Datapacks
linkTitle: Datapacks
description: The isolation unit for agent data in Meko
weight: 10
---

A **datapack** is Meko's fundamental unit of isolation. Each datapack provides a self-contained persistent store for conversations, memory, and knowledge. MCP clients connect to the shared endpoint (`https://mcp.mekodata.ai/mcp`) and target a datapack by passing the datapack ID in tool calls.

## What's in a datapack

When you create a datapack, Meko provisions isolated storage, scoped to your agent(s), and managed internally by Meko.

To connect an external MCP client (Cursor, Claude Code, and so on), you need:

- The **MCP server URL**, `https://mcp.mekodata.ai/mcp` (the same for every datapack and user).
- A **user API key**: a long-lived key you generate under **Settings>API Keys** in the portal (values start with `mko_tkn_`).

Agents select the datapack to connect to by passing the `datapack_id` inside each MCP tool call, not by using a different URL per datapack.

## Create a datapack

The first time you sign in to the Meko portal, Meko walks you through the steps to create your default datapack (`meko_default_datapack`) and connect it to your client.

To create additional datapacks:

1. In the Meko portal, navigate to **Datapacks**.
1. Click **Add Datapack**.
1. Follow the instructions in the configure wizard.

> [!NOTE] Free tier
>
> Free accounts can create up to **10 datapacks**. When you reach the limit, **Add Datapack** is disabled until you upgrade. See [Meko pricing](https://mekodata.ai/pricing/) for plan details.

<!-- Using the CLI:

```bash
meko datapack create --name my-datapack
```

This returns the datapack's MCP server URL and API key:

```json
{
  "datapack_id": "e02a7107-c79e-451d-b360-9cd1e13f175d",
  "agent_id": "18311fce-36c5-40fe-bd45-67f5913b0b52",
  "name": "my-datapack",
  "mcp_server": {
    "url": "https://mekodev.com/<account_id>/mcp",
    "api_key": "318ce1c589d08edb11e24b0a781fe675"
  }
}
``` -->

## Share a datapack

To share your datapack with colleagues:

1. In the Meko portal, under **Datapacks**, select the datapack you want to share.
1. Click **Share**.
1. Enter the email of the person you want to share with, and select the permission level.
1. Click **Send Invite**.

## Isolation model

Each datapack is fully isolated:

- **Data isolation**. Logical isolation within a shared YugabyteDB universe. One agent cannot access another's data.
- **Credential isolation**. User API keys and database credentials are scoped to your account; datapack data is separated in storage.
- **MCP isolation**. Tool calls include a `datapack_id` so the shared MCP server only reads and writes that datapack's data.

## Connect via MCP

Each datapack exposes an MCP server that agents use to interact with their data.

When you create a datapack in the Meko portal, Meko provides instructions to connect your datapack to your agent automatically. The connection commands are also available under **Connect** in the Meko portal.

You can also configure clients by manually adding this configuration to your coding agent or agent framework. See [Integrations](/integrations/).

## Automatic capture and datapack pinning

For on-demand tool calls, your agent chooses the datapack per call — for example, by naming it in your prompt ("save this to my `research` datapack"). Automatic capture (the background behavior installed by the [Automated install](../../../quick-start/#connect-your-ai-client) flow, such as Claude Code and Codex session hooks) works differently, because there's no per-call prompt to read a datapack name from: it needs a datapack to write to by default.

That default is your **pinned datapack**. If you haven't pinned one, automatic capture falls back to your account's default datapack (`meko_default_datapack`).

For Claude Code and Claude Desktop, the Automated install provides two selector skills:

- **`meko-select-datapack`** for Claude Code, through the `meko-agent-skills` plugin.
- **`meko-select-datapack-desktop`** for Claude Desktop, which has no session hooks. The Desktop installer produces both `meko-mcp-tools-desktop.skill` and `meko-select-datapack-desktop.skill`; import both files into Claude Desktop.

> [!NOTE] Cursor and Codex
>
> Their Automated install includes capture hooks and the core `meko-mcp-tools` skill, but not the guided datapack selector. You can still name a datapack in an on-demand prompt without changing your automatic-capture pin.

Ask your agent in plain language to list, inspect, switch, or clear the active datapack:

```text
Switch my Meko datapack to research
```

You can also invoke the skill directly:

```text
# Claude Code
/meko-agent-skills:meko-select-datapack

# Claude Desktop
/meko-select-datapack-desktop
```

The selector can:

- List the datapacks you can access, sorted newest first, and mark the current pin.
- Show up to 10 datapacks at a time; reply with `next` or `prev` to move between pages.
- Pin by replying with a row number or by entering part of a datapack name. If a search has several matches, the selector asks you to choose rather than guessing.
- Automatically select the datapack when your account has only one.
- Offer to set a pin before the first Meko write when you have several datapacks and no pin.
- Report the current pin when you ask "which datapack am I on?"
- Leave the current pin unchanged with `cancel`, or remove it with `clear`, `none`, or `unpin`.

The pin is a default, not a lock. You can target another datapack for one tool call without changing the pin; later calls continue using the pinned datapack.

### How the pin persists

In **Claude Code**, the pin is scoped to the current project using the session's `agent_id` (for example, `claude_code:meko-mcp-server`). The selector stores it in the local Meko capture state, and the SessionStart hook injects it into new sessions and routes conversation creation, memory preload, and automatic checkpoints to that datapack. The pin survives `/clear`, `/compact`, restarting Claude Code, and opening another Claude Code window in the same repository. Projects with different `agent_id` values can have different pins.

In **Claude Desktop**, the pin is stored as a tagged Meko memory under `agent_id="claude_desktop"`. Before a Meko-using turn, the Desktop skill recalls that pin and passes its `datapack_id` to Meko calls. The pin therefore survives across Desktop conversations until you switch or clear it, without relying on local files or hooks.

The Claude Code selector shows your actual access role for each datapack, such as `owner` or `maintainer`.

> [!NOTE] Claude Desktop role display
>
> The Desktop selector currently labels every listed datapack as **Owner**, even when your actual shared-datapack grant is different, such as **Maintainer**. This is a display limitation only; Meko continues to enforce your actual permissions.

If a pinned datapack is later deleted or you lose access to it, calls using the stale pin fail rather than silently writing to another datapack. Run the selector again to choose an accessible datapack, or clear the pin to return to the account default.

### When a switch takes effect

A saved conversation can't move between datapacks after it's created. The selector stores a new pin immediately, and agent-driven calls that aren't tied to the current saved conversation can use it immediately. Automatic capture for the conversation already in progress keeps writing to the datapack where that conversation was created.

Start a **new session or chat** to route automatic capture to the newly pinned datapack. Clearing the pin follows the same rule: the current conversation remains where it started, while new sessions fall back to `meko_default_datapack`. On Claude Desktop, the agent can instead start a new Meko conversation immediately if you ask the switch or clear to take effect in the current chat.

## Manage datapacks

Manage your datapacks from the **Datapacks** page in the Meko portal. **Datapacks** lists all your datapacks. From here you can add and share datapacks.

Select a datapack to view the conversations, memories, learnings, and knowledge the datapack holds.

From here, you can review and promote learnings to [knowledge](../knowledgebase/).

<!--
| Command | Description |
| :------ | :---------- |
| `meko datapack list` | List all your datapacks |
| `meko datapack status --name <n>` | View datapack summary |
| `meko datapack delete --name <n>` | Permanently delete a datapack |

See the [CLI reference](../../../reference/meko-cli/) for full details.
-->