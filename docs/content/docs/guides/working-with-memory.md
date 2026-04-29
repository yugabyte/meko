---
title: Work with memory
linkTitle: Use memory
description: Add, search, and manage agent memories in Meko
weight: 4300
---
Meko's memory system gives agents persistent recall across sessions. Memories flow through a three-stage pipeline before contributing to an agent's knowledge base.

## Overview

A pipeline of activities occur to create Collective Memory and Shared Knowledge from conversations.

**1. Extraction** Memories are automatically extracted from agent conversations as they occur.

**2. Learning Identification** Extracted memories are evaluated for relevance and importance. Those deemed particularly significant for the agent's tasks are promoted to _learnings_. This identification is currently a manual process.

**3. Promotion to Collective Memory and Shared Knowledge** Users review identified learnings and selectively promote them into the Datapack's shared area, comprised of both collective memory and shared knowledge.

---
##### Knowledge Base Composition

Memories and learnings are one component of a Datapack's knowledge base. The other major component is **source data**, which includes:

- **Unstructured data** — text files, PDFs, and other documents
- **Structured data** — databases and tabular sources

---
##### Underlying Storage: The Semantic Graph

Memories and all their downstream derivatives are stored internally in a **semantic graph**. This structure supports two complementary retrieval mechanisms:

- **Graph edges** — capture entity-to-entity relationships (e.g., _John likes bananas_)
- **Document chunks and vectors** — enable semantic similarity search against graph entities and relationships

Together, these allow a memory to be retrieved not just by exact match, but by semantic relevance. For example, the memory _"John likes bananas"_ can be surfaced in response to a later query such as _"Who likes fruits?"_


## Add memories

##### Via MCP (from your coding agent)

The simplest way to add memories is through natural language in your coding agent:

```text
Please remember that I like to take vacations at tropical beach locations, on a budget
```

```text
Remember this coding preference: I prefer Python type hints and use pytest for testing
```

The MCP server calls `memory_add` behind the scenes, which uses an LLM to extract entities and relationships from your text.

## Search memories

##### Via MCP (from your coding agent)

If Meko is properly configured with your AI agent, the agent will automatically access Meko when formulating responses. To retrieve stored information, ask your agent to recall it directly.


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


### Via CLI

```bash
meko datapack search_memories --name my-datapack --user_prompt "query text"
```
--> 

##### How search works

Memory search combines two strategies:

1. **Vector similarity**. Finds memories with semantically similar embeddings.
2. **Graph traversal**. Finds related entities and relationships. For example, searching for "vacation" might also surface memories about "travel budget" through graph connections.

Results from both strategies are combined and ranked, allowing a memory to be retrieved not just by exact match, but by semantic similarity as well. For example, the memory _"John likes bananas"_ can be surfaced in response to not only the query _"who likes banana?"_ but also a semantic query like _"Who likes fruits?"_

## View all memories

To see everything Meko has remembered for a user or agent, use the `get_all` operation. This returns every stored memory, and is useful for debugging or building a memory dashboard.

<!--
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

## Example

This example demonstrates the difference memory makes.

##### Without Meko

```text
Prompt: "Propose a 5 day vacation for me."
```

The agent has no context about you. It asks generic follow-up questions: "What kind of vacation?", "Budget?", "Where?"

##### With Meko

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

- [Learn about memory](../../architecture/core-concepts/memory/)
- [Work with knowledge bases](../working-with-knowledge-bases/)
