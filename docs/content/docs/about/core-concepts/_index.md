---
title: Core Concepts
linkTitle: Core Concepts
description: Understand the foundational building blocks of Meko
weight: 20
---

Meko organizes agent data around a few key concepts. Understanding these will help you get the most out of the platform.

- **[Datapacks](datapacks/)** --- The isolation unit for agent data. Each datapack bundles a database, MCP server, and credentials.
- **[Memory](memory/)** --- Long-term entity and relationship extraction from conversations, powered by mem0 and graph storage.
- **[Knowledge Bases](knowledgebase/)** --- RAG pipelines that chunk, embed, and index your documents for semantic search.
- **[Graph RAG](graph-rag/)** --- Graph-based memory using Apache AGE for entity-relationship queries.
- **[OLTP Data](oltp-data/)** --- Direct PostgreSQL access via YugabyteDB YSQL for structured data.
- **[Traces](traces/)** --- Audit trails and chain-of-thought traceability for agent operations.
- **[Connectors](connectors/)** --- Data source connectors for pg_dist_rag.
