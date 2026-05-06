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
    desc="The fundamental storage primitive for multi-agent systems. One datapack per application, or one project. It is the isolation unit for agents working together. Each datapack bundles a database, the knowledge base, agent memories, collective memories, shared knowledge, MCP server, and credentials."
  >}}
  {{< card
    href="architecture/core-concepts/memory/"
    title="Memory"
    desc="Extracted memories from conversations are specific pieces of information or preferences that are explicitly stored persistently for the long term. Memories are stored as entity and relationship, powered by Meko's graph storage, also called MAGE.  Agent memories can be promoted to a shared knowledge base, so all agents can learn from each other's learnings."
  >}}
  {{< card
    href="architecture/core-concepts/knowledgebase/"
    title="Knowledge bases"
    desc="Information repository that acts as a single source of truth for agents, grounding decisions, reducing hallucinations, and ensuring consistent responses. Uses semantic search and RAG to understand query intent, not just keywords. Documents are chunked, embedded, and indexed for retrieval."
  >}}
  {{< card
    href="architecture/core-concepts/traces/"
    title="Traces"
    desc="Meko provides end-to-end visibility into every conversation, from initial prompt through LLM chain-of-thought, including memory and knowledge additions and search, the associated API and SQL calls, the queries executed, and the results retrieved."
  >}}

{{< /card-group >}}

<!--
## Four pillars of value

### Accelerate agent development

Stop stitching together data across multiple silos. Meko exposes agent-native primitives, such as "add memory," that abstract away storage complexity of agent preferences in a graph, or a vector store. A PDF can be uploaded easily in the Meko UI, in real-time by parsing, chunking, and creating the vector embeddings without you having to model the implementation or writing a complex data pre-processing pipeline. Your code works with agent concepts, not database schemas, dramatically reducing development time.

Meko is a fundamentally multi-agentic, and severless.  It enables agents to share knowledge and memories, integrates with the AI ecosystem through a single MCP server endpoint.

### Higher performance, more efficient agents

Meko's AI-tailored serverless architecture and multi-tenant design handle the variable resource utilization inherent to agentic applications. Agents consume minimal resources when idle and burst instantly when active. Meko optimizes common AI operations like parsing, chunking, and entity extraction with cost-effective models, giving you accuracy versus cost tradeoffs that generic databases cannot provide.

### Full chain-of-thought traceability

End-to-end observability shows exactly which data operations your agents performed, the latencies at each stage, and the token costs per interaction. Meko collects traces for the complete chain of thought, giving you the explainability production AI applications require. Debug performance issues and optimize costs with visibility into your data layer's actual behavior.
-->
