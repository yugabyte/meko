---
title: How Meko Fits in Your Stack
linkTitle: How Meko Fits
description: Where Meko sits in your architecture and how it deploys
weight: 4200
---

## Architecture

Meko sits between your agent SDK and the underlying database, providing a unified data layer that replaces multiple standalone systems.

<img src="/images/architecture.svg" alt="Meko Architecture Diagram" style="max-width: 100%; height: auto;" />

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
|-----------|-----------|
| Distributed database | [YugabyteDB](https://www.yugabyte.com/) |
| Vector search | [pgvector](https://github.com/pgvector/pgvector) |
| Graph memory | [mem0](https://github.com/mem0ai/mem0) |
| RAG pipeline | pg_dist_rag + [unstructured](https://github.com/Unstructured-IO/unstructured) + [LangChain](https://github.com/langchain-ai/langchain) |
| Observability | [LangFuse](https://github.com/langfuse/langfuse) |
| Inference serving | [vLLM](https://github.com/vllm-project/vllm) |
| Inference caching | [LMCache](https://github.com/LMCache/LMCache) |
