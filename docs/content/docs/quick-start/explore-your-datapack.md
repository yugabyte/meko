---
title: Explore Your Datapack
linkTitle: Explore Your Datapack
description: Add memories, query knowledge, and see Meko in action
weight: 20
---

Now that you're [connected to Meko](../connect-to-meko/), let's explore what your datapack can do.

## Demo 1: Trip Planner with Memory

This demo shows how memory makes your agents dramatically better.

### Without Meko

Open your IDE and type:

```text
Propose a 5 day vacation for me.
```

Without any stored context, the agent asks generic follow-up questions: "What kind of vacation?", "Budget?", "Where?"

### With Meko

Now, connect to your Meko MCP server and type:

```text
You have access to the Meko platform through MCP server.
Have Meko remember all my personal preferences.
agent_id is cursor_agent and user_id is <your_username>
```

Then store some preferences:

```text
Please remember that I like to take vacations at tropical beach locations, on a budget
```

**Start a new chat session**, then type:

```text
Propose a 5 day vacation for me.
```

Notice how the agent retrieves your stored preferences and immediately proposes a tropical beach vacation within budget --- no follow-up questions needed.

### View Your Memories

You can verify what Meko stored by checking your datapack's memories in the Meko portal. You'll see:

- The stored memories with their extracted entities
- Key metrics: number of memories, knowledge base sources, and MCP requests

<!-- TODO: Add screenshots of the Meko portal showing datapack memories once the web portal is available -->

## Demo 2: Build a Knowledge Base

Add documents to your datapack and query them using RAG.

### Upload Knowledge

Upload documents to your datapack's knowledge base:

```bash
meko datapack add_knowledge_base --name my-datapack --url s3://bucket/docs/
```

<!-- TODO: Add details on portal-based document upload once the web portal is available -->

### Query Your Knowledge

Once your documents are indexed, query them through your IDE:

```text
Search the knowledge base for information about <your topic>
```

The MCP server performs semantic search across your indexed documents and returns relevant chunks.

## Observe Your Datapack

Check what's happening inside your datapack:

- **Memories** --- View stored memories and their extracted entities
- **Knowledge bases** --- See indexed document sources and their status
- **Traces** --- Review LLM reasoning traces and execution logs

<!-- TODO: Add LangFuse observability walkthrough once integration details are finalized -->
<!-- TODO: Add screenshots showing costs, calls, and timings -->

## Next Steps

- [Examples](../../examples/) --- See more detailed use cases
- [Working with Memory guide](../../guides/working-with-memory/) --- Deep dive into memory operations
- [Working with Knowledge Bases guide](../../guides/working-with-knowledge-bases/) --- Advanced knowledge base management
