---
title: Datapacks
linkTitle: Datapacks
description: The isolation unit for agent data in Meko
weight: 10
---

A **datapack** is Meko's fundamental unit of isolation. Each datapack provides a self-contained environment for an agent, bundling together a dedicated database, MCP server endpoint, and access credentials.

## What's in a datapack

When you create a datapack, Meko provisions:

- **An MCP server endpoint**. A unique URL that your agent uses to discover and interact with the datapack.
- **An MCP server API key**. For authenticating MCP requests.
- **A dedicated database**. An isolated YugabyteDB (YSQL) database with pgvector enabled, scoped to your agent.
- **Database credentials**. A username, password, and connection string for direct PostgreSQL access.

## Create a datapack

Using the CLI:

```bash
meko datapack create --name my-datapack
```

This returns the datapack's MCP server URL, API key, and database connection details:

```json
{
  "datapack_id": "e02a7107-c79e-451d-b360-9cd1e13f175d",
  "agent_id": "18311fce-36c5-40fe-bd45-67f5913b0b52",
  "name": "my-datapack",
  "mcp_server": {
    "url": "https://mekodev.com/<account_id>/mcp",
    "api_key": "318ce1c589d08edb11e24b0a781fe675"
  },
  "db_access": {
    "user_name": "db_my-datapack",
    "password": "d28a0d1498c1ab9692184dd9c9cd1a79",
    "conn_str": "postgresql://db_my-datapack:d28a0d14...@host:5433/account_id_my-datapack"
  }
}
```

## Isolation model

Each datapack is fully isolated:

- **Data isolation**. Separate database per datapack. One agent cannot access another's data.
- **Credential isolation**. Unique credentials per datapack.
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

Add this configuration to your coding agent or agent framework to connect.

For the hosted MCP URL and API key (from the portal or `meko datapack` output), register multiple clients at once with [Connect any AI client](../../quick-start/#connect-any-ai-client).

## Manage datapacks

| Command | Description |
| :------ | :---------- |
| `meko datapack list` | List all your datapacks |
| `meko datapack status --name <n>` | View datapack summary |
| `meko datapack delete --name <n>` | Permanently delete a datapack |

See the [CLI reference](../../../reference/meko-cli/) for full details.
