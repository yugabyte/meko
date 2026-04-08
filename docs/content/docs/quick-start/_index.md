---
title: Quick start
linkTitle: Quick start
description: Sign up, connect any AI tool, and explore your first datapack
weight: 1000
type: docs
---

Get from zero to a working Meko integration in 60 seconds.

## Sign up

1. Create a Meko account at the [Meko portal](https://app.mekodev.com/).

1. Sign in to your account.

    Your default [datapack](../../architecture/core-concepts/datapacks/) (`meko_default_datapack`), is displayed. A datapack is a container for memory and knowledge.

1. Click **Connect**.

1. Copy the following MCP Server details and store them in a safe place:

    - Your MCP server **URL**.
    - Your MCP server **API key**.

## Connect your AI client

Next, connect Claude Desktop, Claude Code, Cursor, VS Code, Codex or any MCP-compatible client** to your Meko datapack.  At the Linux prompt, enter the below text with replacement values for  <meko-mcp-api-key> and <meko-api-key> from the previous step.

```sh
#### Install the latest Node.js 
curl -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.2/install.sh | bash
source ~/.$(basename $SHELL)rc  # This sources .bashrc or .zhsrc
nvm install --lts
#### Run mcp-add to register the Meko MCP server
npx mcp-add@latest \
  --name meko \
  --type stdio \
  --command 'npx -y mcp-remote <meko-mcp-url> --allow-http' \
  --env 'MEKO_API_KEY=<meko-mcp-api-key>' \
  --headers 'MEKO_API_KEY=<meko-api-key>' \
  --scope global \
  --clients 'claude desktop,claude code,cursor,codex,vscode'
```

** If you're using continue, copilot cli, gemini, goose, opencode, and windsurf, modify the "--clients" command-line option to include one or more of these keywords.

For manual instructions, and for guidance on registering the Meko MCP Server with your own AI framework (e.g. LangChain, Llama Index, etc.) in an application development scenario, see [Integrations](../integrations/). 

## Add skills to your agent

> [!NOTE] Temporary instructions
>
> These instructions are temporary, and apply during Tech Preview only.

The Meko Skills package tells agents when and how to use Meko's MCP tools for memory, knowledge, conversation management, and SQL database access. To install it:

1. Download the skills package and start the skills installer

    ```sh
    git clone https://github.com/yugabyte/meko-mcp-server.git
    cd meko-mcp-server
    git checkout add-skills
    npx skills add ./skills -g
    ```

2. When prompted to select the skills to install, select both **meko-mcp-tools** and **ysql**.

3. When prompted to select the agent to install the skills to, select your agent.

## Explore knowledge

Meko allows management of and collaboration around _knowledge_ and _memory_:

- _Knowledge_ is the raw documents you make available to Meko.
- _Memory_ is the information Meko derives from these documents.

In Meko, conversation history is also considered a "raw document".

### Upload knowledge

To see how Meko stores knowledge, add documents to your datapack and query them using RAG.

> [!NOTE] Coming soon
>
> Document upload is coming soon to the web portal. Skip this section unless you want to use the CLI.

In your IDE, type the following, and attach a text file:

```text
Store the attached document as long-term knowledge.
<attach a document>
```

##### Use the CLI

You can also upload knowledge using the Meko CLI. See [Meko CLI](../../reference/meko-cli/) for installation instructions.

Upload documents to your datapack's knowledge base as follows:

```bash
meko datapack add_knowledge_base --name meko_default_datapack --url s3://bucket/docs/
```

<!-- TODO: Add details on portal-based document upload once the web portal is available -->

### Query your knowledge

> [!NOTE] Coming soon
>
> Document upload is coming soon to the web portal. Skip this section unless you added your knowledge using the CLI.

Once your documents are parsed, chunked, and indexed by Meko, query them through your IDE:

```text
Search the knowledge base for information about <your topic>
```

The MCP server performs semantic search across your indexed documents and returns relevant chunks. In short, Meko has automated the retrieval augmented generation (RAG) pre-processing pipeline by reading, parsing, chunking, vectorizing/indexing the documents, and by providing a semantic search capability.

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

    Using the Meko MCP server, the agent will create a conversation in your datapack and assign an agent ID, for example, `cursor_assistant`. The agent should let you know what the agent ID is; you can also ask it.

1. Start a new chat session, then type the following (use the agent ID the other agent provided):

    ```text
    Access my preferences using agent ID cursor_assistant.
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

- **Memories**: View stored memories and their extracted entities.
- **Knowledge bases**: See indexed document sources and their status.
- **Traces**: Review LLM reasoning traces and execution logs.

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