---
title: Architecture
linkTitle: Architecture
description: Learn how Meko works
weight: 4500
body_class: full-width
type: docs
---

When you start building a production-ready agentic application, the data infrastructure problem quickly becomes apparent. Multi-agent applications don’t fail because the agents can't reason; they fail because they can't share what they know. The Multi-Agent System failures stem from inter-agent misalignment. Your agent needs to remember preferences, access knowledge, and store and retrieve conversation history. It needs to do all of this efficiently, at variable load, potentially across multiple concurrent agents working together.

Developers bolt together PostgreSQL for relational data, pgvector for semantic search, separate graph databases for memory, and object stores for conversations. This creates data silos, drives up costs, introduces latency from cross-system queries, and leaves you building custom logic for tiering and observability.

Meko replaces that with a single system. Once you configure your agentic SDK to run with Meko using a single MCP server endpoint, you get it all in a unified data infrastructure layer for all your agents.

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
