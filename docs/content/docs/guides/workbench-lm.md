---
title: WorkbenchLM
linkTitle: WorkbenchLM
description: Chat with your datapack in the Meko portal without installing an MCP client
weight: 1010
type: docs
---

**WorkbenchLM** is Meko's built-in chat experience in the [Meko portal](https://cloud.mekodata.ai/). Use it to try memory, knowledge, and traces without configuring Cursor, Claude, or another external MCP client.

> [!NOTE] Free tier
>
> Free accounts are limited to **1,000 conversations** and **10M WorkbenchLM tokens**. When a cap is reached, the chat composer is disabled. See [Meko pricing](https://mekodata.ai/pricing/) for plan details and upgrade options.

## When to use WorkbenchLM

- First session: Onboarding offers **Try the WorkbenchLM** so you can explore a sample knowledge base and see memories form in real time.
- Quick experiments: Upload a document, ask questions, and inspect traces and memories from the same UI.
- No local install: WorkbenchLM runs entirely in the portal; you do not need to set up an MCP server with an MCP config file or run the installer.

## Use WorkbenchLM

1. Sign in to your account.

    The first time you sign in, Meko displays the **Welcome to Meko** page.

1. Click **Try the WorkbenchLM**.

If it is not your first time signing in, do the following:

1. Sign in to the Meko portal.
1. Select **WorkbenchLM** in the left sidebar.

## What you can do

- Pick a datapack: Choose any datapack you own or that was shared with you as maintainer or contributor (viewer-only shares cannot use chat).
- Knowledge: Upload or manage knowledge-base documents in the left panel. Chat is available after a document has finished indexing.
- Chat: Send messages in the center panel. Meko routes requests through the inference gateway and MCP tools on your behalf.
- Live observation: The right panel shows memories, shared knowledge, and conversations, updated as you chat.
- Traces: Expand **Decision trace** above each reply, or click **View execution trace** under it to view decision traces. See [Observability](../observability/).

## WorkbenchLM vs Choose your tool (via external MCP clients)

| | WorkbenchLM | Choose your tool |
| :-- | :-- | :-- |
| Setup | None in the portal | Shared MCP URL (`https://mcp.mekodata.ai/mcp`) + user API key |
| Authentication | Your portal session | Long-lived `mko_tkn_…` key from **Settings>API Keys** |
| Best for | Trying Meko, demos, light Q&A | IDE-integrated development |

External clients authenticate with an [API key](../../integrations/#mcp-server-url-and-api-key). WorkbenchLM uses your signed-in session and server-side credentials for the selected datapack — you don't need to provide an API key to use WorkbenchLM.

## Next steps

{{< card-group >}}
  {{< card
    href="integrations/"
    title="Connect a coding agent"
    desc="Install Meko MCP on Cursor, Claude Code, or Claude Desktop."
  >}}
  {{< card
    href="guides/working-with-memory/"
    title="Work with memory"
    desc="Add, search, and manage agent memories."
  >}}
{{< /card-group >}}
