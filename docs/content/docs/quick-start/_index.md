---
title: Quick start
linkTitle: Quick start
description: Sign up, connect an AI coding agent, and explore your first datapack
weight: 1000
type: docs
---

## Get started

Get from zero to a working Meko integration in minutes:

1. [Request access](https://info.yugabyte.com/meko-request-access).

1. Create a Meko account at the [Meko portal](https://cloud.mekodata.ai/).

1. Sign in to your account.

    The first time you sign in, Meko displays the **Welcome to Meko** page.

1. Click **Choose Your Tool**.

    Meko displays the **Configure** wizard, where you will create your first [datapack](../architecture/core-concepts/datapacks/).

    Follow the instructions in the wizard to:

    - Name your datapack
    - [Configure](../integrations/connect-to-ai-agent) the Meko MCP server on your AI tool of choice.
    - Optionally, add [knowledge](../architecture/core-concepts/knowledgebase/) to your datapack.

That's it - when you finish, Meko displays your datapack, and you're ready to start adding knowledge and creating memories using your favorite agent.

> [!TIP]
>
> You may need to restart your client to start the MCP server connection.
>
> To add additional clients, you can return to the configure wizard by selecting your datapack and choosing **Actions>Edit**.
>
> The connection instructions use node.js and nvm. If you need to connect your client manually, refer to [Integrations](../../integrations/) for instructions.

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

## Next steps

With your first datapack live, you can transition from a basic demo to a robust, context-aware development environment. Explore the following advanced capabilities to fully leverage Meko's persistent memory and knowledge graph.

{{< card-group >}}
  {{< card
    href="quick-start/explore-memory/"
    title="Explore knowledge and memory"
    desc="Add knowledge to your datapack and create memories."
  >}}
  {{< card
    href="guides/working-with-memory/"
    title="Manage and curate memories"
    desc="Search and list what is stored, compress overlapping facts, or clear a datapack when you need a reset."
  >}}
  {{< card
    href="guides/working-with-knowledge-bases/"
    title="Grow your knowledge base"
    desc="Add more sources and combine document RAG with memory-driven assistants."
  >}}
  {{< card
    href="examples/meko-enhances-teaming/"
    title="Collaborate"
    desc="Share datapacks with your team so that every agent follows the same project standards, or carry personal preferences across AI agents."
  >}}
  {{< card
    href="examples/trace-and-resume/"
    title="Trace and resume"
    desc="Store conversation history and agent reasoning traces, enabling features like cross-session recall and resumability after failures."
  >}}
{{< /card-group >}}

<!--
- **Manage and curate memories.** Search and list what is stored, compress overlapping facts, or clear a datapack when you need a reset. See [Working with Memory](../guides/working-with-memory/).
- **Share context across people and tools.** Collaborate by sharing datapacks with your team, so that every agent follows the same project standards, or carry personal preferences across AI agents. See [Meko in your development process](../examples/meko-in-your-ai-agent/) and [Datapacks](../architecture/core-concepts/datapacks/).
- **Build resilient multi-agent systems**. Preserve context across long-running tasks and multiple tools. Meko lowers costs by archiving old history and allows agents to resume work immediately after failures or session resets. See [Meko in your agentic application](../examples/build-software-with-meko/).
- **Grow knowledge bases.** Add more sources and combine document RAG with memory-driven assistants. See [Working with Knowledge Bases](../guides/working-with-knowledge-bases/).
-->
