---
title: Datapacks
linkTitle: Datapacks
description: The isolation unit for agent data in Meko
weight: 10
---

A **datapack** is Meko's fundamental unit of isolation. Each datapack provides a self-contained persistent store for conversations, memory, and knowledge, accessible via a single MCP server endpoint.

## What's in a datapack

When you create a datapack, Meko provisions:

- **An MCP server endpoint**. A unique URL that your agent uses to discover and interact with the datapack.
- **An MCP server API key**. For authenticating MCP requests.
- **Isolated storage**. Data storage scoped to your agent(s), managed internally by Meko.

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

- **Data isolation**. Separate database per datapack. One agent cannot access another's data.
- **Credential isolation**. Unique credentials per datapack with an MCP endpoint.
- **MCP isolation**. Each datapack's MCP server only exposes that datapack's tools and data.

## Connect via MCP

Each datapack exposes an MCP server that agents use to interact with their data:

```json
{
  "mcpServers": {
    "my-datapack": {
      "command": "meko",
      "args": ["mcp", "serve", "--name", "my-datapack"]
    }
  }
}
```

When you create a datapack in the Meko portal, Meko provides instructions to connect your datapack to your agent automatically. The connection commands are also available under **Install** in the Meko portal.

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