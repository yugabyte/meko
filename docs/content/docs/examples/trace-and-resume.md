---
title: Traceability, efficient conversation history, and resumability
linkTitle: Traces and resumability
description: Meko captures chain-of-thought history, stores & archives conversations, and enables agent resumability.
weight: 2020
---

Today, agents work as individuals but pretend to be a team. They can share outputs, such as a spec, a summary, and a result, but not the reasoning behind them. The less context you share, the more every agent re-derives from first principles, burning tokens and producing results that diverge, because LLMs are not deterministic.

Meko is agent-native data infrastructure: a single data layer for memory, knowledge, and decision traces, built for the primitives agents actually need.

## Auditable learning trail

*Full chain-of-thought traceability for what agents learned and how it flowed — ready for EU AI Act compliance.*

When an AI agent makes a decision, you should be able to ask: where did that come from? Decision traces store not just what an agent concluded, but the sources it consulted, the reasoning it applied, and how that conclusion entered the shared knowledge base.

This matters for two reasons. First, compliance: regulated industries need to reconstruct any agent decision after the fact. Second, quality: if a conclusion is wrong, the trace tells you exactly where it came from so you can fix it at the source — rather than patching outputs while the root cause keeps producing the same bad results.

**How to use it**

1. Tell the agent to log its decisions as it works.

    ```text
    Log each decision you make to Meko, including what context you retrieved and why you chose this approach.
    ```

    For example, an invoice-processing agent might log:

    ```text
    Flagged invoice #4872 as a duplicate. Retrieved prior payment record from 2026-04-15
    for the same vendor and amount. Chose to hold rather than reject — rejection requires
    manual review under current policy; a hold auto-resolves if no matching payment clears
    within 48 hours.
    ```

    The agent should persist these entries with MCP. For example, via `memory_add` for durable facts, or `conversation_add_message` with a `reasoning` field for turn-by-turn audit text.

1. Later, ask the agent to retrieve what it logged, or review it in the portal.

    ```text
    Search Meko for the decisions you logged about invoice #4872 and duplicate detection.
    ```

    ```text
    List the memories and conversation messages you stored about invoice-processing holds in April.
    ```

    Agents can only retrieve what they stored as conversations or memories. There is no separate "decision log API" beyond those MCP tools.

    To review the full technical trace, including MCP spans, LLM token usage, and nested observations, open the portal's **[Observe](../../guides/observability/#observe-full-langfuse-traces)** section, or expand **Decision trace** / **View execution trace** on a message in **Datapack > Conversations** or [WorkbenchLM](../../guides/workbench-lm/).

## Conversation history with tiering

*Recent history in-database for fast recall; older history auto-tiered to S3 to control costs.*

Long-running agents accumulate conversation history. Keeping all of it in a fast database is expensive; dropping it loses context.

Meko handles this automatically: recent history stays hot in the database for fast retrieval, while older history is transparently tiered to S3-backed object storage. The full history remains queryable, so when a decision no longer makes sense, you can trace it back to the conversations from months ago that led to it.

**How to use it**

1. Store conversation history.

    ```text
    Store all conversation history and agent reasoning in Meko.
    ```

That's it. Tiering is handled automatically — you don't configure a threshold or manage the move to object storage. 

1. When you need to trace a decision back to an old conversation, query it the same way regardless of age:

    ```text
    What did we decide about the invoice deduplication policy, and when?
    ```

Meko retrieves it whether the conversation is from yesterday or six months ago.

## Agent resumability

*Persist full agent and sub-agent state in Meko so any run can be paused and resumed reliably.*

Long-running agents fail. LLMs hit token limits. Network calls time out. Without state persistence, a failure means starting over from scratch.

Consider a supervisor agent checking in 300 files to a repository:

1. The agent starts processing files, checkpointing progress to Meko after each batch.
2. Midway through, the LLM runs out of context and the run fails.
3. You fix the underlying issue.
4. You reconnect to Meko with the same datapack.
5. Tell the agent: *"My last run failed midway. Retrieve your last checkpoint from Meko and resume from there."*
6. The agent picks up exactly where it left off.

Meko keeps an audit trail of sub-agent operations, so the supervisor knows exactly what completed and what remains.

**How to use it**

1. Tell the agent to checkpoint its state:

    ```text
    Checkpoint your progress to Meko after completing each file.
    ```

1. When resuming after a failure:

    ```text
    My last run failed midway. Retrieve your last checkpoint from Meko and resume from there.
    ```

## Next steps

{{< card-group >}}
  {{< card
    href="guides/observability/"
    title="Observability"
    desc="Where to find Observe, decision traces, and execution traces in the portal."
  >}}
  {{< card
    href="guides/workbench-lm/"
    title="WorkbenchLM"
    desc="Try memory, knowledge, and traces in the portal without an MCP install."
  >}}
{{< /card-group >}}
