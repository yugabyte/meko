---
title: Architecture
linkTitle: Architecture
description: Learn how Meko works
weight: 4500
body_class: full-width
type: docs
---

Building agentic applications with traditional databases often forces you to use and maintain a patchwork architecture. Developers bolt together PostgreSQL, pgvector for semantic search, separate graph databases for memory, and object stores for conversations. This creates data silos, drives up costs, introduces latency from cross-system queries, and leaves you building custom logic for tiering, caching, and observability.

Meko replaces that with a single system.  Once you configure your agentic sdk to run with  Meko with an MCP server, you get it all in a unified data infrastructure layer for all your agents.

Use the following sections to understand the foundational building blocks of Meko, where Meko sits in your architecture, and how it deploys.

{{< card-group >}}
  {{< card
    href="architecture/how-meko-fits/"
    title="How Meko fits"
    desc="Where Meko sits in your architecture and how it deploys."
    cta="Learn more &rarr;"
  >}}
  {{< card
    href="architecture/core-concepts/"
    title="Core concepts"
    desc="Understand the foundational building blocks of Meko."
    cta="Learn more &rarr;"
  >}}
{{< /card-group >}}
