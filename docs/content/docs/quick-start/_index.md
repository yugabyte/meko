---
title: Quick Start
headerTitle: Meko Quick Start
linkTitle: Quick Start
headcontent: Get up and running with Meko in minutes
description: Sign up, connect your IDE, and explore your first datapack
weight: 1000
type: docs
---

Get from zero to a working Meko integration in minutes.

## Sign up

1. Create a Meko account at the Meko portal. You can sign up with your Gmail or email address.

1. Sign in to your account.

1. Copy your MCP server URL and API key.

## Connect your IDE

Each Meko account has one or more datapacks, which is a container for memory and knowledge. A default datapack is created for you when you sign up.

To start using your datapack, register the MCP Server associated with this datapack with your AI chatbot-enabled IDE or development tool.

{{< tabpane text=true right=false >}}
{{% tab header="Cursor Desktop" lang="cursor" %}}

Add to your Cursor MCP settings (`.cursor/mcp.json`):

```json
{
  "mcpServers": {
    "meko": {
      "url": "<mcp-server-url>",
      "headers": {
        "API_KEY": "<mcp-server-api-key>"
      }
    }
  }
}
```

{{% /tab %}}
{{% tab header="Claude Desktop" lang="claude" %}}

Add the MCP server configuration. The method depends on your Claude version.

1. Go to **Claude>Settings>Developer>Edit Config**.

1. Edit the JSON file and add the following:

    ```json
    {
      "mcpServers": {
        "meko": {
          "command": "npx",
          "args": [
            "-y",
            "mcp-remote",
            "<mcp-server-url>",
            "--allow-http"
          ],
          "env": {
            "API_KEY": "<mcp-server-api-key>"
          }
        }
      },
    }
    ```

1. Restart Claude.

{{% /tab %}}
{{% tab header="Claude Code" lang="claudecode" %}}

Run the following command in your shell to add the Meko MCP server to Claude Code:

```sh
claude mcp add --scope user --transport http meko <mcp-server-url> --header "API_KEY: <mcp-server-api-key>"
```

{{% /tab %}}
{{% tab header="OpenAI Codex" lang="openai" %}}
Add the MCP server configuration to your Codex config.toml settings:

```toml
[mcp_servers.meko]
url = "<mcp-server-url>"
bearer_token_env_var = "<mcp-server-api-key>"
```

{{% /tab %}}
{{% tab header="More" lang="more" %}}
More integrations are available, and others are coming soon.

See [Integrations](../integrations/).
{{% /tab %}}
{{< /tabpane >}}

## Explore

Meko uses _memory_ to enhance user experience through _personalization_.

Ask this question of your AI IDE or development tool without Meko:

```text
Propose a 5 day vacation for me. Do not use Meko or any prior recollection of my preferences in generating your response.
```

Your AI either recommends a randomly-selected vacation, or perhaps asks generic follow-up questions such as "What kind of vacation?", "What budget?", or "To where?"

With Meko, however, your AI can remember your vacation preferences, and use them to generate better responses.

### Create a memory

In your IDE or dev tool, do the following:

1. Tell the agent to store your preferences:

    ```text
    You have access to the Meko platform through MCP server.
    Have Meko remember all my personal preferences.
    ```

1. Type the following to persist your personal preferences as memory:

    ```text
    Please remember that I like to take vacations at tropical beach locations, on a budget
    ```

1. Start a new chat session, then type:

    ```text
    Propose a 5 day vacation for me.
    ```

Your AI can now leverage the memory to provide a more relevant response.

For example, Cursor can respond with something similar to the following:

```text
Here’s a 5-day plan tuned to what you’ve saved: tropical beach + budget.

## Pick a base: Isla Holbox or Puerto Morelos area, Mexico (Yucatán)

Why it fits: Warm water, palm-lined beaches, easy buses from Cancún airport,
hostels / small hotels and street food keep costs down. (Swap-ins: Koh Lanta,
Thailand or Nusa Penida + Sanur, Bali if you’re already closer to Asia.)

<...>

---

## Budget levers

- Stay: hostel dorm or simple guesthouse slightly back from the sand (often 
  half the price of beachfront).
- Eat: markets, loncherías, set “menu del día” lunches; limit restaurant dinners.
- Move: buses + shared shuttles; avoid private taxis except airport if needed.
- Play: prioritize free swimming, walking, and one paid half-day trip instead
  of stacked tours.

---

## If you tell me your month, home airport region, and solo vs. group, I can
## narrow this to one place, rough daily spend, and a rain-day backup.
```

The agent retrieves your stored preferences and immediately proposes a vacation aligned to these preferences (a tropical beach vacation within budget). No follow-up questions were needed.

### Check your memories

You can verify what Meko stored by checking your datapack's memories in the Meko portal. You'll see:

- The stored memories with their extracted entities
- Key metrics: number of memories, knowledge base sources, and MCP requests

Type:

```text
What does Meko remember about me?
```

### Upload knowledge

Meko allows you add knowledge to your datapack by simply uploading documents. We do this using the [meko CLI](../../reference/cli-overview/).

In the following example, we'll add documents to your datapack and query them using RAG.

Upload documents to your datapack's knowledge base:

```bash
meko datapack add_knowledge_base --name my-datapack --url s3://bucket/docs/
```

<!-- TODO: Add details on portal-based document upload once the web portal is available -->

### Query your knowledge

Once your documents are indexed, query them through your IDE:

```text
Search the knowledge base for information about <your topic>
```

The MCP server performs semantic search across your indexed documents and returns relevant chunks.

### Observe your datapack

Check what's happening inside your datapack:

- **Memories** --- View stored memories and their extracted entities
- **Knowledge bases** --- See indexed document sources and their status
- **Traces** --- Review LLM reasoning traces and execution logs

<!-- TODO: Add LangFuse observability walkthrough once integration details are finalized -->
<!-- TODO: Add screenshots showing costs, calls, and timings -->

## Recap

We've seen how Meko uses _memory_ to enhance a user experience through _personalization_.

Memory from Meko can do much more. Beyond personal preferences, it can also capture personal, team, and organization preferences, standards, policies, and more. Meko's memory empowers teams to collaborate. Memories can be captured, shared, managed, and extended... and not just for human teams, but also for agent teams. Meko is a multi-agent data layer for shared memory.

Under the hood, memories are stored in a graph database... where the graph's entities and relationships are derived by Meko's processing of the LLM prompts.

## Next

- [Examples](../examples/). Explore more real-world use cases.
- [Working with Memory](../guides/working-with-memory/). Deep dive into memory operations.
- [Working with Knowledge Bases](../guides/working-with-knowledge-bases/). Advanced knowledge base management.
