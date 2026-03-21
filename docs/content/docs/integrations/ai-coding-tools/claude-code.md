---
title: Claude Code
linkTitle: Claude Code
description: Connect Meko to Claude Code via MCP
weight: 20
---

## Setup

The setup method depends on your version of Claude.

### V0.8 --- Developer Mode

1. Go to **Claude > Settings > Developer > Edit Config**.
2. Edit the JSON file and add your MCP server:

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

3. Restart Claude.

### V0.9 --- Custom Connector

1. Go to **Claude > Settings > Connectors > Add Custom Connector**.
2. Add the MCP server URL from your datapack creation output with the name "meko".

### V1.0 --- Marketplace

1. In Claude, browse **Connectors**.
2. Search for "Meko".
3. Follow the prompts to connect your datapack.

## Verify the Connection

In Claude's chat, type:

```text
You have access to the Meko platform through MCP server.
Have Meko remember all my personal preferences.
agent_id is claude_agent and user_id is <your_username>
```

## Usage

Once connected, use natural language to interact with Meko:

- **Store memories:** "Remember that I prefer concise commit messages"
- **Search memories:** "What are my preferences for code review?"
- **Query knowledge:** "Search the knowledge base for deployment procedures"

<!-- TODO: Add screenshots of each configuration method (Heather/Yu-Shen) -->
<!-- TODO: Add troubleshooting steps for common connection issues -->
