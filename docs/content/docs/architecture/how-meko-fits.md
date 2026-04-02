---
title: How Meko fits in your stack
linkTitle: How Meko fits
description: Where Meko sits in your architecture and how it deploys
weight: 10
---

Meko is an agent-native data layer for multi-agent systems that work and learn together. It supports:

- **Collective memory**. Learning compounds across the entire system, not just per-agent memory.
- **Shared knowledge**. Meko builds knowledge over time from conversations, real-time data sources, and slower-changing knowledge bases.
- **Auditability of the learning process**. Connect execution traces to how agents process data and knowledge, learn from it, and share that learning.

Meko is serverless, multi-tenant (multi-agentic), and optimized for tiering to object stores. In addition to providing memory, conversation history, shareable knowledge, and full chain-of-thought traceability, Meko also exposes PostgreSQL-compatible database models (vector, SQL, NoSQL, graph, and more).

## Architecture

Meko is fully open-source and integrates easily with any agentic framework through MCP servers. It is built on top of a unified distributed PostgreSQL data layer that supports vector, SQL, NoSQL, graph, and search.  

Meko sits between your agent SDK and the underlying database, providing a unified data layer that replaces multiple standalone systems.

![Meko Architecture Diagram](/images/architecture.svg)

Agents interact via MCP. Developers interact via CLI, API, or direct PostgreSQL connection. Meko runs anywhere - local, cloud, on-premises, or air-gapped - with the same configuration everywhere.

### What Meko Replaces

Without Meko, building an agentic application typically requires wiring together:

- A relational database for structured data
- A vector database for embeddings and similarity search
- A graph database for entity relationships
- An object store for conversations
- A separate cache layer for inference
- Custom integration code for each system
- Per-component observability, security, and RBAC

Meko replaces all of this with a single system. The inference, knowledge, and database layers share state and operate as one.

### How Agents Connect

Agents interact with Meko via **MCP** (Model Context Protocol). Each datapack exposes an MCP server that provides tools for memory, knowledge, database access, and more.

Developers can also interact via:

- **CLI** --- `meko` commands for datapack management, memory operations, and knowledge base administration.
- **REST API** --- Programmatic access to all datapack operations.
- **Direct PostgreSQL** --- Connect to your datapack's YugabyteDB YSQL database with standard PostgreSQL tools.

### Framework Compatibility

Meko works with any agent framework that supports MCP:

- CrewAI
- LangChain / LangGraph
- OpenAI Agents SDK
- Any MCP-compatible client

## Deployment

Meko runs anywhere --- local, cloud, on-prem, air-gapped. The same configuration works everywhere, with no architectural rewrites between development and production.

- **Local development** --- `meko start` to run everything on your machine.
- **Cloud** --- Deploy as a managed service or self-hosted on any cloud provider.
- **On-prem / air-gapped** --- Same system, no external dependencies required.

Scale out by adding YugabyteDB nodes; scale down by removing them --- with no downtime.

## Built On

Meko is built on proven open-source technologies:

| Component | Technology |
| :-------- | :--------- |
| Distributed database | [YugabyteDB](https://www.yugabyte.com/) |
| Vector search | [pgvector](https://github.com/pgvector/pgvector) |
| Graph memory | [mem0](https://github.com/mem0ai/mem0) |
| RAG pipeline | pg_dist_rag + [unstructured](https://github.com/Unstructured-IO/unstructured) + [LangChain](https://github.com/langchain-ai/langchain) |
| Observability | [LangFuse](https://github.com/langfuse/langfuse) |
| Inference serving | [vLLM](https://github.com/vllm-project/vllm) |
| Inference caching | [LMCache](https://github.com/LMCache/LMCache) |
