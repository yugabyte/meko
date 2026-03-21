---
title: CLI Overview
linkTitle: CLI Overview
description: Authentication, global flags, and logging for the Meko CLI
weight: 10
---

The Meko CLI (`meko`) is the primary tool for managing datapacks, knowledge bases, and memory from the command line.

## Authentication

All datapack commands require a valid login session. If you are not logged in, commands will fail with:

```text
Error: not logged in - run `meko login` first
```

### Login

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

The default API server is `https://mekodev.com`. Credentials are stored locally at `~/var/meko/credentials.json`.

### Logout

```bash
meko logout
```

Removes locally stored tokens.

## Global Flags

| Flag | Description |
|------|-------------|
| `--json` | Output raw JSON instead of formatted text |
| `--verbose`, `-v` | Enable verbose logging |

These flags can be used with any `meko datapack` subcommand.

## Logging

The CLI writes structured logs to `~/var/meko/logs/`. Use `--verbose` to enable debug-level output. Log files are useful for diagnosing authentication or network errors.
