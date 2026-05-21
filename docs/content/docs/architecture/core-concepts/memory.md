---
title: Memory
linkTitle: Memory
description: Long-term memory for AI agents using mem0 and graph storage
weight: 20
---

Memory in Meko gives agents persistent, long-term recall of facts, preferences, and relationships extracted from conversations. Unlike conversation history (which stores raw messages), memory stores *derived knowledge* - the key facts and entities that an LLM extracts from interactions.

## How it works

When you add a memory, Meko does the following:

1. Extracts entities and relationships from the text using an LLM.
1. Stores vector embeddings in the vector store for semantic similarity search.
1. Stores graph relationships in Meko AGE for entity-relationship queries.

This dual storage means memories can be found both by semantic similarity ("find memories about vacation preferences") and by graph traversal ("what entities are related to this user?").

## Memory operations

Meko exposes four core memory operations.

### Add memory

Store a new memory from text. The LLM extracts entities and relationships automatically from agent conversations.

```text
"I like to take vacations at tropical beach locations, on a budget"
```

Behind the scenes, this calls `memory.add()` which extracts entities (user, tropical beaches, budget travel) and stores both vector embeddings and graph edges.

### Search memories

Find relevant memories using semantic similarity search. Returns the top matches with relevance scores.

```text
"What are my vacation preferences?"
```

This searches both the vector store (pgvector) and the graph store (Meko AGE), combining results for comprehensive recall.

### Get all memories

Retrieve every memory stored for a given user or agent. Useful for debugging or displaying a memory dashboard.

### Clear memories

Remove all memories for a user or agent. This is a destructive operation - cleared memories cannot be recovered.

## Memory vs. conversation history

|      | Memory | Conversation History |
| :--- | :----- | :------------------- |
| **What's stored** | Extracted facts and relationships | Raw messages (user + assistant) |
| **How it's created** | LLM extracts from conversations | Stored verbatim |
| **Search method** | Semantic similarity + graph traversal | Chronological lookup |
| **Use case** | "Remember my preferences across sessions" | "Show me what was said in the last chat" |

## Next steps

- [Work with memory](../../../guides/working-with-memory/) - How to add, search, and list memories
- [Graph RAG](../graph-rag/) - How Apache AGE powers entity-relationship memory
