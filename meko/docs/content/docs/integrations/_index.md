---
title: Integrations
headerTitle: Meko Integrations
linkTitle: Integrations
headcontent: Connect Meko to your tools and frameworks
description: Integrate Meko with AI coding tools, agent frameworks, and consumer AI applications
weight: 4000
type: docs
---

Meko uses the Model Context Protocol (MCP) to connect each datapack to your AI coding tools, agent frameworks, and chat applications.

> [!NOTE] Free tier
>
> MCP tool usage counts toward Free tier limits, including **1,000 conversation messages** and **10,000 retrieval requests** per month (memory and knowledge-base searches). See [Meko pricing](https://mekodata.ai/pricing/) for plan details.

Follow these steps for any tool or framework:

1. **Create a datapack** in the Meko web portal.
2. **Add the Meko MCP server** to your AI agent or application.
3. **Use Meko** through natural language prompts.

## MCP server URL

MCP clients such as Cursor, Claude Desktop, and Claude Code need the MCP server URL.

Every account and datapack uses the same hosted endpoint URL:

```text
https://mcp.mekodata.ai/mcp
```

The datapack **Connect** dialog also shows this URL. The URL doesn't change when you switch datapacks.

The `datapack_id` or datapack name in each MCP tool call selects the datapack.

## Choose an authentication method

Meko accepts two ways for a client to authenticate.

| | API key | OAuth |
| :--- | :--- | :--- |
| What the client sends | An [`Authorization: Bearer`](#send-the-key-as-a-bearer-token) header | Nothing — it registers itself and opens a browser |
| Requires a browser | No; works unattended and in CI | Yes, whenever the session expires |
| Works from a terminal | Yes | No — see the note below |
| Identity | The key you generated | Your Meko portal login |

*Use only one method for each client.* If you configure both, the client may use OAuth instead of your API key. OAuth uses your browser identity. If that identity doesn't have a Meko account, every call returns `no Meko account found for your identity`. Generating a new API key won't help; remove one of the two configurations.

## OAuth

To use OAuth, enter only the MCP server URL. The client opens your browser so you can sign in to the Meko portal. It then gets the MCP tool list automatically.

> [!NOTE] OAuth not supported from the command line
>
> Command-line clients use a local `http://127.0.0.1` callback for OAuth. Meko currently rejects this callback, and registration returns an HTML `403 Forbidden` error. Use an API key for command-line clients. Desktop and IDE clients use their own URL scheme or a hosted callback, so OAuth works for them.

## API key

Meko API keys are long-lived personal access tokens. Each key starts with `mko_tkn_`.

Create a key in **Settings > API Keys** or in the datapack **Connect** dialog. To learn about naming, limits, revocation, and best practices, see [API keys](../reference/api-keys/).

> [!WARNING]
>
> Meko cannot show the full API key again after you close the create dialog. If you lose it, revoke the old key and generate a new one.

The datapack **Connect** dialog lists the prefixes of your active keys, such as `mko_tkn_3KvL9X8z…`. Use these prefixes to tell keys apart. When you configure a client, paste the full key that you saved, not the prefix.

### Send the key as a Bearer token

To authenticate with an API key, add both headers:

```text
Authorization: Bearer mko_tkn_your_key_here
User-Agent: meko-mcp-installer/1.0
```

The names matter. Meko reads only the `Authorization` header, and its value must start with `Bearer `. A header or environment variable named `API_KEY`, `MEKO_API_KEY`, or `X-Api-Key` doesn't work. Meko also rejects requests that don't include `User-Agent`.

Some clients need the `mcp-remote` helper to connect to a remote MCP server. In this guide, only Claude Desktop needs it. Its API key configuration is different; copy the [Claude Desktop configuration](connect-to-ai-agent/#claude-desktop).

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
