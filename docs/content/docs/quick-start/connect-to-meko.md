---
title: Connect to Meko
linkTitle: Connect to Meko
description: Sign up, authenticate, and connect your IDE to Meko
weight: 1010
---

## Step 1: Sign Up

Create a Meko account at the Meko portal. You can sign up with your Gmail or email address.

## Step 2: Log In via CLI

Install the Meko CLI (see [Install the CLI](../../reference/install-cli/)) and authenticate:

```bash
meko login
```

You'll be prompted for your email and password. Alternatively, pass them as flags:

```bash
meko login --email user@example.com --password secret
```

To authenticate against a different API server:

```bash
meko login --api-server https://my-server.example.com
```

The default API server is `https://mekodev.com`. Credentials are stored locally at `~/var/meko/credentials.json`.

## Step 3: Create a Datapack

Create your first datapack:

```bash
meko datapack create --name my-datapack
```

This provisions a dedicated database, MCP server endpoint, and access credentials. Save the output --- you'll need the MCP server details for the next step.

## Step 4: Connect Your IDE

Each datapack exposes an MCP server. Add the following configuration to your IDE to connect.

### Cursor

Add to your Cursor MCP settings (`.cursor/mcp.json`):

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

### Claude Code

Add the MCP server configuration. The method depends on your Claude version:

**V0.8 (Developer Mode):** Go to Claude > Settings > Developer > Edit Config. Edit the JSON file and add:

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

Then restart Claude.

**V0.9 (Custom Connector):** Go to Claude > Settings > Connectors > Add Custom Connector. Add the MCP server URL from your datapack creation output with the name "meko".

**V1.0 (Marketplace):** In Claude, browse Connectors and search for "Meko".

### OpenAI Codex

Add the MCP server configuration to your Codex settings:

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

## Step 5: Verify the Connection

In your IDE, try a prompt that uses Meko:

```text
You have access to the Meko platform through MCP server.
Have Meko remember all my personal preferences.
agent_id is cursor_agent and user_id is <your_username>
```

If the connection is working, your IDE will use the Meko MCP tools to store the memory.

## Next Steps

Continue to [Explore Your Datapack](../explore-your-datapack/) to see Meko in action with the Trip Planner demo.
