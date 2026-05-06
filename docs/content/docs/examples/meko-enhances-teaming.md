---
title: Enhanced teaming
linkTitle: Enhanced teaming
description: Use Meko to enhance team efficiency and productivity
weight: 2010
---

Meko gives your AI agent persistent memory that follows you across sessions, projects, and tools. This page shows how to use Meko to enhance team collaboration by having every human and agent on the team work from the same playbook.

## Knowledge handoff with context preserved

*Agent B gets Agent A's reasoning, not just its final output. No lost context across agents.*

Consider the classic handoff: one agent designs a spec, another implements it. The implementer gets the document, but not the reasoning behind it. When they hit a problem, they can't fix it at the source — they don't know the reasoning behind the decision. They patch the output without understanding the constraint, or escalate back and wait.

This compounds at every step. By the time the third agent acts, the original reasoning is gone.

![Multi-agent handoff Diagram](/images/meko-usecase-handoff.svg)

With Meko, you save what was decided and why, not only the final output. The downstream agent retrieves that context before it acts — including tradeoffs and rejected alternatives, not just conclusions.

**How to use it**

1. Agent A stores its reasoning before handing off.

    At the end of a task, ask the first agent to capture what it learned:

    Store your reasoning and findings in Meko so the next agent can pick them up.

    For example, a spec-writing agent might store:
  
    ```text
    Chose optimistic locking for the inventory table. Pessimistic locking caused
    deadlocks under concurrent writes in load testing (PR #142). Tradeoff: optimistic
    locking requires explicit retry logic on conflict — the implementation agent needs
    to handle that.
    ```

1. Agent B retrieves that context before starting.

   The implementing agent opens by searching for relevant prior reasoning:

   Retrieve the spec agent's reasoning from Meko before starting.

   It gets back the stored decision, including the retry requirement, rather than discovering that constraint by hitting it in production.

Takeaway: Capture conclusions before you switch agents or tasks. The downstream agent can ask Meko for the reasoning instead of re-deriving it or patching around constraints it doesn't understand.

## Collective learning across runs

*What worked, what didn't, and why — available on the next run when you save it.*

Most agentic systems have no memory of previous runs. Each run starts from scratch, repeating the same mistakes and rediscovering the same solutions. And if two agents independently analyze the same problem — or the same agent runs the same analysis twice — they may arrive at different conclusions, because LLMs are not deterministic.

A better approach: run the reasoning once, save what mattered, and have the next run start from that stable picture. A shared, consistent snapshot — even an imperfect one — is a better foundation for iteration than independent re-derivations that diverge. It also costs less: every redundant re-derivation burns tokens that stored context can avoid.

Meko keeps those learnings in the datapack so you can retrieve them later. **You still choose what to save**; not every line of chat becomes team truth automatically.

**How to use it**

After a run, store what the agent learned:

```text
Store what worked and what failed in this run to Meko, including the root cause of any failures.
```

For example, after a data pipeline run, an agent might store:

```text
Batch size of 500 caused memory errors on the transform step. Dropping to 100
resolved it. Root cause: the enrichment query does a full join — it doesn't
scale linearly. Don't exceed batch size 200 for this workflow.
```

At the start of the next run, retrieve prior learnings:

```text
Before starting, retrieve any learnings from previous runs of this workflow.
```

The next run starts with that constraint already known — no re-discovery, no repeated failure.

## Group/team policies and standards

*Share a datapack across your team so every agent works from the same playbook.*

Every team has tribal knowledge: the error format your APIs must follow, the testing bar for a PR to ship, the architectural patterns that are blessed vs. banned. Today that knowledge lives in wiki pages nobody reads, or worse, in one person's head. When a team member's AI agent generates code, it has no idea those standards exist.

Meko helps with a **shared team datapack** — one workspace where standards and decisions can live so agents **retrieve** them when asked. Sharing the datapack lets teammates plug into that same stored context.

