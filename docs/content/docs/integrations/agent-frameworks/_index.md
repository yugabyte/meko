---
title: Agent Frameworks
linkTitle: Agent Frameworks
description: Integrate Meko with agent orchestration frameworks
weight: 20
---

Meko works with agent frameworks that support MCP (Model Context Protocol). Each datapack exposes an MCP server that frameworks can connect to for memory, knowledge, and database operations.

## Supported Frameworks

| Framework | Status | Description |
|-----------|--------|-------------|
| [Python MCP](python-mcp/) | Available | Direct MCP client integration |
| [LangChain](langchain/) | Planned | LangChain/LangGraph integration |
| [OpenAI Agents SDK](openai-agents-sdk/) | Planned | OpenAI agent framework integration |

Meko also works with CrewAI, LangGraph, and other frameworks that support MCP.

## General Pattern

Regardless of framework, the integration pattern is:

1. Create a datapack with `meko datapack create`.
2. Configure your framework to connect to the datapack's MCP server.
3. Use the MCP tools (memory, knowledge, database) from within your agent logic.

<!-- TODO: Add SDK-specific integration details and code samples -->
<!-- TODO: Add comparison of framework capabilities with Meko -->
