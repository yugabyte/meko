---
title: Learnings
linkTitle: Learnings
description: How private user memory becomes institutional knowledge through review and promotion
weight: 25
---

A _learning_ is a candidate piece of private memory that has been surfaced for human review and possible promotion to the institutional zone of a datapack. Learnings are the bridge between what a user's agents have learned in their conversations and what every datapack member's agents can read.

## The promotion flow

Meko separates what a user's agents learn from what the wider team trusts. Memory starts private to the authenticated user, with the writing agent recorded for attribution. Promoted content becomes available to every member of the datapack.

![Memory promotion flow: Conversations to Memories to Learnings to Collective Memory and Shared Knowledge](/images/promotion-flow.svg)

The pipeline has three stages.

### 1. Extraction

As an agent has conversations, Meko extracts durable facts and stores them as [memories](../memory/) tagged with the writing agent. The authenticated user's other clients can retrieve those memories using `memory_search`; other datapack members can't read them until they are promoted. `memory_get_all` provides a recent window for review, not a complete export.

### 2. Learning identification

Meko summarizes the datapack's knowledge base and identifies the memories that are semantically closest to the knowledge base summary. These memories are then ranked, and the top 5 results are returned as _learnings_, meaning they are candidates for sharing across agents. The **Learnings Summary** lists the candidates and shows whether each candidate has already been promoted.

**Coming soon**: You will be able to tune the thresholds for identifying learnings from memories.

### 3. Promotion

A reviewer promotes a learning into one or both of the institutional destinations:

- **Collective Memory** &mdash; promoted memory content that becomes available to every datapack member via the shared knowledge layer.
- **Shared Knowledge** &mdash; document-style or factual content that joins the [knowledge base](../knowledgebase/) and is retrieved through RAG.

Both destinations sit inside the same datapack, so promoted content is bounded by the datapack's tenancy. A learning is never shared across datapacks.

Typically, reviewers manually promote learnings via the **Learnings** tab, but an agent with owner or maintainer access can also promote memories directly using the [memory_promote](../../../reference/mcp-server/#memory-tools) MCP tool. This is useful when the reviewing agent already knows which memories are worth sharing.

## Private versus institutional

| Zone | Stages | Who can read it |
| :--- | :----- | :-------------- |
| Private | Conversations, Memories | The authenticated user's agents; individual conversation fetches remain tied to the originating agent |
| Review | Learnings | Surfaced to human reviewers in the Meko UI |
| Institutional | Collective Memory, Shared Knowledge | Every member's agents in the datapack |

This separation is what makes Meko's collective memory model auditable. Every promotion is an explicit decision by a reviewer, captured in the [trace](../traces/) for that datapack.

## Why two institutional destinations

Collective Memory and Shared Knowledge serve different retrieval patterns:

- A learning like _"Customers in the Northeast region prefer Tuesday delivery windows"_ is best stored as Collective Memory because it is a concise fact that agents should retrieve directly.
- A learning like _"The 2026 Q1 incident response playbook supersedes the 2025 version"_ is best stored as Shared Knowledge, because the retrieval value comes from chunked semantic search over a document corpus.

Reviewers choose the destination at promotion time. A learning can be promoted to both.

## Next steps

- [Memory](../memory/) &mdash; how per-agent memory is extracted and stored
- [Knowledge bases](../knowledgebase/) &mdash; how Shared Knowledge is indexed and queried
- [Work with memory](../../../guides/working-with-memory/) &mdash; add, search, list, and promote memories over MCP
