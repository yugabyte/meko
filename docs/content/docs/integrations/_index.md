---
title: Integrations
headerTitle: Meko Integrations
linkTitle: Integrations
headcontent: Connect Meko to your tools and frameworks
description: Integrate Meko with AI coding tools, agent frameworks, and consumer AI applications
body_class: full-width
weight: 3000
type: docs
---

Meko integrates via the Model Context Protocol (MCP). Each Datapack exposes an MCP server that your AI coding tools, agent frameworks, or consumer chat applications connect to.

You follow the same basic pattern with all tools and frameworks:

1. **Create a datapack** using either the Meko web portal or the Meko CLI.
2. **Register the Meko MCP server** with your coding tool or with your application's MCP Client.
3. **Start using Meko** through natural language prompts.

## Connect to an IDE

{{< card-group variant="half" >}}
  {{< card
    href="integrations/connect-to-ide/#cursor-desktop"
    title="Cursor Desktop"
    logo="logos/integrations/cursor.svg"
  >}}
  {{< card
    href="integrations/connect-to-ide/#cursor-web"
    title="Cursor Web"
    logo="logos/integrations/cursor.svg"
  >}}
  {{< card
    href="integrations/connect-to-ide/#claude-desktop"
    title="Claude Desktop"
    logo="logos/integrations/claude.svg"
  >}}
  {{< card
    href="integrations/connect-to-ide/#claude-code"
    title="Claude Code"
    logo="logos/integrations/claude.svg"
  >}}
  {{< card
    href="integrations/connect-to-ide/#codex-desktop"
    title="Codex Desktop"
  >}}
{{< /card-group >}}

## Connect programmatically

{{< card-group variant="half" >}}
  {{< card
    href="integrations/connect-programatically/#langchain-and-langgraph"
    title="LangChain and LangGraph"
    logo="logos/integrations/langchain.svg"
  >}}
  {{< card
    href="integrations/connect-programatically/#python"
    title="Python"
    icon="fa-brands fa-python"
  >}}
{{< /card-group >}}
