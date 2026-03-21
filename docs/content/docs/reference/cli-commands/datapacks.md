---
title: Datapack Commands
linkTitle: Datapacks
description: CLI commands for creating and managing datapacks
weight: 10
---

All `meko datapack` subcommands require a valid login session. If you are not logged in, they will fail with:

```text
Error: not logged in - run `meko login` first
```

## Create a Datapack

```bash
meko datapack create --name my-datapack
```

This provisions a new datapack with a dedicated database, MCP server endpoint, and database access credentials. A spinner is shown while the server processes the request (typically up to 10 seconds).

For machine-readable output:

```bash
meko datapack create --name my-datapack --json
```

Example JSON output:

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

## List Datapacks

```bash
meko datapack list
```

Displays a table of all datapacks with name, ID, and creation date.

```bash
meko datapack list --json
```

## Show Datapack Status

```bash
meko datapack status --name my-datapack
```

Displays summary info: name, ID, agent count, knowledge base count, and creation date.

```bash
meko datapack status --name my-datapack --json
```

## Delete a Datapack

```bash
meko datapack delete --name my-datapack
```

You will be prompted to re-type the datapack name as confirmation, since this permanently destroys the datapack and its underlying database.

To skip the confirmation prompt (for scripts):

```bash
meko datapack delete --name my-datapack --force
```
