---
title: Meko Documentation
headerTitle: Meko Documentation
linkTitle: Introduction
headcontent: Meko is the agent-native data layer for multi-agent systems. Agents share knowledge, build collective memory, and every step is auditable end-to-end.
description: Simplify the Agent-Native Data Layer
body_class: full-width
notoc: true
type: docs
layout: list
---

Meko is the agent-native data layer for multi-agent systems, with shared knowledge, compound memory, and end-to-end auditable traces.

Teams still fail when agents can't share state (36% of failures trace to inter-agent misalignment); Meko treats memory and knowledge as infrastructure so agent teams stay coordinated instead of collisional.

&nbsp;

**Works with**: <img class="docs-works-with-logo" src="/logos/integrations/claude.svg" alt="" width="15" height="15" decoding="async" loading="lazy" /> Claude Code | <img class="docs-works-with-logo" src="/logos/integrations/cursor.svg" alt="" width="15" height="15" decoding="async" loading="lazy" /> Cursor | <img class="docs-works-with-logo" src="/logos/integrations/codex.svg" alt="" width="15" height="15" decoding="async" loading="lazy" /> Codex | <img class="docs-works-with-logo" src="/logos/integrations/claude.svg" alt="" width="15" height="15" decoding="async" loading="lazy" /> Claude Desktop</span> | [...and more](integrations/)

#### Meko offers:

{{< card-group variant="third" >}}
  {{< card
    icon="fa-solid fa-layer-group"
    title="Collective memory"
    desc="Learning that compounds across agents, not trapped in per-agent memory silos."
  >}}
  {{< card
    icon="fa-solid fa-book-open"
    title="Shared knowledge"
    desc="A knowledge base that grows over time from conversations, real-time data sources, and slower-changing reference material."
  >}}
  {{< card
    icon="fa-solid fa-diagram-project"
    title="Decision traces"
    desc="Every output linked back to the conversation, reasoning, and execution that produced it."
  >}}
{{< /card-group >}}

&nbsp;

<img src="/images/meko-architecture-intro.svg" alt="Diagram: agents A B and C connect to MCP server, then datapack with memory knowledge traces, then YugabyteDB and S3 tier" width="800" height="534" decoding="async" loading="lazy" style="max-width: min(800px, 100%); height: auto; display: block; margin-inline: auto;" />

&nbsp;

Everything lives in a datapack, the storage primitive that holds your memory, knowledge, conversation history, and traces, and is exposed via an MCP server.

Meko integrates easily with any agentic framework through MCP servers. It is serverless, multi-tenant (multi-agentic), and optimized for tiering to object stores. Built on YugabyteDB, a distributed Postgres with vector, relational, document, graph, and full-text search in one query plane. One database, no glue code.

## Learn more about Meko

{{< card-group >}}
  {{< card
    href="quick-start/"
    title="Quick start"
    desc="Request access, connect your AI client, and start using Meko in minutes."
    cta="Request access &rarr;"
  >}}
  {{< card
    href="architecture/core-concepts/"
    title="Core concepts"
    desc="Understand datapacks, memory, knowledge, and how Meko fits your stack."
    cta="Learn more &rarr;"
  >}}
  {{< card
    href="examples/"
    title="Examples"
    desc="Real-world scenarios: handoffs, collective learning, and shared team standards."
    cta="Browse examples &rarr;"
  >}}
  {{< card
    href="integrations/"
    title="Integrations"
    desc="Wire Meko into Cursor, Claude, and other tools over MCP with the same pattern."
    cta="View integrations &rarr;"
  >}}
{{< /card-group >}}

### Start where you fit

- I'm building agents > [Quick Start](quick-start/)
- I'm running platform > [Architecture](architecture/)
- I'm assessing for audit > [Decision traces](architecture/core-concepts/traces/)


<!--
Meko is an agent-native data layer for agents that work and learn together.

Meko provides:

- Collective memory. Learning compounds across the whole system, not just per-agent.
- Shared knowledge. Knowledge is built over time from conversations, real-time feeds, and knowledge bases.
- Auditability. Meko connects execution traces to how agents learn and share that learning.
- Datapack model. Each tenant (agent or team) gets an isolated, portable datapack.

Meko is fully open-source and integrates easily with any agentic framework through MCP servers. It is built on top of a unified distributed PostgreSQL data layer that supports vector, SQL, NoSQL, graph, and search.

Meko is the agent-native data layer designed to unify agent knowledge, memory, and conversations, eliminating the impedance mismatch of traditional relational and vector databases.

*Memory, knowledge, and SQL for every AI agent with a single MCP endpoint.*-->


<!--
{{< card-group >}}
  {{< card
    href="guides/"
    title="Guides"
    desc="Step-by-step guides for memory, knowledge bases, database access, and observability."
    cta="Read guides &rarr;"
  >}}
{{< /card-group >}}

## Popular integrations

Meko is fully open-source and integrates easily with any agentic framework through MCP servers, including the following.

{{< card-group >}}
  {{< card
    href="integrations/connect-to-ai-agent/#cursor-desktop"
    title="Cursor Desktop"
    desc=""
    cta="Setup &rarr;"
  >}}
  {{< card
    href="integrations/connect-to-ai-agent/#claude-desktop"
    title="Claude Desktop"
    desc=""
    cta="Setup &rarr;"
  >}}
  {{< card
    href="integrations/connect-to-ai-agent/#claude-code"
    title="Claude Code"
    desc=""
    cta="Setup &rarr;"
  >}}
{{< /card-group >}}

[More integrations](/integrations/)

## How Meko works

Learn how Meko adds memory, knowledge, and SQL to agentic applications.

-->
