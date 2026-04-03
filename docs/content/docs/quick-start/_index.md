---
title: Quick start
linkTitle: Quick start
description: Sign up, connect any AI tool, and explore your first datapack
weight: 1000
type: docs
---

Get from zero to a working Meko integration in 60 seconds.

## Sign up

1. Create a Meko account at the Meko portal.

2. Sign in to your account.

3. Obtain these and store them in a safe place:
	* Your MCP server URL
	* Your API key

## Add skills to your agent (temporary instructions during Tech Preview):

1. Clone the meko-mcp-server repo
```sh
git clone git@github.com:yugabyte/yugabytedb-mcp-server.git
```

2. Switch to the branch add-skills
```sh
<TBD>
```

3. From the meko-mcp-server directory run the following command: 

```sh
npx skills add ./skills -g
```

3. You will be prompted to select the skills you want to install, select both meko-mcp-tools as well as ysql.
4. Next you will be prompted to select the agent you want to install the skills to, select your agent, and it should install both the skills to the agents.
## Connect any AI client

Each Meko account has one or more datapacks, which is a container for memory and knowledge. A default datapack is created for you when you sign up.

To start using your datapack, register the MCP Server associated with this datapack with any AI chatbot-enabled IDE or development tool.

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

## Explore knowledge

Meko allows management of and collaboration around "knowledge" and "memory".   

* Knowledge = raw documents    
* Memory = derived information from documents

Note: conversation history is also considered a "raw document"

### Upload knowledge 
**(This is coming soon to the web portal... Skip this section unless you want to try the CLI approach)**

We'll add documents to your datapack and query them using RAG.   

In your IDE, type the below, and attach a text file: 

```text
Store the attached document as long-term knowledge.
<attach a document>
```

##### CLI approach

We can also do this using the Meko CLI.  Install the Meko CLI using instructions [here](../../reference/cli-overview/) 

Upload documents to your datapack's knowledge base:

```bash
meko datapack add_knowledge_base --name my-datapack --url s3://bucket/docs/
```

<!-- TODO: Add details on portal-based document upload once the web portal is available -->

### Query your knowledge 
**(This is coming soon to the web portal... Skip this section unless you want to try the CLI approach mentioned above)**

Once your documents are parsed, chunked, and indexed by Meko, query them through your IDE:

```text
Search the knowledge base for information about <your topic>
```

The MCP server performs semantic search across your indexed documents and returns relevant chunks.  In short, Meko has automated the retrieval augmented generation (RAG) pre-processing pipeline by reading, parsing, chunking, vectorizing/indexing the documents, and by providing a semantic search capability.

## Explore memory

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

### Observe your datapack

Check what's happening inside your datapack:

- **Memories** --- View stored memories and their extracted entities
- **Knowledge bases** --- See indexed document sources and their status
- **Traces** --- Review LLM reasoning traces and execution logs

<!-- TODO: Add LangFuse observability walkthrough once integration details are finalized -->
<!-- TODO: Add screenshots showing costs, calls, and timings -->

## Next steps

With your first datapack live, you can transition from a basic demo to a robust, context-aware development environment. Explore the following advanced capabilities to fully leverage Meko's persistent memory and knowledge graph.

{{< card-group >}}
  {{< card
    href="guides/working-with-memory/"
    title="Manage and curate memories"
    desc="Search and list what is stored, compress overlapping facts, or clear a datapack when you need a reset."
  >}}
  {{< card
    href="guides/working-with-knowledge-bases/"
    title="Grow your knowledge base"
    desc="Add more sources and combine document RAG with memory-driven assistants."
  >}}
  {{< card
    href="examples/meko-in-your-ide/"
    title="Share context across people and tools"
    desc="Collaborate by sharing datapacks with your team, so that every agent follows the same project standards, or carry personal preferences across IDEs."
  >}}
  {{< card
    href="examples/build-software-with-meko/"
    title="Build resilient multi-agent systems"
    desc="Store conversation history and agent reasoning traces, enabling features like cross-session recall and resumability after failures."
  >}}
{{< /card-group >}}

<!--
- **Manage and curate memories.** Search and list what is stored, compress overlapping facts, or clear a datapack when you need a reset. See [Working with Memory](../guides/working-with-memory/).
- **Share context across people and tools.** Collaborate by sharing datapacks with your team, so that every agent follows the same project standards, or carry personal preferences across IDEs. See [Meko in your development process](../examples/meko-in-your-ide/) and [Datapacks](../architecture/core-concepts/datapacks/).
- **Build resilient multi-agent systems**. Preserve context across long-running tasks and multiple tools. Meko lowers costs by archiving old history and allows agents to resume work immediately after failures or session resets. See [Meko in your agentic application](../examples/build-software-with-meko/).
- **Grow knowledge bases.** Add more sources and combine document RAG with memory-driven assistants. See [Working with Knowledge Bases](../guides/working-with-knowledge-bases/).
-->