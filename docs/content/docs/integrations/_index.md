---
title: Integrations
headerTitle: Meko Integrations
linkTitle: Integrations
headcontent: Connect Meko to your tools and frameworks
description: Integrate Meko with AI coding tools, agent frameworks, and consumer AI applications
weight: 3000
type: docs
---
Meko integrates via the Model Context Protocol (MCP).  Each Datapack exposes an MCP server that your AI coding tools, agent frameworks, or consumer chat applications connect to.

You follow the same basic pattern with all tools and frameworks:

1. **Create a datapack** using either the Meko web portal or the Meko CLI.
2. **Register the Meko MCP server** with your coding tool or with your application's MCP Client.
3. **Start using Meko** through natural language prompts.

<!-- TODO: create cards here that link to the different sections below for Cursor Desktop, Claude Desktop, Claude Code, LangGraph/LangChain, etc. -->

{{< card-group >}}
  {{< card
    href="integrations/connect-to-ide/#cursor-desktop"
    title="Cursor Desktop"
    desc=""
    cta="Setup &rarr;"
  >}}
  {{< card
    href="integrations/connect-to-ide/#cursor-web"
    title="Cursor Web"
    desc=""
    cta="Setup &rarr;"
  >}}
  {{< card
    href="integrations/connect-to-ide/#claude-desktop"
    title="Claude Desktop"
    desc=""
    cta="Setup &rarr;"
  >}}
  {{< card
    href="integrations/connect-to-ide/#claude-code"
    title="Claude Code"
    desc=""
    cta="Setup &rarr;"
  >}}
  {{< card
    href="integrations/#langchain-and-langgraph"
    title="LangChain and LangGraph"
    desc=""
    cta="Setup &rarr;"
  >}}
  {{< card
    href="integrations/#python"
    title="Python"
    desc=""
    cta="Setup &rarr;"
  >}}
{{< /card-group >}}

## IDEs
Step-by-step setup for Cursor, Claude, and other editors: [Connect to IDE](connect-to-ide/).

## Programmatic

To integrate Meko programmatically, register the Meko MCP Server with your application's internal MCP Client. Most AI frameworks include built-in clients to simplify this process.

This approach enables your application to save memory and knowledge in Meko, such as conversation memory, user and team preferences, and even application state.  

### LangChain and LangGraph

Meko can be integrated with LangChain and LangGraph agent frameworks through MCP. This page documents the setup and usage patterns for using Meko as the data layer for LangChain-based agents.

### Python

You can connect to Meko's MCP server directly from Python using an MCP client library. This page documents the setup, connection pattern, and example code for interacting with Meko datapacks from Python.

### Coming soon

* OpenAI Agents SDK
