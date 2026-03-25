---
title: Meko Documentation
headerTitle: Meko Documentation
linkTitle: Docs Home
headcontent: Simplify the Agent-Native Data Layer
description: Guides, examples, and reference material for building AI agents with persistent, portable, shareable memory and knowledge using Meko
body_class: no-toc
type: docs
layout: list
---

Meko is the agent-native data layer designed to unify agent knowledge, memory, and conversations, eliminating the impedance mismatch of traditional relational and vector databases.

*Memory, knowledge, and SQL for every AI agent with a single MCP endpoint.*

## Get Started

{{< card-group >}}
  {{< card
    href="about/"
    title="About Meko"
    desc="Learn what Meko is, its core concepts, and how it fits in your stack."
    cta="Explore &rarr;"
  >}}
  {{< card
    href="quick-start/"
    title="Quick Start"
    desc="Sign up, connect your IDE, and see Meko in action in under 5 minutes."
    cta="Get started &rarr;"
  >}}
  {{< card
    href="examples/"
    title="Examples"
    desc="See real-world scenarios: coding preferences, knowledge bases, and resumable agents."
    cta="View examples &rarr;"
  >}}
  {{< card
    href="integrations/"
    title="Integrations"
    desc="Connect Meko to AI coding tools, agent frameworks, and vibe coding platforms."
    cta="Browse integrations &rarr;"
  >}}
  {{< card
    href="guides/"
    title="Guides"
    desc="Step-by-step guides for memory, knowledge bases, database access, and observability."
    cta="Read guides &rarr;"
  >}}
  {{< card
    href="reference/"
    title="Reference"
    desc="CLI commands, API endpoints, and technical reference."
    cta="View reference &rarr;"
  >}}
{{< /card-group >}}

## Architecture Overview

![Meko Architecture Diagram](/images/architecture.svg)

Agents interact via MCP. Developers interact via CLI, API, or direct PostgreSQL connection. Meko runs anywhere --- local, cloud, on-prem, or air-gapped --- with the same config everywhere.
