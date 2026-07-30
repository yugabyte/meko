---
title: Connect programmatically
linkTitle: Frameworks
description: Integrate Meko with LangChain, LangGraph, Python, and other frameworks via MCP
weight: 3020
type: docs
---

Any MCP-capable framework can register with the Meko MCP server (`https://mcp.mekodata.ai/mcp`) using an API key.

To integrate Meko programmatically, register the Meko MCP Server with your application's internal MCP Client. Most AI frameworks include built-in clients to simplify this process.

This approach enables your application to save memory and knowledge in Meko, such as conversation memory, user and team preferences, and even application state.

Note that currently you must [upload knowledge](../../quick-start/explore-meko/#upload-knowledge) via the portal.

## Learn more

- [Meko MCP Server reference](../../reference/mcp-server/)

<!-- 

> [!NOTE] Coming soon
>
>* OpenAI Agents SDK
>* LangChain
>* LangGraph
>* Lyzr
>* CrewAI

## Langchain and Langraph
Meko can be integrated with LangChain and LangGraph agent frameworks through MCP. This page documents the setup and usage patterns for using Meko as the data layer for LangChain-based agents.

## Python

You can connect to Meko's MCP server directly from Python using an MCP client library. This page documents the setup, connection pattern, and example code for interacting with Meko datapacks from Python. -->