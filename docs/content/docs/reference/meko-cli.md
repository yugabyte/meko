---
hide:
  - navigation
---
---
title: Meko CLI
linkTitle: Meko CLI
description: Install and use the Meko CLI
weight: 20
---

The Meko CLI (`meko`) is the primary tool for managing datapacks from the command line.

## Install meko CLI

Pre-built binaries are published as GitHub Releases for every tagged version. Download the archive for your platform from the latest [GitHub Releases](https://github.com/yugabyte/meko/releases/latest).

| Platform | Archive |
| :------- | :------ |
| macOS (Apple Silicon) | `meko_<VERSION>_darwin_arm64.tar.gz` |
| macOS (Intel) | `meko_<VERSION>_darwin_amd64.tar.gz` |
| Linux x86_64 | `meko_<VERSION>_linux_amd64.tar.gz` |
| Linux ARM64 | `meko_<VERSION>_linux_arm64.tar.gz` |

### Example install on macOS

To install Meko CLI on macOS (Apple Silicon), do the following:

1. Download the compiled Meko binary tar file that's compatible with your machine from the [Releases](https://github.com/yugabyte/meko/releases/latest) website.

1. Unzip the file (double click the tar file).

1. Move `meko` to `/usr/local/bin/`, and make it executable as follows:

    ```bash
    sudo mv meko /usr/local/bin/ && sudo chmod +x /usr/local/bin/meko
    ```

    > [!TIP]
    >
    > Ensure /usr/local/bin/ is in your PATH to run `meko` from anywhere.

1. Verify the installation:

    ```bash
    meko --version
    ```

## Commands

| Command | Description |
| :------ | :---------- |
| [`meko login`](#login) | Authenticate with the Meko API server. |
| [`meko logout`](#logout) | Remove locally stored tokens. |
| [`meko help`](#help) | Help with Meko CLI command or sub-command. |
| [`meko datapack create`](#datapack-create) | Create a new datapack. |
| [`meko datapack list`](#datapack-list) | List all datapacks. |
| [`meko datapack status`](#datapack-status) | View datapack summary. |
| [`meko datapack delete`](#datapack-delete) | Permanently delete a datapack. |

## Basics of Meko CLI

### Login

You must sign in to the Meko platform before you can use the CLI.

```bash
meko login
```

Enter your email and password when prompted. Alternatively, you can use flags:

```bash
meko login --email user@example.com --password secret
```

### Logout

To end your session, run:

```bash
meko logout
```

This removes locally stored tokens.

### Help

To view all available commands and options, run:

```sh
meko help
```

For help with a specific command, use the `--help` or `-h` flag:

```sh
meko [ <command> ] -h
```

### Global flags

These flags can be used with any `meko datapack` subcommand.

| Flag | Description |
| :--- | :---------- |
| `--json` | Output raw JSON instead of formatted text |
| `--verbose`, `-v` | Enable verbose [logging](#logging) |

### Datapack commands

All `meko datapack` subcommands require you to be [signed in](#login). If you are not signed in, the commands will fail with the following error:

```text
Error: not logged in - run `meko login` first
```

#### datapack create

When you sign in, a default datapack named `meko_default_datapack` is automatically assigned to your account.

To create a new datapack, use `datapack create` to provision a new datapack with a dedicated database, MCP server endpoint, and database access credentials as follows:

```bash
meko datapack create --name my-datapack
```

The request typically takes a few seconds.

You should see an output similar to the following, and you should save them somewhere safe for future reference:

```output
 ✅ Datapack Created

  Name             <my-datapack-name>
  Datapack ID      <my-datapack-id>
  Agent ID         <my-agent-id>

  ────────────────────────────────────────────────────────
  MCP Server
  ────────────────────────────────────────────────────────
  URL              <my-mcp-server-url>
  API Key          <my-mcp-server-api-key>

  ────────────────────────────────────────────────────────
  Database Access
  ────────────────────────────────────────────────────────
  User             <my-db-user-name>
  Password         <my-db-password>
  Connection String  <postgres-sql-connection-string>
```

For machine-readable output, use the `--json` flag:

```bash
meko datapack create --name my-datapack --json
```

Example JSON output:

```json
{
  "datapack_id": "<my-datapack-id>",
  "agent_id": "<my-agent-id>",
  "name": "my-datapack",
  "mcp_user_name": "<my-mcp-user-name>",
  "mcp_server": {
    "url": "<my-mcp-server-url>",
    "api_key": "<my-mcp-server-api-key>"
  },
  "db_access": {
    "user_name": "<my-db-user-name>",
    "password": "<my-db-password>",
    "conn_str": "<postgres-sql-connection-string>"
  }
}
```

#### datapack list

Display a table of all datapacks, including their name, ID, and creation date:

```bash
meko datapack list
```

For machine-readable output, use the `--json` flag:

```bash
meko datapack list --json
```

#### datapack status

Display summary information including name, ID, agent count, knowledge base count, and creation date:

```bash
meko datapack status --name my-datapack
```

For machine-readable output, use the `--json` flag:

```bash
meko datapack status --name my-datapack --json
```

#### datapack delete

Delete a specific datapack.

> [!WARNING]
>
> This action permanently destroys the datapack and its underlying database. This is a hard delete, and all memories, knowledge base, and observability traces will be permanently deleted without any recourse of recovering the data.  Use this command with extreme caution.
>

```bash
meko datapack delete --name my-datapack
```

You will be prompted to re-type the datapack name to confirm.

To skip the confirmation prompt (useful for scripts), use the `--force` flag:

```bash
meko datapack delete --name my-datapack --force
```

<!--
### Knowledge Base commands

```bash
meko datapack add_knowledge_base --name <datapack-name> --url <source-url>
```

Adds a new knowledge base source to a datapack. Meko will fetch the documents, preprocess them, chunk the text, generate embeddings, and index them for semantic search.

### Options

| Flag | Description |
|------|-------------|
| `--name` | Datapack name (required) |
| `--url` | Source URL --- S3, local path, or NFS (required) |
| `--settings` | JSON string with chunking settings (optional) |

### Examples

```bash
# Add from S3
meko datapack add_knowledge_base --name my-datapack --url s3://bucket/docs/

# With custom chunk size
meko datapack add_knowledge_base --name my-datapack \
  --url s3://bucket/docs/ \
  --settings '{"chunk_size": 512}'
```

<!-- TODO: Add documentation for additional KB CLI commands as they become available -->
<!-- TODO: Document list, status, and delete operations for knowledge bases via CLI -->

## Logging

The CLI writes structured logs to `~/var/meko/logs/`. Use `--verbose` to enable debug-level output. Log files are useful for diagnosing authentication or network errors.

```sh
meko [command] --verbose
```