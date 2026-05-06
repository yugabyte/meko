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

Meko is serverless, and multi-tenant (multi-agentic). In addition to providing memory, conversation history, shareable knowledge, and full chain-of-thought traceability, Meko also exposes PostgreSQL-compatible database models (vector, SQL, graph, and more).

## Architecture

Meko integrates easily with any agentic framework through MCP servers. It is built on top of a unified distributed PostgreSQL data layer that supports vector, SQL, NoSQL, graph, and search.  

Meko sits between your agent SDK and the underlying database, providing a unified data layer that replaces multiple standalone systems.

![Meko Architecture Diagram](/images/architecture.svg)

Agents interact via MCP.

### What you get

Each agent gets an isolated [datapack](../core-concepts/datapacks/) with:

| Capability | Description |
| :--------- | :---------- |
| PostgreSQL (YugabyteDB YSQL) | Structured data and conversation history |
| Vector search (pgvector, hnswlib) | Embeddings and similarity search |
| Graph memory (MAGE, a graphRAG database) | Long-term entity and relationship extraction from conversations |
| RAG pipeline<br>(pg_dist_rag, a YugabyteDB extension) | Document chunking, embedding, indexing with built-in data preprocessing |
| MCP server | Agents discover and query their own datapack |
| Audit trails | LLM reasoning traces and execution logs |
| Observability, security, RBAC | Spans the full stack, not bolted on per-component |

The memory, knowledge, and database layers share state, and they operate as one system, not three isolated tiers.

### What meko replaces

Without Meko, building an agentic application typically requires wiring together:

- A relational database for structured data
- A vector database for embeddings and similarity search
- A graph database for entity relationships
- Custom integration code for each system
- Per-component observability, security, and RBAC

Meko replaces all of this with a single system. The memory, knowledge, and database layers share state and operate as one.

### How agents connect

Agents interact with Meko via **MCP** (Model Context Protocol). Each datapack exposes an MCP server that provides tools for memory, knowledge, and more.

Developers can also interact via:

- **CLI**: `meko` commands for datapack management

### Framework compatibility

Meko works with any agent framework that supports MCP:

- CrewAI
- LangChain / LangGraph
- OpenAI Agents SDK
- Any MCP-compatible client

## Deployment

- **Cloud**: Currently, Meko is a cloud service, managed by YugabyteDB.  
