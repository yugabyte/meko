---
title: Work with memory
linkTitle: Use memory
description: Add, search, and manage agent memories in Meko
weight: 4300
---

Meko's memory system gives your agents persistent recall across sessions. Memories are automatically extracted from conversations and stored as both vector embeddings (for semantic search) and graph edges (for relationship traversal).

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

### Via CLI

```bash
meko datapack search_memories --name my-datapack --user_prompt "query text"
```

## Search memories

### Via MCP (from your coding agent)

Ask your agent to recall information:

```text
What are my vacation preferences?
```

```text
What coding standards have I set?
```

The MCP server performs a semantic similarity search across your memories and returns the most relevant results with relevance scores.

### Via CLI

```bash
meko datapack search_memories --name my-datapack --user_prompt "vacation preferences"
```

### How search works

Memory search combines two strategies:

1. **Vector similarity** (pgvector). Finds memories with semantically similar embeddings. Returns results ranked by cosine similarity score.
2. **Graph traversal** (Apache AGE). Finds related entities and relationships. For example, searching for "vacation" might also surface memories about "travel budget" through graph connections.

Results from both strategies are combined and ranked, giving you comprehensive recall.

## View all memories

To see everything Meko has remembered for a user or agent, use the `get_all` operation. This returns every stored memory, and is useful for debugging or building a memory dashboard.

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

## Example: Trip planner with memory

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

## Next steps

- [Learn about memory](../../about/core-concepts/memory/)
- [Work with knowledge bases](../working-with-knowledge-bases/)
