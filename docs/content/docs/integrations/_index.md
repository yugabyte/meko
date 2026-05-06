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

1. **Create a datapack** using the Meko web portal.
2. **Register the Meko MCP server** with your AI agent or with your application's MCP Client.
3. **Start using Meko** through natural language prompts.

## MCP Server URL and API Key

To register the hosted MCP server, you will need your datapack MCP URL and API key.

1. In the Meko portal, select your datapack.
1. In the **Connect** box, click **Connect**.
1. Copy the MCP **URL** and **API key**.

## Connect to an AI agent

{{< card-group variant="third" >}}
  {{< card
    href="integrations/connect-to-ai-agent/#cursor-desktop"
    title="Cursor Desktop"
    logo="logos/integrations/cursor.svg"
  >}}
  {{< card
    href="integrations/connect-to-ai-agent/#cursor-web"
    title="Cursor Web"
    logo="logos/integrations/cursor.svg"
  >}}
  {{< card
    href="integrations/connect-to-ai-agent/#claude-desktop"
    title="Claude Desktop"
    logo="logos/integrations/claude.svg"
  >}}
  {{< card
    href="integrations/connect-to-ai-agent/#claude-code"
    title="Claude Code"
    logo="logos/integrations/claude.svg"
  >}}
  {{< card
    href="integrations/connect-to-ai-agent/#open-ai-codex-desktop"
    title="Codex Desktop"
    logo="logos/integrations/codex.svg"
  >}}
  {{< card
    href="integrations/connect-to-ai-agent/#vs-code"
    title="VS Code"
    logo="logos/integrations/vscode.svg"
  >}}
{{< /card-group >}}

<!--
## Connect programmatically

> [!NOTE] Coming soon
-->
