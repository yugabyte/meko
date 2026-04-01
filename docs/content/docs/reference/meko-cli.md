---
title: Meko CLI
linkTitle: Meko CLI
description: Install and use the Meko CLI
weight: 20
---

The Meko CLI (`meko`) is the primary tool for managing datapacks from the command line.

## Install meko CLI

Pre-built binaries are published as GitHub Releases for every tagged version. Visit [Releases](https://github.com/yugabyte/meko/releases/latest) to download the archive for your platform.

| Platform | Archive |
|----------|---------|
| macOS (Apple Silicon) | `meko_<VERSION>_darwin_arm64.tar.gz` |
| macOS (Intel) | `meko_<VERSION>_darwin_amd64.tar.gz` |
| Linux x86_64 | `meko_<VERSION>_linux_amd64.tar.gz` |
| Linux ARM64 | `meko_<VERSION>_linux_arm64.tar.gz` |

### Example install on macOS

**Step 1 Download the meko binary tarball**  
To install on macOS (Apple Silicon), dowload the compiled meko binary tarball file that's compatible with your machine from the [Releases](https://github.com/yugabyte/meko/releases/latest) website.

**Step 2 : Unzip the file** 
Double clicking the tar file on a macOS, from the Download folder.

**Step 3 : Move it to /usr/local/bin**
```bash
sudo mv meko /usr/local/bin/
```
This will require you to enter the Mac login password.

**Step 4 : Ensure it is Executable**
```bash
sudo chmod +x /usr/local/bin/meko
```

**Step 5 : Verify the installation**

```bash
meko --version
```

## Basics of Meko CLI

### Login

Before you can use the Meko CLI, you must sign in to the meko platform.

To sign in, navigate to the directory on your terminal where you downloaded the CLI binary and use the following command:

```bash
meko login
```

You will be prompted for your email and password.

Alternatively, pass them as flags:

```bash
meko login --email user@example.com --password secret
```


### Logout

```bash
meko logout
```

Removes locally stored tokens.

### Help

Access command-line help for meko by running the following command:

```sh
meko help
```

For help with specific meko commands, use the --help or -h flag, in the following form:

```sh
meko [ <command> ] -h
```

## Commands

| Command | Description |
| :------ | :---------- |
| `meko login` | Authenticate with the Meko API server. |
| `meko logout` | Remove locally stored tokens. |
| `meko help` | Help with Meko CLI command or sub-command. |
| `meko datapack create` | Create a new datapack. |
| `meko datapack list` | List all datapacks. |
| `meko datapack status` | View datapack summary. |
| `meko datapack delete` | Permanently delete a datapack. |

### Global flags

These flags can be used with any `meko datapack` subcommand.

| Flag | Description |
| :--- | :---------- |
| `--json` | Output raw JSON instead of formatted text |
| `--verbose`, `-v` | Enable verbose [logging](#logging) |

### Datapack commands

All `meko datapack` subcommands require you to be [signed in](#login). If you are not signed in, they will fail with:

```text
Error: not logged in - run `meko login` first
```

#### datapack create

When you sign in, a default datapack named `meko-default-datapack` is automatically assigned to your account.

If you need to create more than one datapacks, use `datapack create` to provision a new datapack with a dedicated database, MCP server endpoint, and database access credentials.

```bash
meko datapack create --name my-datapack
```

The request typically takes up to 10 seconds.

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
    "url": "https://mekodev.com/<account-id>/mcp",
    "api_key": "318ce1c589d08edb11e24b0a781fe675"
  },
  "db_access": {
    "user_name": "db_my-datapack",
    "password": "d28a0d1498c1ab9692184dd9c9cd1a79",
    "conn_str": "postgresql://db_my-datapack:d28a0d14...@host:5433/account-id_my-datapack"
  }
}
```

#### datapack list

Display a table of all datapacks with name, ID, and creation date.

```bash
meko datapack list
```

For machine-readable output:

```bash
meko datapack list --json
```

#### datapack status

Display summary info: name, ID, agent count, knowledge base count, and creation date.

```bash
meko datapack status --name my-datapack
```

Display summary info in json format:

```bash
meko datapack status --name my-datapack --json
```

#### datapack delete

Delete the specified datapack. This permanently destroys the datapack and its underlying database.

```bash
meko datapack delete --name my-datapack
```

You will be prompted to re-type the datapack name as confirmation.

To skip the confirmation prompt (for scripts):

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

## Build from Source

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