For example, if you use Meko in Cursor and invite someone else to the same datapack, they see what has been saved in that workspace from memories you asked to store, knowledge you added, and similar — not every message from your session. For today's findings to help them, ask your agent to remember or save what matters (for example "remember this for the team"). Don't assume sharing alone copies ephemeral thread history to them.

**How to use it**

1. A team lead creates the shared [datapack](../../architecture/core-concepts/datapacks/#create-a-datapack) with a clear name and description.

1. The lead seeds the datapack with team standards by storing the real policies as memories or documents.

    In any session connected to that datapack, the lead asks the agent to remember the rules:

    ```text
    Remember our team coding standard: all API endpoints must return JSON
    using our standard envelope: { "data": ..., "error": { "code": ..., "message": ... } }
    ```

    ```text
    Remember our testing policy: PRs require minimum 80% code coverage,
    integration tests for all API endpoints, and no mocked database calls
    in integration tests — we got burned when mocked tests passed but a
    prod migration failed.
    ```

    ```text
    Remember our architectural policy: new services must use the event-driven
    pattern with our internal message bus. No direct synchronous service-to-service
    calls for new features.
    ```

    The agent stores these as memories in the shared datapack. The lead, as owner, can promote memories to shared knowledge so the team can retrieve them through the usual flows. For details on memories, see [Work with memory](../../guides/working-with-memory/).

1. Team members connect to the shared datapack.

    Think of a shared datapack like shared notes where you invite the team, give each person access that matches how much they should edit versus read, and have them register that datapack's connection in their AI client (Cursor, Claude Desktop, and so on).

    Each team member already has their own personal datapack (created when they signed up). Now they add the team datapack to their MCP server configuration, so their agent has access to both:

    - **Personal datapack** — individual preferences, coding style, role context.
    - **Team datapack** — shared standards, policies, and project knowledge.

    When their agent needs context, it draws from both — personal preferences for style, team standards for correctness.

    When you add a member, you assign them a role:

    | Role | View | Add | Promote |
    | ---- | ---- | --- | ------- |
    | Viewer | Yes | No | No |
    | Contributor | Yes | Yes | No |
    | Maintainer | Yes | Yes | Yes |

    - *View*: read knowledge-base material, memories, and conversations your deployment exposes for that datapack.
    - *Add*: create new memories.
    - *Promote*: elevate content into shared knowledge (often after review), per your product's flows.

1. Team members contribute knowledge as they work.

    As the team works, capture decisions in the shared datapack when others should benefit:

    - **Tech lead** — design direction as memory or uploaded docs.
    - **Developer** — summarize an API change from chat or Slack into Meko so it isn't lost (paste or paraphrase; there's no magic Slack sync).
    - **Analyst** — structured data through the paths your team uses (database tools, uploads, pipelines).
    - **QA** — flaky-environment gotchas as durable notes.

    Not every memory gets promoted to shared knowledge. A contributor's private memories stay private until a maintainer reviews and promotes them. This culling is what keeps the shared knowledge signal-dense. If everything flowed in indiscriminately, the useful policies would get buried in noise — and the compounding effect that makes shared context valuable would weaken.

1. The knowledge compounds.

    Here's the payoff: a developer implements a new payment endpoint; their agent can recall envelope format, architecture expectations, and recent decisions because those were stored in the team datapack — not because every IDE streams chat to everyone else.

    When you learn something the team should share, say it plainly and save it to the team datapack, for example:

    ```text
    Remember that the payments-v2 API has a 30-second timeout on webhook
    delivery — retries should use exponential backoff starting at 2 seconds.
    ```

    Teammates who use that datapack will see it when they search or ask their agent to recall team context — on their next session or question, not as an instant push to every screen.

##### What this looks like day-to-day

You open your IDE, connect to the team datapack when you need project rules, and ask naturally: "What does our team do about errors?" or "Remember this for payments."

**Takeaway:** Sharing opens a **shared vault** of what you chose to store. Ask Meko to remember what matters so collaborators' agents can use it when they connect to the same datapack.
