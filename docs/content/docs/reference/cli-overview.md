---
title: Meko CLI
linkTitle: Meko CLI
description: Overview of the Meko CLI
weight: 10
---

The Meko CLI (`meko`) is the primary tool for managing datapacks from the command line. To get started, first download the CLI on your local desktop environment.

## Download a pre-built binary

Pre-built binaries are published as GitHub Releases for every tagged version. Visit [Releases](https://github.com/yugabyte/meko/releases/latest) to download the archive for your platform.

| Platform | Archive |
|----------|---------|
| macOS (Apple Silicon) | `meko_<VERSION>_darwin_arm64.tar.gz` |
| macOS (Intel) | `meko_<VERSION>_darwin_amd64.tar.gz` |
| Linux x86_64 | `meko_<VERSION>_linux_amd64.tar.gz` |
| Linux ARM64 | `meko_<VERSION>_linux_arm64.tar.gz` |

### Example install (macOS apple silicon)

```bash
curl -Lo meko.tar.gz \
  https://github.com/yugabyte/meko/releases/latest/download/meko_0.1.0_darwin_arm64.tar.gz
tar xzf meko.tar.gz
chmod +x meko
sudo mv meko /usr/local/bin/
```

### Verify

```bash
meko --version
```

## Build from Source

If you decide to download the CLI source code from the github repository, you would need to build the CLI locally.

Requires Go 1.25+.

```bash
cd cli_app
go mod download
go build -o meko .
```

### Login

You must log into the meko platform, before you can use the meko CLI.  Navigate to the directory on your terminal where you downloaded the CLI to login.

```bash
meko login
```
You will be prompted for your email and password. Alternatively, pass them as flags:

```bash
meko login --email user@example.com --password secret
```

To authenticate against a different API server:

```bash
meko login --api-server https://my-server.example.com
```

The default API server is `https://mekodev.com`. Credentials are stored locally in `~/var/meko/credentials.json`.


## Command Summary

Here is a summary of all the Meko commands supported in the latest release. More details about each of the commands can be found in the cli-commands section.

| Command | Description |
|---------|-------------|
| `meko login` | Authenticate with the Meko API server |
| `meko logout` | Remove locally stored tokens |
| `meko help` | Help with Meko CLI command or sub-command |
| `meko datapack create` | Create a new datapack |
| `meko datapack list` | List all datapacks |
| `meko datapack status` | View datapack summary |
| `meko datapack delete` | Permanently delete a datapack |


## Global Flags

These flags can be used with any `meko datapack` subcommand.


| Flag | Description |
|------|-------------|
| `--json` | Output raw JSON instead of formatted text |
| `--verbose`, `-v` | Enable verbose logging |


## Logging

The CLI writes structured logs to `~/var/meko/logs/`. Use `--verbose` to enable debug-level output. Log files are useful for diagnosing authentication or network errors.

### Logout

```bash
meko logout
```

Removes locally stored tokens.