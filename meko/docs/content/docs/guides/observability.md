---
title: Observability
linkTitle: Observability
description: Where to find traces, decision logs, and conversation history in the Meko portal
weight: 4600
---

Meko records what your agents did, including MCP tool calls, memory and knowledge lookups, LLM reasoning, and the conversations they stored. This guide shows where to review that history in the [Meko portal](https://cloud.mekodata.ai/).

For the concepts behind traces, see [Traces](../../architecture/core-concepts/traces/).

## Observe full Langfuse traces

The portal's **Observe** section (left sidebar) embeds [Langfuse](https://langfuse.com/) for end-to-end trace review.

To review traces:

1. Click **Observe** in the left sidebar.

    This displays your Langfuse **organization**, scoped to your Meko account. Each datapack has its own Langfuse **project**.

1. Select a datapack project to browse its **traces**. There is one trace per stored conversation session.

Inside a trace you can inspect:

- User and assistant messages for the session
- MCP tool invocations (`memory_*`, `knowledgebase_*`, `conversation_*`, and other published tools) and their inputs/outputs
- LLM spans with token usage and latency
- Nested observations for multi-step agent runs

This is the full-fidelity view. Use it when you need span-level timing, token costs, or raw tool payloads.

> [!NOTE] Pro
>
> **Decision traces** in the portal (WorkbenchLM, Conversations viewer) are available on Free. **Audit log export** and enterprise compliance features require [Pro](https://mekodata.ai/pricing/).

## Key metrics
### Deep-link from a conversation

You can also access traces directly from conversations, memories, and knowledge:

- **Datapack > Conversations**: Open a conversation, then click **Observe** in the thread header to jump to that session's trace in Observe.
- **WorkbenchLM**: Under any WorkbenchLM message, click **View execution trace** to display the **Message trace** panel. In the panel, click **View Detailed Trace** to open the same session in Observe (new tab).
- **Memories and promoted knowledge**: Rows that originated from a traced session include an **Observe** button that deep-links to the source trace.

Deep links use query parameters (`?project=<langfuse_project_id>&session=<conversation_id>`) so Observe opens directly on the right trace.

## Datapack Conversations — thread-level review

For day-to-day review without leaving your datapack:

1. Open a datapack in the portal.
1. Select the **Conversations** tab.

The left rail lists stored sessions. Select one to read the full thread. For each assistant turn you can:

- Expand the **Decision trace** card to see agentic phases - planning, retrieval, tool use, and reasoning steps Langfuse captured for that turn.
- Click **View execution trace** to open a drawer with the observation tree, per-span latency, extracted memories, and a **View Detailed Trace** link into Observe.

Conversation headers also show summary chips where available: message count, memories extracted, and LLM calls for the session.

## WorkbenchLM — live chat traces

[WorkbenchLM](../workbench-lm/) is the portal's built-in chat. Each assistant reply includes the same trace affordances:

- **Decision trace**: Expand the purple **Decision trace** block above an answer to see how WorkbenchLM planned, searched memory and knowledge, and called MCP tools for that turn.
- **View execution trace**: Click the link under the reply to open the execution trace drawer (same drawer as Conversations), including the Observe deep-link in the footer.

Historical turns loaded from a previous session show their decision trace inline. Fresh turns fetch the trace shortly after Langfuse indexes the turn.

## What agents store vs. what Observe shows

Agents reach Meko through MCP. What appears in Observe depends on what your agent actually did:

| Source | What you see |
| :-- | :-- |
| MCP `conversation_*` tools | Stored threads in **Conversations** and matching Langfuse traces in **Observe** |
| MCP `memory_*` tools | Memories in the **Memories** tab; memory spans inside execution traces |
| MCP `knowledgebase_*` tools | Retrieval spans inside execution and decision traces |
| Agent-logged reasoning | **Decision trace** cards when the agent passes `reasoning` to `conversation_add_message`, or when Langfuse captures LLM chain-of-thought |

Observe reflects MCP and gateway activity Langfuse records. It is not a separate "traces API" your agent calls; agents retrieve audit history through [conversation and memory tools](../../reference/mcp-server/) for what they stored, and you review the full technical trace in the portal.

## Counts and per-turn metrics

In the portal you can view:

- **Datapack overview**: Counts of conversations, memories, learnings, and knowledge sources on each datapack.
- **Conversation list and headers**: Messages, memories extracted, and LLM calls per session (when Langfuse has indexed them).
- **Assistant message footers**: Per-turn latency, memories extracted, and LLM call count (WorkbenchLM and Conversations).
- **Observe / Langfuse**: Token usage and span latency inside individual traces — not rolled up as account-wide dashboards.

Plan limits (such as monthly MCP tool-call caps) appear under **Settings > Pricing**. They are entitlement limits, not live usage charts.

## Next steps

- [Trace and resume](../../examples/trace-and-resume/) — Log decisions to Meko, tier conversation history, and checkpoint long runs
- [Work with memory](../working-with-memory/) — Search and audit what agents stored as memories
- [Meko MCP Server reference](../../reference/mcp-server/) — Tools agents use to store conversations and memories
