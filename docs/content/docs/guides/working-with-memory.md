---
title: Work with memory
linkTitle: Use memory
description: Add, search, and list agent memories in Meko through MCP
weight: 4300
---
Meko's memory system gives agents persistent recall across sessions.

This guide covers per-agent memory operations you drive from a coding agent over MCP: add a memory, search for relevant memories, and list everything stored for a user or agent.

Meko can also move memories into shared, institutional knowledge through a separate review flow (learnings, collective memory, and shared knowledge). That lifecycle, including how candidates are identified and promoted from **Learnings** in the Meko UI, is described in [Learnings](../../architecture/core-concepts/learnings/). For how extraction and dual storage (vectors + graph) work under the hood, see [Memory](../../architecture/core-concepts/memory/).

> [!NOTE] Knowledge base composition
>
> Private memories are one part of a datapack's knowledge base. The other major component is source data, which includes unstructured files (text, PDFs, documents) and structured sources (databases, tabular data). See [Knowledge bases](../../architecture/core-concepts/knowledgebase/).

> [!NOTE] Underlying storage
>
> Memories are stored in a semantic graph with graph edges for entity relationships (for example, _John_ likes _bananas_) and document chunks with vectors for similarity search. That combination supports retrieval by relationship and by meaning, not only by exact text match.

## Add memories

### Via MCP (from your coding agent)

The simplest way to add memories is through natural language in your coding agent:

```text
Please remember that I like to take vacations at tropical beach locations, on a budget
```

```text
Remember this coding preference: I prefer Python type hints and use pytest for testing
```

The MCP server calls `memory_add` behind the scenes, which uses an LLM to extract entities and relationships from your text.

### Automatically, from saved conversations

You don't have to explicitly ask your agent to "remember" something for it to become a memory. If your client has automatic capture enabled (the [Automated install](../../quick-start/#connect-your-ai-client)) or your agent otherwise saves the turn with `conversation_add_message`, Meko scans the saved turn server-side and creates memories from the durable facts it finds — no extra step required.

> [!NOTE] Extraction is best-effort
>
> Conversation-driven extraction runs asynchronously after a turn is saved, and can occasionally miss an individual fact within a longer turn. For anything that must not be lost, explicitly say "remember this" (`memory_add`) rather than relying on automatic extraction alone.

## Search memories

### Via MCP (from your coding agent)

With Meko connected through MCP, your agent can retrieve memories on its own when answering questions that clearly depend on stored preferences or facts. You do not have to name a tool for that to happen. When you want to inspect or force a lookup, for example, to confirm what was saved, ask in natural language, as in the following examples.

Example 1:

```text
What are my vacation preferences?
```

Example 2:

```text
What coding standards have I set?
```

The MCP server performs a semantic similarity search across your memories and returns the most relevant results with relevance scores.

<!-- 
### Via CLI

```bash
meko datapack search_memories --name my-datapack --user_prompt "vacation preferences"
```
--> 

### How search works

Memory search combines two strategies:

1. **Vector similarity**. Finds memories with semantically similar embeddings.
2. **Graph traversal**. Finds related entities and relationships. For example, searching for "vacation" might also surface memories about "travel budget" through graph connections.

Results from both strategies are combined and ranked, allowing a memory to be retrieved not just by exact match, but by semantic similarity as well. For example, the memory _"John likes bananas"_ can be surfaced in response to not only the query _"who likes banana?"_ but also a semantic query like _"Who likes fruits?"_

## View all memories

To see every memory Meko has stored for a user or agent, ask your coding agent in plain language. For example:

```text
List every memory Meko has for me in my default datapack.
```

```text
Show all stored memories for my cursor agent — I want the complete list, not just the top matches.
```

You should see the agent use the [MCP tool](../../reference/mcp-server/#memory-tools) `memory_get_all` to return the full list (not ranked like search), which is useful for auditing or building a memory dashboard.

<!-- These are CLI actions not exposed for now
## Clear memories

To remove all memories for a user or agent:

```bash
meko datapack clear_memories --name my-datapack
```

{{% pageinfo color="warning" %}}
Clearing memories is a destructive operation. Cleared memories cannot be recovered.
{{% /pageinfo %}}

## Compress memories

Over time, memories can accumulate redundant or overlapping information. Use compression to consolidate:

```bash
meko datapack compress_memories --name my-datapack
```
--> 

<!-- The following example is already used in Quick Start/Explore
## Example

This example demonstrates the difference memory makes.

### Without Meko

```text
Prompt: "Propose a 5 day vacation for me."
```

The agent has no context about you. It asks generic follow-up questions: "What kind of vacation?", "Budget?", "Where?"

### With Meko

First, store your preferences:

```text
Please remember that I like to take vacations at tropical beach locations, on a budget
```

Start a _new session_, then:

```text
Prompt: "Propose a 5 day vacation for me."
```

The agent retrieves your stored preferences and immediately proposes a tropical beach vacation within budget; no follow-up questions are needed.
-->

## Next steps

- [Learn about memory](../../architecture/core-concepts/memory/)
- [Learnings](../../architecture/core-concepts/learnings/) — review, identification, and promotion to shared knowledge
- [Work with knowledge bases](../working-with-knowledge-bases/)
