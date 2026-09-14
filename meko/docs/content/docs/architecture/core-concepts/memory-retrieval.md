---
title: Memory retrieval
linkTitle: Memory retrieval
description: How Meko ranks personal memory search results
weight: 40
aliases:
  - /architecture/core-concepts/graph-rag/
---

Meko uses hybrid retrieval to find personal memories by meaning while preserving useful exact matches.

## How it works

The [memory_search](../../../reference/mcp-server/#memory-tools) tool ranks stored memories using three signals:

1. **Semantic similarity** finds memories with related meaning.
1. **Keyword matching** gives exact terms more weight.
1. **Entity boosting** raises memories that mention the people, projects, tools, or other named things in the query.

The server combines these signals and returns the highest-ranked memories.

The response includes two score fields:

- `score` is the fused hybrid rank. Results are ordered by this field.
- `raw_similarity_score` is the cosine similarity from the vector store.

A result with a lower `raw_similarity_score` can rank above one with a higher value when its keyword or entity signals are stronger.

## Keep model context bounded

The goal is to retrieve enough candidates to cover the question, then pass only useful evidence into the model context. A broad result set can increase cost and make the answer less precise.

The Meko agent skills retrieve up to about 25 candidates and use at most the top 10 relevant items by default. Increase that reader window only when you can show that relevant evidence falls outside it.

## Distinguish failure from no results

Inspect the response before interpreting the results:

- An `error` or error-shaped `detail` means the search failed. The result is unknown.
- A successful response with an empty `results` array means Meko found no matching memories.

Don't retry quota or permission errors. Resolve the account, key, or datapack access problem first.

## Find older or exact memories

The [memory_get_all](../../../reference/mcp-server/#memory-tools) tool returns a recent window, not a complete export. Use `memory_search` for older content and `memory_get_by_id` when you know the exact memory ID.
