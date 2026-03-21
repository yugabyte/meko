---
title: Cursor
linkTitle: Cursor
description: Connect Meko to Cursor via MCP
weight: 10
---

## Setup

1. Create a datapack if you haven't already:

   ```bash
   meko datapack create --name my-datapack
   ```

2. Add the MCP server to your Cursor configuration. Create or edit `.cursor/mcp.json` in your project root:

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

3. Restart Cursor to pick up the new MCP server.

## Verify the Connection

In Cursor's chat, type:

```text
You have access to the Meko platform through MCP server.
Have Meko remember all my personal preferences.
agent_id is cursor_agent and user_id is <your_username>
```

If connected, Cursor will use Meko's MCP tools to store the memory.

## Usage

Once connected, you can use natural language to interact with Meko:

- **Store memories:** "Remember that I prefer Python type hints on all functions"
- **Search memories:** "What are my coding preferences?"
- **Query knowledge:** "Search the knowledge base for API documentation"

<!-- TODO: Add screenshots of Cursor MCP configuration (Heather/Yu-Shen) -->
<!-- TODO: Add screenshots of Cursor using Meko tools in chat -->
