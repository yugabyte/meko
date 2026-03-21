---
title: Build Software with Meko
linkTitle: Build Software with Meko
description: Leverage conversation history, reasoning traces, and resumability
weight: 20
---

Meko stores conversation history and agent reasoning traces, enabling features like cross-session recall and resumability after failures.

## Conversation History and Reasoning

### The Problem

Without Meko, agent reasoning is lost when a session ends. Try this:

1. Ask your agent: *"Create a program in Python that lists the first 10 Fibonacci numbers"*
2. The agent generates code with reasoning along the way.
3. **Start a new session.**
4. Ask: *"Show me the agent's reasoning for building the Fibonacci program"*
5. You get zero results --- the reasoning is gone.

### With Meko

1. Connect to your Meko datapack and tell the agent:

   ```
   You have access to the Meko platform through MCP server.
   Store all my conversation history and agent reasoning in Meko.
   ```

2. Ask: *"Create a program in Python that lists the first 10 Fibonacci numbers"*
3. The agent generates code, and Meko stores the conversation and reasoning.
4. **Start a new session.**
5. Ask: *"Show me the agent's reasoning for building the Fibonacci program"*
6. The agent retrieves the stored reasoning from Meko.

Meko keeps all conversation history queryable while transparently tiering older messages to object storage (S3-backed), keeping hot data fast and cold data cheap.

## Resumability

Agents and sub-agents can persist their state in Meko for resumability after failures.

### Scenario

Imagine a supervisor agent checking in 300 files to GitHub:

1. The agent starts processing files, storing progress in Meko.
2. Midway through, the LLM runs out of tokens and the operation fails.
3. You fix the underlying problem (add more tokens).
4. Re-connect to Meko with the same datapack name.
5. Tell the agent: *"My last command failed midway in; I've fixed the underlying problem. Please retry or resume the last command."*
6. The agent retrieves its stored state from Meko and resumes where it left off.

Meko keeps an audit trail of sub-agent operations, so the supervisor agent knows exactly what was completed and what remains.

<!-- TODO: Add full walkthroughs with step-by-step examples (Heather/Naveen) -->
<!-- TODO: Add code samples showing how to configure conversation history storage -->
<!-- TODO: Add detailed resumability example with actual MCP tool calls -->

## Next Steps

- [Use Meko in Your IDE](../meko-in-your-ide/) --- Store preferences and standards
- [Observability guide](../../guides/observability/) --- View traces and reasoning history
