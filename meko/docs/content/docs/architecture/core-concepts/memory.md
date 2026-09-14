---
title: Memory
linkTitle: Memory
description: Long-term memory for AI agents with hybrid retrieval
weight: 20
---

Memory in Meko gives agents persistent, long-term recall of facts, preferences, and decisions extracted from conversations. Unlike conversation history, which stores raw messages, memory stores the focused facts that an LLM extracts from interactions.

## How it works

Memories are created two ways:

- **Explicitly**, by calling `memory_add` (or asking your agent to "remember this") with text you want stored.
- **Automatically**, from saved conversation turns. When a turn is saved — through automatic capture in your AI client, or an explicit `conversation_add_message` call — Meko's extractor scans it server-side for durable facts and creates memories from what it finds, with no extra tool call required.

Either way, Meko does the following with the text:

1. Uses an LLM to extract focused, durable facts.
1. Stores the facts and their vector embeddings for retrieval.
1. Indexes the text so searches can combine semantic similarity with keyword and named-entity signals.

This hybrid retrieval helps an agent find a memory by meaning while giving extra weight to exact terms and named entities in the query.

> [!NOTE] Extraction is asynchronous and best-effort
>
> Conversation-driven extraction runs after the turn is saved and isn't guaranteed to catch every fact in a turn — occasionally an individual detail can be dropped. For anything that must not be lost, explicitly ask your agent to remember it (`memory_add`) rather than relying on automatic extraction alone.

## Memory operations

Meko exposes [memory tools](../../../reference/mcp-server/#memory-tools), including the following.

### Add memory

Store a new memory from text.

```text
"I like to take vacations at tropical beach locations, on a budget"
```

Behind the scenes, this calls `memory_add`, which stores the durable preference and makes it available to later searches.

The same extraction runs automatically whenever a conversation turn is saved (see [How it works](#how-it-works) above) — you don't have to explicitly say "remember this" for a fact to become a memory, though doing so is more reliable.

### Search memories

Find relevant memories using hybrid search. A single search spans the authenticated user's agent namespaces in the selected datapack and returns the top matches ordered by `score`. The calling `agent_id` is retained for trace attribution; it doesn't filter the results.

```text
"What are my vacation preferences?"
```

This combines semantic similarity, keyword matching, and an entity boost, then orders results by `score`. The response also includes `raw_similarity_score`, the cosine similarity from the vector store. Strong keyword or entity signals can place a result above one with a higher `raw_similarity_score`. See [Memory retrieval](../memory-retrieval/).

### Get recent memories

`memory_get_all` returns a recent window of roughly 20 memories and the total count for the authenticated user in a datapack. It isn't a complete list or export. Use `memory_search` to find older memories, or `memory_get_by_id` when you know the exact memory ID.

### Clear memories

Remove memories written under an agent ID, optionally narrowed by run ID. This is a destructive operation - cleared memories cannot be recovered.

## Memory vs. conversation history

|      | Memory | Conversation History |
| :--- | :----- | :------------------- |
| **What's stored** | Extracted facts, preferences, and decisions | Raw messages (user + assistant) |
| **How it's created** | LLM extracts from conversations | Stored verbatim |
| **Search method** | Semantic, keyword, and entity-aware ranking | Chronological lookup |
| **Use case** | "Remember my preferences across sessions" | "Show me what was said in the last chat" |

## Next steps

- [Work with memory](../../../guides/working-with-memory/) - How to add, search, and inspect memories
- [Memory retrieval](../memory-retrieval/) - How Meko ranks memory search results
