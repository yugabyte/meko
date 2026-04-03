---
title: Core concepts
linkTitle: Core concepts
description: Understand the foundational building blocks of Meko
weight: 50
---

Meko organizes agent data around a few key concepts. Understanding these will help you get the most out of the platform.

{{< card-group >}}
  {{< card
    href="architecture/core-concepts/datapacks/"
    title="Datapacks"
    desc="The isolation unit for agent data. Each datapack bundles a database, MCP server, and credentials."
  >}}
  {{< card
    href="architecture/core-concepts/memory/"
    title="Memory"
    desc="Long-term entity and relationship extraction from conversations, powered by mem0 and graph storage."
  >}}
  {{< card
    href="architecture/core-concepts/knowledgebase/"
    title="Knowledge bases"
    desc="RAG pipelines that chunk, embed, and index your documents for semantic search.."
  >}}
  {{< card
    href="architecture/core-concepts/graph-rag/"
    title="Graph RAG"
    desc="Graph-based memory using Apache AGE for entity-relationship queries."
  >}}
  {{< card
    href="architecture/core-concepts/oltp-data/"
    title="OLTP data"
    desc="Direct PostgreSQL access via YugabyteDB YSQL for structured data."
  >}}
  {{< card
    href="architecture/core-concepts/traces/"
    title="Traces"
    desc="Audit trails and chain-of-thought traceability for agent operations."
  >}}
  {{< card
    href="architecture/core-concepts/connectors/"
    title="Connectors"
    desc="Data source connectors for pg_dist_rag."
  >}}
{{< /card-group >}}

<!--
## Four pillars of value

### Accelerate agent development

Stop stitching together data across multiple silos. Meko exposes agent-native primitives, such as "add knowledge," that abstract away storage complexity. A PDF can live in object storage, in real-time updates to SQL tables, or in both, without you having to model the implementation. Your code works with agent concepts, not database schemas, dramatically reducing development time.

Meko is a fundamentally multi-agentic, open-source data layer that enables agents to share knowledge and memories, integrates with the AI ecosystem through open APIs, and is available as both a serverless managed service and for private cloud deployment.

### Higher performance, more efficient agents

Meko's AI-tailored serverless architecture and multi-tenant design handle the variable resource utilization inherent to agentic applications. Agents consume minimal resources when idle and burst instantly when active. Meko optimizes common AI operations like parsing, chunking, and entity extraction with cost-effective models, giving you accuracy versus cost tradeoffs that generic databases cannot provide.

### Automatic data tiering lowers storage costs

Conversations are fundamental to agents but expensive to store in real-time databases. Meko keeps all conversation history queryable while transparently tiering older messages to object storage. Configurable retention policies purge data beyond your needs. This applies across all data models, keeping hot data fast and cold data cheap without manual intervention.

### Full chain-of-thought traceability

End-to-end observability shows exactly which data operations your agents performed, the latencies at each stage, and the token costs per interaction. Meko collects traces for the complete chain of thought, giving you the explainability production AI applications require. Debug performance issues and optimize costs with visibility into your data layer's actual behavior.
-->
