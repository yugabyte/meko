---
title: About Meko
linkTitle: About Meko
description: Learn what Meko is and what it can do for you
weight: 10
type: docs
---

Meko is the agent-native data layer designed to unify agent knowledge, memory, and conversations, eliminating the impedance mismatch of traditional relational and vector databases. Built from the ground up around AI data constructs, Meko lets agentic applications create datapacks that store per-agent memory and shareable knowledge, accessible through MCP servers. Instead of forcing agents to work with tables, vectors, and graphs separately, Meko exposes primitives that match how agents naturally operate. Its serverless, multi-tenant architecture automatically tiers cold data to object storage while maintaining full chain-of-thought traceability. Your agents get native persistence without the complexity of stitching together multiple database systems.

## Why Meko?

Building agentic applications with traditional databases often forces you to use and maintain a patchwork architecture. Developers bolt together PostgreSQL, pgvector for semantic search, separate graph databases for memory, and object stores for conversations. This creates data silos, drives up costs, introduces latency from cross-system queries, and leaves you building custom logic for tiering, caching, and observability.

Meko replaces that with a single system. `meko start` and you have everything.

## What You Get

Each agent gets an isolated **datapack** with:

| Capability | Description |
|-----------|-------------|
| **PostgreSQL** (YugabyteDB YSQL) | Structured data and conversation history |
| **Vector search** (pgvector, hnswlib, scaNN) | Embeddings and similarity search |
| **Graph memory** (mem0) | Long-term entity and relationship extraction from conversations |
| **RAG pipeline** (pg_dist_rag + unstructured + LangChain) | Document chunking, embedding, indexing with built-in data preprocessing |
| **Inference caching** (LMCache / KDN) | Reduces redundant LLM calls without a separate cache layer |
| **MCP server** | Agents discover and query their own datapack |
| **Audit trails** | LLM reasoning traces and execution logs |
| **Observability, security, RBAC** | Spans the full stack, not bolted on per-component |

The inference, knowledge, and database layers share state --- they operate as one system, not three isolated tiers.

## Four Pillars

### Accelerate Agent Development

Stop stitching together data across multiple silos. Meko exposes agent-native primitives, such as "add knowledge," that abstract away storage complexity. A PDF can live in object storage, in real-time updates to SQL tables, or in both, without you having to model the implementation. Your code works with agent concepts, not database schemas, dramatically reducing development time.

Meko is a fundamentally multi-agentic, open-source data layer that enables agents to share knowledge and memories, integrates with the AI ecosystem through open APIs, and is available as both a serverless managed service and for private cloud deployment.

### Higher Performance, More Efficient Agents

Meko's AI-tailored serverless architecture and multi-tenant design handle the variable resource utilization inherent to agentic applications. Agents consume minimal resources when idle and burst instantly when active. Meko optimizes common AI operations like parsing, chunking, and entity extraction with cost-effective models, giving you accuracy versus cost tradeoffs that generic databases cannot provide.

### Automatic Data Tiering Lowers Storage Costs

Conversations are fundamental to agents but expensive to store in real-time databases. Meko keeps all conversation history queryable while transparently tiering older messages to object storage. Configurable retention policies purge data beyond your needs. This applies across all data models, keeping hot data fast and cold data cheap without manual intervention.

### Full Chain-of-Thought Traceability

End-to-end observability shows exactly which data operations your agents performed, the latencies at each stage, and the token costs per interaction. Meko collects traces for the complete chain of thought, giving you the explainability production AI applications require. Debug performance issues and optimize costs with visibility into your data layer's actual behavior.

## Next

* **[Quick Start](../quick-start)** - Get up and running with Meko in minutes