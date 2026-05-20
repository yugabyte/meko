---
title: Quick start
linkTitle: Quick start
description: Sign up, connect an AI coding agent, and explore your first datapack
weight: 1000
type: docs
---

External AI agents can connect to Meko's hosted **Meko MCP server** at:

```text
https://mcp.mekodata.ai/mcp
```

## Get started

Get from zero to a working Meko integration in minutes:

1. Sign in to your account at the [Meko portal](https://cloud.mekodata.ai/).

    The first time you sign in, Meko displays the **Welcome to Meko** page.

1. Choose how you want to start:

    - **Try the WorkbenchLM** — Chat in the portal with your default datapack, sample knowledge, and live memory traces. No MCP install required. See [WorkbenchLM](../guides/workbench-lm/).
    - **Connect to AI agent** — Opens the **Configure** wizard to name your datapack, [install Meko MCP](../integrations/connect-to-ai-agent) on Cursor / Claude Code / Claude Desktop, and optionally add [knowledge](../architecture/core-concepts/knowledgebase/).

That's it — when you finish, you can use **WorkbenchLM** in the sidebar, or connect an external agent with the MCP URL above and an API key from **Settings** → **API Keys**.

> [!TIP]
>
> - You may need to restart your client to start the MCP server connection.
> - Generate an API key under **Settings** → **API Keys** (or **Generate API key** on the configure wizard **Install** step) before running install commands. Copy or download the key before closing the dialog — it is shown only once.
> - To add additional clients, return to the configure wizard by selecting your datapack and choosing **Actions>Edit**, or open **Install** from the sidebar.
> - The connection instructions use Node.js and nvm. For manual setup, see [Integrations](../../integrations/).

With your first datapack live, you can transition from a basic demo to a robust, context-aware development environment.

<!--
1. Copy the following MCP Server details and store them in a safe place:

    - Your MCP server **URL**.
    - Your MCP server **API key**.

## Connect your AI client manually

A single setup command registers the Meko MCP server with your agent and installs the Meko skills package, so your agent knows when and how to use Meko's MCP tools for memory, knowledge, and conversation management. -->

<!-- Add this after confirming what skills will finally get installed. A single setup command registers the Meko MCP server with your agent and installs the Meko skills package (including **meko-mcp-tools** and **ysql**), so your agent knows when and how to use Meko's MCP tools for memory, knowledge, conversation management, and SQL database access. -->

<!--
> [!NOTE]
>
> Currently, this command is supported for **Cursor**, **Claude Desktop**, and **Claude Code** only. For other supported clients such as VS Code and Codex, see [Integrations](../integrations/).

### Prerequisites

- [Node.js 18+](https://nodejs.org/)
- [npm](https://www.npmjs.com/) and [npx](https://docs.npmjs.com/cli/v8/commands/npx) (usually included with a standard Node.js install)
- Meko MCP Server URL and API key

### Install

Run the following command to register the Meko MCP server and install the Meko skills package.

Replace the placeholders as follows:

- `<mcp-client>` — `cursor`, `claude-code`, or `claude-desktop`
- `<mcp-server-url>` — the MCP server URL you saved from the portal
- `<mcp-server-api-key>` — the MCP server API key you saved from the portal

```sh
npx @yugabytedb/meko-mcp --client <mcp-client> --url <mcp-server-url> --api-key <mcp-server-api-key>
```

> [!TIP] Install all supported clients at once
>
> Use `--client all` to configure Cursor, Claude Desktop, and Claude Code in a single run (with the same URL and API key as above):
>
> ```sh
> npx @yugabytedb/meko-mcp --client all --url <mcp-server-url> --api-key <mcp-server-api-key>
> ```
-->

## Next

{{< card-group >}}
  {{< card
    href="quick-start/explore-memory/"
    title="Explore Meko"
    desc="Add knowledge to your datapack and create memories."
  >}}
{{< /card-group >}}

<!--
- **Manage and curate memories.** Search and list what is stored, compress overlapping facts, or clear a datapack when you need a reset. See [Working with Memory](../guides/working-with-memory/).
- **Share context across people and tools.** Collaborate by sharing datapacks with your team, so that every agent follows the same project standards, or carry personal preferences across AI agents. See [Meko in your development process](../examples/meko-in-your-ai-agent/) and [Datapacks](../architecture/core-concepts/datapacks/).
- **Build resilient multi-agent systems**. Preserve context across long-running tasks and multiple tools. Meko lowers costs by archiving old history and allows agents to resume work immediately after failures or session resets. See [Meko in your agentic application](../examples/build-software-with-meko/).
- **Grow knowledge bases.** Add more sources and combine document RAG with memory-driven assistants. See [Working with Knowledge Bases](../guides/working-with-knowledge-bases/).
-->
