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

## Manage datapacks

Manage your datapacks from the **Datapacks** page in the Meko portal. **Datapacks** lists all your datapacks. From here you can add and share datapacks.

Select a datapack to view the conversations, memories, learnings, knowledge the datapack holds.

From here, you can review and promote learnings to [knowledge](../knowledgebase/).

<!--
| Command | Description |
| :------ | :---------- |
| `meko datapack list` | List all your datapacks |
| `meko datapack status --name <n>` | View datapack summary |
| `meko datapack delete --name <n>` | Permanently delete a datapack |

See the [CLI reference](../../../reference/meko-cli/) for full details.
-->