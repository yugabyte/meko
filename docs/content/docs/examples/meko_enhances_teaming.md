---
title: Enhanced teaming
linkTitle: Enhanced teaming
description: Use Meko to enhance team efficiency & productivity
weight: 2010
---

Meko gives your AI agent persistent memory that follows you across sessions, projects, and tools. This page shows how to use Meko to enhance team collaboration by having every human and agent on the team work from the same playbook.

## Knowledge handoff with context preserved

*Agent B gets Agent A's reasoning, not just its final output. No lost context across systems.*

Consider the classic handoff: one agent designs a spec, another implements it. The implementer gets the document — but not why it was written that way. When they hit a problem, they can't fix it at the source because they don't know the reasoning behind the decision. They have to escalate back, re-explain the issue, and wait — or just patch the output without understanding what caused it.

This degrades at every step in a multi-agent pipeline. By the time the third agent acts, the original reasoning is gone.

With Meko, each agent stores its full reasoning trace — not just its output — so any downstream agent can retrieve not only *what* was decided but *why*.

**How to use it**

Before a handoff, the first agent stores its reasoning:

```text
Store your reasoning and findings in Meko before handing off to the code generation agent.
```

The receiving agent retrieves the full context before proceeding:

```text
Retrieve the research agent's reasoning from Meko before starting.
```

## Collective learning across runs

*What worked, what didn't, and why — persisted across workflow runs automatically.*

Most agentic systems have no memory of previous runs. Each run starts from scratch, repeating the same mistakes and rediscovering the same solutions. And if two agents independently analyze the same problem — or the same agent runs the same analysis twice — they may arrive at different conclusions, because LLMs are not deterministic.

A better approach: run the reasoning once, store the result, and have every agent work from that single stable truth. A shared, consistent result — even an imperfect one — is a better foundation for iteration than independent re-derivations that diverge. It also costs less: every redundant re-derivation burns tokens that a shared memory would have avoided.

Meko persists learnings across runs and reconciles new findings with what's already known, so shared knowledge compounds rather than diverges.

**How to use it**

After a run, store what the agent learned:

```text
Store what worked and what failed in this run to Meko, including the root cause of any failures.
```

At the start of the next run, retrieve prior learnings:

```text
Before starting, retrieve any learnings from previous runs of this workflow.

## Group/team policies and standards

*Share a datapack across your team so every agent works from the same playbook.*

Every team has tribal knowledge: the error format your APIs must follow, the testing bar for a PR to ship, the architectural patterns that are blessed vs. banned. Today that knowledge lives in wiki pages nobody reads, or worse, in one person's head. When a team member's AI agent generates code, it has no idea those standards exist.

Meko fixes this with a **shared team datapack** — a persistent, team-scoped knowledge base that every member's agent can read from and write to. Standards go in once, and every agent on the team applies them automatically.

### How it works in practice

**1. A team lead creates the shared datapack**

Using the Meko CLI, the lead creates a dedicated datapack for the team:

```bash
meko datapack create --name "payments-team"
```

Give the datapack a clear description of what kind of knowledge it will hold. Meko uses this description to guide how memories get extracted from your team's conversations — a vague description produces generic memories; a specific one keeps the datapack focused.

**2. The lead seeds it with team standards**

In any IDE session connected to the shared datapack, the lead tells their agent to store the team's policies:

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

The agent stores these as memories in the shared datapack — both as vector entries (for semantic search) and as graph relationships (for structured retrieval). The lead, as owner, can promote any memory to shared knowledge — elevating it from a personal observation to a team-wide truth that every agent applies automatically.

**3. Team members connect to the shared datapack**

Think of a shared datapack like a shared document. You create it, share it with the team, and everyone with access can contribute. Except instead of paragraphs and bullet points, the primitives are memories and knowledge graphs that your agents query directly.

Each team member already has their own personal datapack (created when they signed up). Now they add the team datapack to their MCP server configuration, so their agent has access to both:

- **Personal datapack** — individual preferences, coding style, role context
- **Team datapack** — shared standards, policies, and project knowledge

When their agent needs context, it draws from both — personal preferences for style, team standards for correctness.

When you add a member, you assign them a role:

| Role | Can add memories | Can promote to shared knowledge |
|------|-----------------|-------------------------------|
| Owner | Yes | Yes |
| Maintainer | Yes | Yes |
| Contributor | Yes | No |

Contributors can add memories immediately, but only maintainers and owners can promote memories to shared knowledge. This separation matters: it lets new team members start contributing without risking the integrity of the shared knowledge base.

**4. Team members contribute knowledge as they work**

As the team works on a project, each member's agent writes new knowledge back to the shared datapack:

- **The tech lead** adds the design spec to the knowledge base, so every agent understands the architecture
- **A developer** captures a Slack thread about an API contract change that affects the team
- **An analyst** loads structured data — a product catalog, pricing tiers, or feature flags — into a database table within the datapack
- **A QA engineer** documents a tricky workaround for a flaky test environment

Each of these goes into the right place automatically. Structured data lands in tables. Unstructured knowledge becomes searchable memories and graph relationships. The agent figures out the right storage — you don't have to think about it.

Not every memory gets promoted to shared knowledge. A contributor's private memories stay private until a maintainer reviews and promotes them. This culling is what keeps the shared knowledge signal-dense. If everything flowed in indiscriminately, the useful policies would get buried in noise — and the compounding effect that makes shared context valuable would weaken.

**5. The knowledge compounds**

Here's the payoff: a developer is implementing a new payment endpoint. Their agent already knows:

- The team's API envelope format (from the lead's policy)
- The event-driven architecture requirement (from the team standard)
- The updated API contract from last week's Slack thread (from a teammate's contribution)
- The product catalog and pricing tiers (from the analyst's structured data)

No one had to re-explain any of this. The agent just knows, because the team's collective context is right there in the shared datapack.

And when that developer discovers a new edge case — say, a retry policy that the team should adopt — they tell their agent, and it flows back into the shared datapack for everyone.

### What this looks like day-to-day

In practice, using a shared team datapack feels invisible. You open your IDE, start a session, and your agent already has the full team context. You don't say "check the team standards" — the agent does it automatically when it's relevant.

The context follows you regardless of which tool you're in. A developer using Cursor and a teammate using Claude Desktop connect to the same datapack — different agents, different IDEs, but the same shared knowledge. When a correction flows into the datapack, every connected agent gets it immediately.

When you learn something new that the team should know, you just say it naturally:

```text
Remember that the payments-v2 API has a 30-second timeout on webhook
delivery — retries should use exponential backoff starting at 2 seconds.
```

That knowledge is now available to every agent on the team, in every session, from that moment forward.

## Next

- [Traceability, resumability and more](../and_more_examples/) — Auditable learning trails, resumability, and conversation history
- [Integrations](../../integrations/)
