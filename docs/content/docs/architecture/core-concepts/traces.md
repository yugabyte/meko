---
title: Traces
linkTitle: Traces
description: Audit trails and chain-of-thought traceability for agent operations
weight: 60
---

Traces provide end-to-end observability into your agents' data operations. Meko collects traces for the complete chain of thought, giving you the explainability that production AI applications require.

## What's traced

Meko captures:

- **Data operations**. Which memory, knowledge, and database operations each agent performed.
- **Latencies**. Timing at each stage of the pipeline (embedding, search, retrieval, etc.).
- **Token costs**. Per-interaction token usage for LLM calls.
- **LLM reasoning**. The chain of thought and reasoning traces from agent interactions.
- **Execution logs**. Detailed logs of MCP tool invocations and their results.

## Chain-of-thought traceability

One of Meko's four pillars is _Full Chain-of-Thought Traceability_. This means you can:

- **Debug performance issues**. See exactly where time is spent in the data pipeline.
- **Optimize costs**. Identify which operations consume the most tokens.
- **Explain agent behavior**. Trace back from an agent's output to the data operations and reasoning that produced it.
- **Audit compliance**. Maintain a complete record of what data agents accessed and how they used it.

## Integration with LangFuse

Meko integrates with [LangFuse](https://github.com/langfuse/langfuse) for trace collection and visualization.

<!-- TODO: Add details on LangFuse integration setup and configuration -->
<!-- TODO: Document how to view traces in the LangFuse dashboard -->
<!-- TODO: Add examples of trace queries and common debugging patterns -->
<!-- TODO: Document trace retention policies and data tiering for trace data -->

## Next steps

- [Use traces for monitoring and debugging](../../../guides/observability/)
