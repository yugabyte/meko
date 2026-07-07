---
title: Quick start
linkTitle: Quick start
description: Three levels of integration — from exploring in the portal to connecting your own AI app
weight: 1000
type: docs
---

Meko has three levels of integration. Each gives you more control over how your agents use memory, knowledge, and conversation history. Start at level 1 and work up, or skip ahead.

| Level | What you connect | What you get |
| :--- | :--- | :--- |
| 1. [WorkbenchLM](#try-meko-in-workbenchlm) | Our chat agent, in the portal | Simplest. See memory and knowledge work before installing anything. |
| 2. [Your AI client](#connect-your-ai-client) | Cursor, Claude Code, Claude Desktop, Codex, VS Code | Daily driver. Use your own coding tools against your datapacks. |
| 3. [Your AI app](#connect-your-ai-app) | Your own agent or framework | Full control. Build Meko into the apps you ship. _Under development._ |

Before you start, [request access](https://info.yugabyte.com/meko-request-access) and sign in to the [Meko portal](https://cloud.mekodata.ai/). You land on the **Welcome to Meko** page with a default datapack already created for you.

New accounts start on the **Free** tier. See [Meko pricing](https://mekodata.ai/pricing/) for plan details and limits.

> [!NOTE]
>
> Meko itself is not an AI agent — it's the data layer (shared knowledge, collective memory, decision traces) that your agents connect to. WorkbenchLM is an embedded chat agent so you can try Meko in the portal without installing anything; it's not the daily-use surface.

## Try Meko in WorkbenchLM

The simplest way to see Meko in action. WorkbenchLM runs in the portal against your default datapack, with sample knowledge loaded and live memory traces visible as you chat. Nothing to install.

1. In the portal sidebar, open **WorkbenchLM**.
2. Ask about the sample knowledge, or share something about your project for Meko to remember.
3. Watch the **Memory** and **Decision traces** panels update as the conversation moves.

See [WorkbenchLM](../guides/workbench-lm/) for what's loaded, when to use it versus a real coding agent, and the limits of the embedded experience.

## Connect your AI client

External AI clients (Cursor, Claude Code, Claude Desktop, Codex, VS Code, and so on) connect to Meko through one shared MCP endpoint:

```text
https://mcp.mekodata.ai/mcp
```

You don't need a different URL per datapack. Your agent picks the datapack at call time by passing a `datapack_id` (or datapack name) into MCP tool calls. API keys and the MCP connection are scoped to your account, not to a single datapack. Once connected, your agent can see and act on every datapack you own.

There are three ways to connect. Pick the one that fits your workflow.

{{< tabpane text=true right=false >}}
{{% tab header="Automated install" %}}

_Recommended for Cursor, Claude Code, and Claude Desktop._

The portal gives you a one-line `npx` command per client. It registers the Meko MCP server _and_ installs an agent behavioral skill that teaches your agent _when_ to use Meko tools automatically, saving conversations, capturing memories, and searching knowledge without you having to ask each time. In harnesses that support it (such as Claude Code), the skill also hooks into session lifecycle events to capture context at session start and end.

1. Generate an API key from **Settings > API Keys**. Name it for the client that will use it (for example, `laptop/claude-desktop`). The key starts with `mko_tkn_` and is shown exactly once: copy or download it before closing.
2. Open the **Connect** dialog on any datapack, or go to the **Connect** page in the sidebar. Select your client tab (Claude Code, Claude Desktop, or Cursor).
3. Run the install command shown. It looks like this:

    ```sh
    npx @yugabytedb/meko-mcp --client <client> --url "https://mcp.mekodata.ai/mcp" --api-key <paste-your-api-key>
    ```

    Replace `<client>` with `claude-code`, `claude-desktop`, or `cursor`.

4. Restart your client if the new MCP server doesn't show up automatically.

The portal also has a **Manual MCP Install** tab for other clients, which walks through running nvm commands and adding the skills package step by step.

See [@yugabytedb/meko-mcp on npm](https://www.npmjs.com/package/@yugabytedb/meko-mcp) to see what the installer does under the hood.

{{% /tab %}}
{{% tab header="OAuth" %}}

_For Claude Desktop and other clients with a custom-connector flow._

If your client supports adding a custom remote MCP connector, you can connect Meko by URL with no API key and no installer. The client opens a browser for OAuth; your Meko portal login is the identity.

1. In your client, add a custom MCP connector pointing at `https://mcp.mekodata.ai/mcp`.
2. Complete the OAuth handshake when the browser opens.
3. The MCP server returns its tool list to the client automatically.

With OAuth alone, your agent interacts with Meko _on demand_: you need to mention "Meko" in your prompt for the agent to call the tools. For example, _"save this to Meko"_, _"search my Meko knowledge base"_. The agent won't proactively save conversations or capture memories unless you ask.

To get automatic behavior, use the **Automated install** instead, or write your own skills to teach your agent when to call Meko tools.

{{% /tab %}}
{{% tab header="Manual API key" %}}

_For any MCP-compatible client — no npx, no installer._

Add the Meko MCP server directly to your client's config file with your API key. This gives you the same on-demand MCP tools as OAuth, but authenticated with a long-lived key instead of a browser handshake.

1. Go to **Settings > API Keys** and click **Generate API key**. Name it for the client that will use it (for example, `laptop/cursor`). The full key is shown exactly once: click **Copy** or **Download .txt** before closing.
2. Edit your client's MCP config to add the Meko server. For example, in Cursor's `mcp.json`:

    ```json
    {
      "mcpServers": {
        "meko": {
          "url": "https://mcp.mekodata.ai/mcp",
          "headers": {
            "API_KEY": "<paste-your-api-key>"
          }
        }
      }
    }
    ```

3. Restart your client.

Like OAuth, this is _on demand_: your agent calls Meko tools when you ask. For per-client configuration paths and examples (Claude Desktop, Claude Code, Codex, VS Code), see [Connect to an AI agent](../integrations/connect-to-ai-agent/).

{{% /tab %}}
{{< /tabpane >}}

For API key generation, revocation, and limits, see [API keys](../reference/api-keys/).

### Verify the connection

1. Ask your agent to list your datapacks.

    ```text
    List my datapacks in Meko.
    ```

    It should call the `datapack_list` tool and return the names you see in the portal. If it doesn't, see [Troubleshooting](../troubleshooting/).

## Connect your AI app

_Under development._ Programmatic integration patterns for agent frameworks (LangChain, LlamaIndex, custom Python or TypeScript agents) are coming. See [Connect programmatically](../integrations/connect-programmatically/) for the current state and what's planned.

## Next

{{< card-group >}}
  {{< card
    href="quick-start/explore-meko/"
    title="Explore Meko"
    desc="Add knowledge to your datapack and create memories."
  >}}
{{< /card-group >}}
