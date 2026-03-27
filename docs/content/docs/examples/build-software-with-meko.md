---
title: Meko in your agentic application
linkTitle: In your agentic application
description: Leverage conversation history, reasoning traces, and resumability
weight: 2020
---

Meko stores conversation history and agent reasoning traces, enabling features like cross-session recall and resumability after failures.

## Conversation and reasoning history

*Retain agent reasoning traces across sessions so you have a detailed log of how your agent achieved its outcomes.*

**Detailed Example**

Without Meko, agent reasoning is lost when a session ends. Try this:

1. Ask your agent:

    ```text
    Create a program in Python that lists the first 10 Fibonacci numbers
    ```

    The agent generates code with reasoning along the way.

1. Start a new session and ask the following:

    ```text
    Show me the agent's reasoning for building the Fibonacci program
    ```

    You get zero results --- the reasoning is gone.

Using Meko, agents can retain context:

1. Connect to your Meko datapack and tell the agent:

    ```text
    You have access to the Meko platform through MCP server.
    Store all my conversation history and agent reasoning in Meko.
    ```

1. Ask your agent:

    ```text
    Create a program in Python that lists the first 10 Fibonacci numbers
    ```

    The agent generates code, and Meko stores the conversation and reasoning.

1. Start a new session and ask the following:

    ```text
    Show me the agent's reasoning for building the Fibonacci program
    ```

    The agent retrieves the stored reasoning from Meko.

Meko keeps all conversation history queryable while transparently tiering older messages to object storage (S3-backed), keeping hot data fast and cold data cheap.

## Resumability

*Resume long-running agent tasks after failures by storing and retrieving agent state in Meko.*

Agents and sub-agents can persist their state in Meko so that they can resume after failures.

Consider the scenario of a supervisor agent checking in 300 files to GitHub:

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

## Next

- [Integrations](../../integrations/)
