---
title: How Meko fits in your stack
linkTitle: How Meko fits
description: Where Meko sits in your architecture and how it deploys
weight: 10
---

Meko is the agent-native data infrastructure that enables multi-agent systems to learn together, building collective memory and shared knowledge that compounds across the entire system.

It supports:

- **Collective memory**. Learning compounds across the entire system, not just per-agent memory.
- **Shared knowledge**. Meko builds knowledge over time from conversations, real-time data sources, and slower-changing knowledge bases.
- **Decision Traces**. Connect raw conversations to execution traces of how LLMs think, agents process memory and knowledge, learn from it, and share that learning.

Meko is serverless, and multi-tenant (multi-agentic). It provides memory, conversation history, shareable knowledge, and full chain-of-thought traceability.

## Architecture

Meko integrates with any agentic framework through a standard MCP interface. It is built on top of a unified distributed PostgreSQL data layer that supports vector, SQL, NoSQL, graph, and search.

Meko sits between your agent SDK and the underlying storage, providing a unified data layer that replaces multiple standalone systems.

![Meko Architecture Diagram](/images/architecture.svg)

### What you get

A [datapack](../core-concepts/datapacks/) is the isolation unit for agent data in Meko. Datapacks can be shared by many users and connected to many agents. A datapack incorporates the following:

| Capability | Powered by (internal) | API |
| :--------- | :-------------------- | :--- |
| Conversation history | PostgreSQL (YugabyteDB YSQL) | `conversation_*` tools |
| Memory | Graph memory + vector search (pgvector, hnswlib) | `memory_*` tools |
| Knowledge base | RAG pipeline (pg_dist_rag) + vector search | `knowledgebase_*` tools |
| Decision traces | LLM reasoning traces and execution logs | Observability dashboard |
| Observability, security, RBAC | Spans the full stack, not bolted on per-component | — |

![Meko Datapack Internals Diagram](/images/meko-datapack-internals.svg)

The underlying storage technologies are managed internally and not directly accessible.

### What Meko replaces

Without Meko, building an agentic application typically requires wiring together:

- A relational database for structured data
- A vector database for embeddings and similarity search
- A graph database for entity relationships
- Per-database observability, security, and RBAC
- Custom integration code for stitching together search results from each database


![What Meko Replaces Diagram](/images/meko-what-replaces.svg)

Meko replaces all of this with a single system. The memory, knowledge, and conversation layers share state and operate as one.

### Framework compatibility

Meko works with any agentic framework, including:

- CrewAI
- LangChain / LangGraph
- OpenAI Agents SDK
- Any MCP-compatible client

## Deployment

- **Cloud**: Currently, Meko is a cloud service, managed by YugabyteDB.
