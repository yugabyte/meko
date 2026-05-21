---
title: Learnings
linkTitle: Learnings
description: How per-agent memory becomes institutional knowledge through review and promotion
weight: 25
---

A _learning_ is a candidate piece of per-agent memory that has been surfaced for human review and possible promotion to the institutional zone of a datapack. Learnings are the bridge between what an individual agent has learned in its own conversations and what every agent in the datapack can read.

## The promotion flow

Meko separates what an agent learns from itself from what the wider system trusts. Memory starts private and per-agent. Promoted content becomes available to every agent in the datapack.

![Memory promotion flow: Conversations to Memories to Learnings to Collective Memory and Shared Knowledge](/images/promotion-flow.svg)

The pipeline has three stages.

### 1. Extraction

As an agent has conversations, Meko extracts entities, relationships, and facts and stores them as [memories](../memory/) scoped to that agent. The agent owns this memory; no other agent in the datapack can read it.

### 2. Learning identification

A subset of memories is flagged as a _learning_, meaning it is a candidate for sharing across agents. Today this identification is a manual step performed in the Meko UI; the **Learnings** tab surfaces candidates and shows whether each candidate has already been promoted. Future releases will introduce automated identification heuristics.

### 3. Promotion

A reviewer promotes a learning into one or both of the institutional destinations:

- **Collective Memory** &mdash; entity and relationship content that joins the shared graph and vector store. Other agents retrieve it the same way they retrieve their own memory: through semantic and graph search.
- **Shared Knowledge** &mdash; document-style or factual content that joins the [knowledge base](../knowledgebase/) and is retrieved through RAG.

Both destinations sit inside the same datapack, so promoted content is bounded by the datapack's tenancy. A learning is never shared across datapacks.

## Per-agent versus institutional

| Zone | Stages | Who can read it |
| :--- | :----- | :-------------- |
| Per-agent | Conversations, Memories | Only the originating agent |
| Review | Learnings | Surfaced to human reviewers in the Meko UI |
| Institutional | Collective Memory, Shared Knowledge | Every agent in the datapack |

This separation is what makes Meko's collective memory model auditable. Every promotion is an explicit decision by a reviewer, captured in the [trace](../traces/) for that datapack.

## Why two institutional destinations

Collective Memory and Shared Knowledge serve different retrieval patterns:

- A learning like _"Customers in the Northeast region prefer Tuesday delivery windows"_ is best stored as Collective Memory, because the retrieval value comes from graph traversal and entity similarity.
- A learning like _"The 2026 Q1 incident response playbook supersedes the 2025 version"_ is best stored as Shared Knowledge, because the retrieval value comes from chunked semantic search over a document corpus.

Reviewers choose the destination at promotion time. A learning can be promoted to both.

## Next steps

- [Memory](../memory/) &mdash; how per-agent memory is extracted and stored
- [Knowledge bases](../knowledgebase/) &mdash; how Shared Knowledge is indexed and queried
- [Work with memory](../../../guides/working-with-memory/) &mdash; add, search, and list per-agent memories over MCP; use the **Learnings** flow in the Meko UI for identification and promotion
