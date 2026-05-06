---
title: Explore knowledge and memory
linkTitle: Explore memory
description: Experience how Meko stores knowledge and saves memories
weight: 1000
type: docs
---

Meko provides a persistent datastore for the full spectrum of information that AI agents need, from raw to refined:

- **Conversations** are one example of episodic information: raw, verbatim records of events, unfiltered and comprehensive. Episodic information captures everything that happened, relevant or not.
- **Memory** is derived from episodic information: extracted, condensed, and stored for associative retrieval. Memory trades completeness for efficiency and recallability.
- **Knowledge** is confirmed information — curated documents and facts that agents can rely on with confidence. Knowledge may be loaded directly or promoted from memory and conversations.

We'll start by exploring knowledge, then move on to memory.

## Upload knowledge

To see how Meko stores knowledge, add documents to your datapack and query them from your AI client.

As an example, for Claude Code, the standard for providing project knowledge and coding practices is a [CLAUDE.md file](https://claude.com/blog/using-claude-md-files). You can add your CLAUDE.md file to your datapack.

In your coding agent, issue a prompt to add a document to your datapack, and attach a file in CSV, JSON, markdown, or PDF format.

Use the following markdown content in Claude Code to try this out by copying and modifying the contents to a local `CLAUDE.md` file.

```text
# Project Guidelines: Building agents to triage issues being logged by customers

## Overview
Build agents to triage issues being logged by Zendesk and create a Root Cause Analysis for it

## Tech Stack
- **Framework**: Next.js 15 (App Router)
- **Language**: TypeScript (Strict Mode)
- **Styling**: Tailwind CSS
- **Database**: Meko

## Core Coding Standards
- **Strict Typing**: Never use `any`. Define interfaces for all API responses.
- **Functional Style**: Prefer pure functions and immutable data patterns over classes.
- **Components**: Place reusable UI in `src/components/ui` and feature-specific logic in `src/features/*`.
- **Naming**: Use camelCase for variables/functions and PascalCase for components/types.

## Critical Commands
Specific bash commands Claude should use to verify its work:
- **Build**: `npm run build`
- **Lint**: `npm run lint`
- **Test**: `npm test` or `npm run test:watch`
- **Format**: `npm run format`

## Quality Check Workflow
IMPORTANT: After every significant code change, you MUST follow this sequence:
1. Run `npm run format` to ensure style consistency.
2. Run `npm run lint` to check for type errors or warnings.
3. Run `npm test` and fix any regressions before reporting completion.

## Project Gotchas & Patterns
- **Auth**: Always use the `useAuth()` hook; do not access session tokens directly from local storage.
- **API**: All external service calls must include a 3-second timeout and explicit error handling.
```

##### Use the portal

On the Meko portal:

1. Navigate to **Datapacks** and select your datapack.
1. Click **Actions>Add Knowledge**.
1. Upload your file.

Meko will process the file; this may take a few minutes.

<!--##### Use the CLI

You can also upload knowledge using the Meko CLI. See [Meko CLI](../../reference/meko-cli/) for installation instructions.

Upload documents to your datapack's knowledge base as follows:

```bash
meko datapack add_knowledge_base --name meko_default_datapack --url s3://bucket/docs/
```
-->

## Query your knowledge

Once your documents are parsed, chunked, and indexed by Meko, query them through your coding agent. For example:

```text
What are the critical commands for the project?
```

```output
The critical commands defined in your project guidelines are:

- Build: npm run build
- Lint: npm run lint
- Test: npm test (or npm run test:watch for watch mode)
- Format: npm run format

And per the Quality Check Workflow, after every significant code change you should run them in this order: npm run format → npm run lint → npm test, fixing any regressions before reporting completion.
```

The MCP server performs semantic search across your indexed documents and returns relevant chunks. In short, Meko has automated the retrieval augmented generation (RAG) pre-processing pipeline by reading, parsing, chunking, vectorizing/indexing the documents, and by providing a semantic search capability.

## Explore memory

Meko uses _memory_ to enhance user experience through _personalization_.

Ask this question of your coding agent without Meko:

```text
Propose a 5 day vacation for me. Do not use Meko or any prior recollection of my preferences in generating your response.
```

Your AI either recommends a randomly-selected vacation, or perhaps asks generic follow-up questions such as "What kind of vacation?", "What budget?", or "To where?"

With Meko, however, your AI can remember your vacation preferences, and use them to generate better responses.

### Create a memory

In your coding agent, do the following:

1. Tell the agent to store your preferences:

    ```text
    You have access to the Meko platform through MCP server.
    Have Meko remember all my personal preferences.
    ```

1. Type the following to persist your personal preferences as memory:

    ```text
    Please remember that I like to take vacations at tropical beach locations, on a budget
    ```

    Using the Meko MCP server, the agent will create a conversation in your datapack and assign an agent ID, for example, `personal-preferences`. The agent should let you know what the agent ID is; you can also ask it.

1. Start a new chat session, then type the following (use the agent ID the other agent provided):

    ```text
    Access my preferences using agent <cursor_name>.
    Propose a 5 day vacation for me.
    Access my preferences using agent personal-preferences.

Your AI can now leverage the memory to provide a more relevant response.

For example, Cursor can respond with something similar to the following:

```text
Here’s a 5-day plan tuned to what you’ve saved: tropical beach + budget.

## Pick a base: Isla Holbox or Puerto Morelos area, Mexico (Yucatán)

Why it fits: Warm water, palm-lined beaches, easy buses from Cancún airport,
hostels / small hotels and street food keep costs down. (Swap-ins: Koh Lanta,
Thailand or Nusa Penida + Sanur, Bali if you’re already closer to Asia.)

<...>

---

## Budget levers

- Stay: hostel dorm or simple guesthouse slightly back from the sand (often 
  half the price of beachfront).
- Eat: markets, loncherías, set “menu del día” lunches; limit restaurant dinners.
- Move: buses + shared shuttles; avoid private taxis except airport if needed.
- Play: prioritize free swimming, walking, and one paid half-day trip instead
  of stacked tours.

---

## If you tell me your month, home airport region, and solo vs. group, I can
## narrow this to one place, rough daily spend, and a rain-day backup.
```

The agent retrieves your stored preferences and immediately proposes a vacation aligned to these preferences (a tropical beach vacation within budget). No follow-up questions are needed.

### Check your memories

You can verify what Meko stored by checking your datapack's memories in the Meko portal. You'll see:

- The stored memories with their extracted entities
- Key metrics: number of conversations, memories, learnings, and knowledge

You can also ask your agent. Type:

```text
What does Meko remember about me?
```

## Observe your datapack

Check what's happening inside your datapack:

- **Conversations**: View stored conversation history. Provides access to reasoning traces and execution logs for all your conversations.
- **Memories**: View stored memories and their extracted entities.
- **Learnings**: Meko highlights memories that might be worth promoting to shared knowledge (Ko).
- **Knowledge**: See indexed document sources and their status.

<!-- TODO: Add LangFuse observability walkthrough once integration details are finalized -->
<!-- TODO: Add screenshots showing costs, calls, and timings -->

## Next steps

With your first datapack live, you can transition from a basic demo to a robust, context-aware development environment. Explore the following advanced capabilities to fully leverage Meko's persistent memory and knowledge graph.

{{< card-group >}}
  {{< card
    href="guides/working-with-memory/"
    title="Manage and curate memories"
    desc="Search and list what is stored, compress overlapping facts, or clear a datapack when you need a reset."
  >}}
  {{< card
    href="guides/working-with-knowledge-bases/"
    title="Grow your knowledge base"
    desc="Add more sources and combine document RAG with memory-driven assistants."
  >}}
  {{< card
    href="examples/meko-enhances-teaming/"
    title="Collaborate"
    desc="Share datapacks with your team so that every agent follows the same project standards, or carry personal preferences across AI agents."
  >}}
  {{< card
    href="examples/trace-and-resume/"
    title="Trace and resume"
    desc="Store conversation history and agent reasoning traces, enabling features like cross-session recall and resumability after failures."
  >}}
{{< /card-group >}}

<!--
- **Manage and curate memories.** Search and list what is stored, compress overlapping facts, or clear a datapack when you need a reset. See [Working with Memory](../guides/working-with-memory/).
- **Share context across people and tools.** Collaborate by sharing datapacks with your team, so that every agent follows the same project standards, or carry personal preferences across AI agents. See [Meko in your development process](../examples/meko-in-your-ai-agent/) and [Datapacks](../architecture/core-concepts/datapacks/).
- **Build resilient multi-agent systems**. Preserve context across long-running tasks and multiple tools. Meko lowers costs by archiving old history and allows agents to resume work immediately after failures or session resets. See [Meko in your agentic application](../examples/build-software-with-meko/).
- **Grow knowledge bases.** Add more sources and combine document RAG with memory-driven assistants. See [Working with Knowledge Bases](../guides/working-with-knowledge-bases/).
-->