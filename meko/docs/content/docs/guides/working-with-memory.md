---
title: Work with memory
linkTitle: Use memory
description: Add, search, and list agent memories in Meko through MCP
weight: 4300
---
Meko's memory system gives agents persistent recall across sessions.

This guide covers memory operations you drive from a coding agent over MCP: add a memory, search for relevant memories, and inspect recently stored memories.

Meko can also move memories into shared, institutional knowledge through a separate review flow. That lifecycle, including how candidates are identified and promoted from **Learnings** in the Meko UI, is described in [Learnings](../../architecture/core-concepts/learnings/). For information on how extraction and hybrid retrieval work, see [Memory](../../architecture/core-concepts/memory/).

> [!NOTE] Knowledge base composition
>
> Private memories are one part of a datapack's knowledge base. The other major component is source data, which includes unstructured files (text, PDFs, documents) and structured sources (databases, tabular data). See [Knowledge bases](../../architecture/core-concepts/knowledgebase/).

> [!NOTE] Memory retrieval
>
> Memory search combines semantic similarity, keyword matching, and entity-aware ranking. It returns memory results without graph relations.

## Add memories

### Via MCP (from your coding agent)

The simplest way to add memories is through natural language in your coding agent:

```text
Please remember that I like to take vacations at tropical beach locations, on a budget
```

```text
Remember this coding preference: I prefer Python type hints and use pytest for testing
```

The MCP server calls `memory_add` behind the scenes and stores the durable fact for later retrieval.

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

`memory_search` spans the current user's agent namespaces in the selected datapack. For example, a search from Claude Code can find a relevant memory written from Cursor or Claude Desktop. The `agent_id` sent with the search is used for trace attribution; it doesn't filter the results. Agents should make one search rather than repeating it for every known agent ID.

> [!WARNING] Retrieval size and LLM context
>
> Retrieving all memories or running a broad memory search can add substantial content to your LLM context. Set `memory_search.limit` to return fewer search results when appropriate. See the [MCP memory tools reference](../../reference/mcp-server/#memory-tools) for details.

<!-- 
### Via CLI

```bash
meko datapack search_memories --name my-datapack --user_prompt "vacation preferences"
```
--> 

### How search works

Memory search combines three signals:

1. **Semantic similarity** finds memories with related meaning.
1. **Keyword matching** gives exact terms more weight.
1. **Entity boosting** raises memories that mention the named people, projects, or tools in the query.

Meko combines and ranks these signals. For example, the memory _"John likes bananas"_ can match the exact query _"Who likes bananas?"_ and the related query _"Who likes fruit?"_

## View recent memories

To inspect your recent memories in a datapack, ask your coding agent in plain language. For example:

```text
Show my recent Meko memories in the default datapack.
```

`memory_get_all` returns roughly 20 recent rows and a `total` count across your agent namespaces. It isn't a complete list, and MCP doesn't currently provide a complete memory export. Use `memory_search` to find older memories or `memory_get_by_id` when you know an exact ID.

Don't call `memory_get_all` repeatedly or add pagination arguments. The tool returns the same recent window and rejects unsupported pagination fields.

## Promote memories to shared knowledge

Datapack owners and maintainers can use `memory_promote` to move private memories into the datapack's shared knowledge base and Collective Memory. Promotion removes the selected records from private memory and can't be reversed through the MCP tool.

Before promoting, your agent should:

1. Retrieve and show the exact memories it will promote.
1. Explain that datapack members will be able to find the promoted knowledge and that the private records will be removed.
1. Ask for explicit confirmation.
1. Call `memory_promote` only after you confirm.

For the complete shared-knowledge lifecycle, see [Learnings](../../architecture/core-concepts/learnings/).

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
