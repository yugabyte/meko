---
title: Meko CLI
linkTitle: Meko CLI
description: Overview of the Meko CLI
weight: 20
---

The Meko CLI (`meko`) is the primary tool for managing datapacks from the command line. 


### Login

In order to use the Meko CLI, you must log into the meko platform first. To login, navigate to the directory on your terminal where you downloaded the CLI binary.

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