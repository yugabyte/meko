---
title: Explore Memory
linkTitle: Explore Memory
description: See how to use Meko to store your memories
weight: 1010
---
## Sign Up

Create a Meko account at the Meko portal. You can sign up with your Gmail or email address.

## Try This without Meko

Type this into your LLM chatbot:

```text
Propose a 5 day vacation for me.
```

Without any stored memories, your agent either recommends a randomly-selected vacation, or perhaps asks generic follow-up questions such as "What kind of vacation?", "What budget?", or "To where?"

## Connect Your IDE to Meko

Let's register the Meko MCP Server with your IDE or Developer Tool

Each Meko account has one or more Datapacks, which is a container for memory and knowledge.  A default Datapack has been created for you.  Register the MCP Server associated with this Datapack with your AI chatbot-enabled IDE or development tool.    

<!-- TODO: FORMAT THE BELOW IN TABS --> 

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

For troubleshooting, see [Integrations](../integrations/_index)

### Claude Code

Add the MCP server configuration. The method depends on your Claude version:

**Developer Mode** Go to Claude > Settings > Developer > Edit Config. Edit the JSON file and add:

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

For troubleshooting, see [Integrations](../integrations/_index)
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

For troubleshooting, see [Integrations](../integrations/_index)

## Configure Your IDE for Meko
 
In your IDE or dev tool, enter the following:

```text
You have access to the Meko platform through MCP server.
Have Meko remember all my personal preferences.
```

<!-- TODO: validate the above.   Evaluate whether the prompt also needs this text:  "agent_id is cursor_agent and user_id is <your_username>" -->

If the connection is working, your IDE will use the Meko MCP tools to store the memory.

## Create a Memory 

Type this to persist your personal preferences as memory:

```text
Please remember that I like to take vacations at tropical beach locations, on a budget
```

## Retry with Meko

**Start a new chat session**, then type:

```text
Propose a 5 day vacation for me.
```

Compare the response to earlier response prior to connecting to and configuring Meko.   Notice how the agent retrieves your stored preferences and immediately proposes a vacation aligned to these preferences (a tropical beach vacation within budget), and how no follow-up questions were needed.

## Visibility & Observability

You can verify what Meko stored by checking your datapack's memories in the Meko portal. You'll see:

- The stored memories with their extracted entities
- Key metrics: number of memories, knowledge base sources, and MCP requests

## Recap

What we learned: the value of memory.


## Next

* Continue to **[Explore Knowledge](../explore-knowledge/)** to see Meko in action.
