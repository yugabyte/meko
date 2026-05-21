---
title: Integrations
headerTitle: Meko Integrations
linkTitle: Integrations
headcontent: Connect Meko to your tools and frameworks
description: Integrate Meko with AI coding tools, agent frameworks, and consumer AI applications
body_class: full-width
weight: 4000
type: docs
---

Meko integrates via the Model Context Protocol (MCP). Each Datapack exposes an MCP server that your AI coding tools, agent frameworks, or consumer chat applications connect to.

You follow the same basic pattern with all tools and frameworks:

1. **Create a datapack** using the Meko web portal.
2. **Register the Meko MCP server** with your AI agent or with your application's MCP Client.
3. **Start using Meko** through natural language prompts.

## MCP server URL and API key

External MCP clients (Cursor, Claude Desktop, Claude Code, and similar tools) need two values:

| Value | Scope | Where to get it |
| :---- | :---- | :-------------- |
| **Meko MCP server URL** | Shared (same for all users and datapacks) | `https://mcp.mekodata.ai/mcp` — also shown in any datapack **Connect** dialog. |
| **API key** | Per user | **Settings>API Keys** section (or, if there are no existing keys, **Generate API key** on the datapack **Connect** wizard). |

### Generate an API key

Meko issues long-lived API keys (personal access tokens) for MCP authentication. Each key starts with `mko_tkn_` and is shown in full **only once** when you create it.

Generate keys from **Settings > API Keys** or from the datapack **Connect** dialog. For the full walkthrough — naming, limits, revocation, and best practices — see [API keys](../reference/api-keys/).

> [!WARNING]
>
> Meko cannot show the full API key again after you close the create dialog. If you lose it, revoke the old key and generate a new one.

### MCP server URL

Every account and datapack uses the same hosted endpoint:

```text
https://mcp.mekodata.ai/mcp
```

The datapack **Connect** dialog also shows this URL (it does not change when you switch datapacks).

Which datapack your agent reads or writes is determined by the `datapack_id` (or datapack name) passed in MCP tool calls — not by a different URL per datapack.

The datapack **Connect** dialog lists your active key **prefixes** (for example `mko_tkn_3KvL9X8z…`) so you can tell keys apart. Paste the full key you saved at creation into your client configuration or installer command. Use the placeholder `<paste-your-api-key>` in portal install commands, not the prefix alone.

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
