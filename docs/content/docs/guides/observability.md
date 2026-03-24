---
title: Observability
linkTitle: Observability
description: Traces, conversation history, and chain-of-thought visibility
weight: 4600
---

Meko provides end-to-end observability into your agents' data operations, giving you the visibility production AI applications require.

## What You Can Observe

### Traces

Every data operation your agent performs is traced:

- **Memory operations** --- When memories are added, searched, or cleared
- **Knowledge base queries** --- Document retrieval operations and their results
- **Database operations** --- SQL queries and their execution times
- **MCP tool invocations** --- Each tool call and its result

### Conversation History

Meko stores the full conversation history between users and agents:

- Verbatim message logs (user + assistant)
- Timestamps and session identifiers
- S3-backed tiering for cost-effective long-term storage

### Chain of Thought

For agents that produce reasoning traces, Meko captures the complete chain of thought:

- Step-by-step reasoning logs
- Sub-agent orchestration audit trails
- Decision points and the data that informed them

## Key Metrics

For each datapack, you can view:

- Number of memories stored
- Number of knowledge base sources (or document count)
- Number of MCP requests (past day / week / month)
- Token costs per interaction
- Latency at each pipeline stage

## LangFuse Integration

Meko integrates with [LangFuse](https://github.com/langfuse/langfuse) for trace collection and visualization.

<!-- TODO: Add LangFuse setup and configuration instructions -->
<!-- TODO: Add screenshots of the LangFuse dashboard with Meko traces -->
<!-- TODO: Add documentation for trace-related MCP tools and API endpoints -->
<!-- TODO: Add guide for common debugging patterns using traces -->

## Next Steps

- [Traces concept](../../about/core-concepts/traces/) --- How tracing works under the hood
- [Working with Memory](../working-with-memory/) --- Memory operations that generate traces
