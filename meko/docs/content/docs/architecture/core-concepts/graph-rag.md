---
title: Memory retrieval
linkTitle: Memory retrieval
description: How Meko ranks personal memory search results
weight: 40
url: /architecture/core-concepts/memory-retrieval/
aliases:
  - /architecture/core-concepts/graph-rag/
---

Meko uses hybrid retrieval to find personal memories by meaning while preserving useful exact matches.

## How it works

`memory_search` ranks stored memories using three signals:

1. **Semantic similarity** finds memories with related meaning.
1. **Keyword matching** gives exact terms more weight.
1. **Entity boosting** raises memories that mention the people, projects, tools, or other named things in the query.

The server combines these signals and returns the highest-ranked results. `memory_search` doesn't return graph relations.

## Keep model context bounded

The goal is to retrieve enough candidates to cover the question, then pass only useful evidence into the model context. A broad result set can increase cost and make the answer less precise.

The Meko agent skills retrieve up to about 25 candidates and use at most the top 10 relevant items by default. Increase that reader window only when you can show that relevant evidence falls outside it.

## Distinguish failure from no results

Inspect the response before interpreting the results:

- An `error` or error-shaped `detail` means the search failed. The result is unknown.
- A successful response with an empty `results` array means Meko found no matching memories.

Don't retry quota or permission errors. Resolve the account, key, or datapack access problem first.

## Find older or exact memories

`memory_get_all` returns a recent window, not a complete export. Use `memory_search` for older content and `memory_get_by_id` when you know the exact memory ID.
