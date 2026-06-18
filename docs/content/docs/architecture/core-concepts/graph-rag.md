---
title: Graph RAG
linkTitle: Graph RAG
description: Graph-based memory using Meko AGE for entity-relationship queries
weight: 40
---

**Graph RAG** in Meko uses Meko Age (MAGE), a graph database for graphRAG purposes, to store and query entity-relationship data extracted from agent conversations. This complements the vector-based memory by providing structured relationship traversal.

## How it works

When memories are added, Meko's graphRAG integration extracts entities and their relationships:

1. An LLM analyzes the text to identify **entities** (people, places, concepts, preferences) and **relationships** between them.
2. Entities become **nodes** in the graph, stored in Meko AGE.
3. Relationships become **edges** connecting those nodes.
4. A similarity threshold (default 0.7) prevents duplicate nodes for semantically equivalent entities.

For example, the memory *"I like tropical beach vacations on a budget"* might create:

- Nodes: **User**, **Tropical Beaches**, **Budget Travel**
- Edges: **User --prefers--> Tropical Beaches**, **User --prefers--> Budget Travel**

## Why graph + vector

Vector search alone finds memories that are semantically similar to a query. Graph traversal adds the ability to:

- **Follow relationships**: "What else does this user prefer?" can traverse edges from the user node.
- **Discover indirect connections**: Two memories might not be similar by embedding, but connected through shared entities.
- **Provide structured context**: Graph edges carry relationship types, giving the LLM more structured context than raw text similarity.

Meko's memory search combines results from both pgvector (semantic) and Meko AGE (graph) to provide comprehensive recall.

## Configuration

Under the hood, the graph store is configured with connection details to the Meko AGE instance and a similarity threshold for node matching. (These are set by Meko and are not user configurable.)

| Setting | Description |
| :------ | :---------- |
| `graph_name` | Name of the graph in Meko AGE. This is set automatically per deployment |
| `threshold` | Similarity threshold for node matching, set to 0.7 (on a scale of 0.0 to 1.0) |

<!-- TODO: Add deeper technical details on graph schema, query patterns, and tuning the similarity threshold -->
<!-- TODO: Document how to query the graph directly via PostgreSQL/AGE SQL extensions -->
