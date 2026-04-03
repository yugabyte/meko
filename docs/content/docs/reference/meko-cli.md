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

To create more, use `datapack create` to provision a new datapack with a dedicated database, MCP server endpoint, and database access credentials as follows:

```bash
meko datapack create --name my-datapack
```

The request typically takes a few seconds.

You should see an output similar to the following:

```output
 ✅ Datapack Created

  Name             my-datapack
  Datapack ID      e02a7107-c79e-451d-b360-9cd1e13f175d
  Agent ID         18311fce-36c5-40fe-bd45-67f5913b0b52

  ────────────────────────────────────────────────────────
  MCP Server
  ────────────────────────────────────────────────────────
  URL              https://d81bff68-2659-4d9e-a3a3-26ce6e0fb4de.mcp.mekodev.com/mcp
  API Key          318ce1c589d08edb11e24b0a781fe675

  ────────────────────────────────────────────────────────
  Database Access
  ────────────────────────────────────────────────────────
  User             db_d81bff68-2659-4d9e-a3a3-26ce6e0fb4de_my-datapack
  Password         40ab21c8637c088862f70fa4503ce7ff

  postgresql://db_d81bff68-2659-4d9e-a3a3-26ce6e0fb4de_my-datapack1:40ab21c8637c088862f70fa4503ce7ff@k8s-ybdevmek-ybmekosa-c74a0a6102-e4aec974c3e8e420.elb.us-east-2.amazonaws.com:5433/d81bff68-2659-4d9e-a3a3-26ce6e0fb4de_my-datapack
```

For machine-readable output, use the `--json` flag:

```bash
meko datapack create --name my-datapack --json
```

Example JSON output:

```json
{
  "datapack_id": "e02a7107-c79e-451d-b360-9cd1e13f175d",
  "agent_id": "18311fce-36c5-40fe-bd45-67f5913b0b52",
  "name": "my-datapack",
  "mcp_user_name": "mcp_h66hkj87-4567-6y7u-b7b7-12gg5y7h8jj",
  "mcp_server": {
    "url": "https://d81bff68-2659-4d9e-a3a3-26ce6e0fb4de.mcp.mekodev.com/mcp",
    "api_key": "318ce1c589d08edb11e24b0a781fe675"
  },
  "db_access": {
    "user_name": "db_d81bff68-2659-4d9e-a3a3-26ce6e0fb4de_my-datapack",
    "password": "40ab21c8637c088862f70fa4503ce7ff",
    "conn_str": "postgresql://db_d81bff68-2659-4d9e-a3a3-26ce6e0fb4de_my-datapack1:40ab21c8637c088862f70fa4503ce7ff@k8s-ybdevmek-ybmekosa-c74a0a6102-e4aec974c3e8e420.elb.us-east-2.amazonaws.com:5433/d81bff68-2659-4d9e-a3a3-26ce6e0fb4de_my-datapack"
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
> This action permanently destroys the datapack and its underlying database.
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

## Build from source

If you decide to download the CLI source code from the GitHub repository, you need to build the CLI locally.

Requires Go 1.25+.

```bash
cd cli_app
go mod download
go build -o meko .
```

To authenticate against a different API server that you may build from the GitHub repository, you can login using the following command:

```bash
meko login --api-server https://my-server.example.com
```

The default API server is `https://mekodev.com`.

Credentials are stored locally in `~/var/meko/credentials.json`.