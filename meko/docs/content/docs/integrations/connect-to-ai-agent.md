---
title: Connect to an AI agent
linkTitle: AI agents
description: Integrate Meko with Cursor, Claude Desktop, Claude Code, Codex, and more
weight: 3010
type: docs
---

Integrate with AI agents for interactive use cases, such as saving personal, team, and organizational preferences, standards, and policies.

> [!NOTE] Free tier
>
> Every MCP call from an external client counts toward your account's Free tier quotas (conversations, retrievals, and related caps). See [Meko pricing](https://mekodata.ai/pricing/) for plan details.

## Before you connect

1. **Generate an API key**. In the portal, open **Settings>API Keys>Generate API key**, enter a name, then copy or download the key before closing the dialog (it starts with `mko_tkn_` and is shown only once).
1. **Use the MCP server URL** `https://mcp.mekodata.ai/mcp` (the same URL appears in the **Datapacks>Connect** dialog).

For any of the following integrations, replace `<paste-your-api-key>` with the key you saved. The portal's one-line installer uses that same placeholder.

You can also connect Meko by URL with no API key and no installer using OAuth. The client opens a browser for OAuth; your Meko portal login is the identity. The MCP server returns its tool list to the client automatically.

For the full workflow, see [MCP server URL and API key](../#mcp-server-url-and-api-key).

### OAuth and Manual setup

When you connect with OAuth or using the Manual setup, your agent interacts with Meko _on demand_ only: you need to mention "Meko" in your prompt (for example, _"save this to Meko,"_ _"search my Meko knowledge base"_) for the agent to call the tools. The agent won't proactively save conversations or capture memories _unless you ask._

To get automatic behavior, use the **Automated setup** instead, or write your own skills to teach your agent when to call Meko tools.

## Cursor Desktop

##### Automated setup

In the Meko portal, select **Connect** and follow the steps.

##### OAuth

In Cursor Desktop:

1. Open **Cursor > Settings > Cursor Settings > Tools & MCPs > New MCP Server**.
1. Add to `mcp.json` without any auth headers:

    ```json
    {
      "mcpServers": {
        "meko": {
          "url": "https://mcp.mekodata.ai/mcp"
        }
      }
    }
    ```

1. Complete the OAuth handshake when the browser opens.

##### Manual setup

In Cursor Desktop:

1. Choose **Cursor>Settings>Cursor Settings>Tools & MCPs>New MCP Server**.

1. Modify the `mcp.json` file that Cursor opens to add the following MCP server configuration.

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

    Replace `<paste-your-api-key>` with the API key you saved from **Settings>API Keys**.

    The result should look like the following:

    ![Cursor Desktop](/images/integrations-cursor-settings.png)

1. Verify the Connection by entering the following in chat.

    ```text
    List my datapacks in Meko
    ```

## Cursor Web

> [!NOTE] Coming soon
>
> This section is under development. Check back soon.

## Claude Desktop

##### Automated setup

In the Meko portal, select **Connect** and follow the steps.

This adds a `mcpServers.meko` entry in the `claude_desktop_config.json` configuration file of your Claude Desktop (see also Manual setup).

##### OAuth

In Claude Desktop:

1. Navigate to **Settings**, and under **Customize** select **Connectors**.
1. Search for **Meko** in the Connectors search box, and add it as a custom MCP connector with the URL `https://mcp.mekodata.ai/mcp`.
1. Complete the OAuth handshake when the browser opens.

##### Manual setup

In Claude Desktop (whether used for Chat, Cowork, or Code):

1. Navigate to **Settings**, and under **Desktop app** select **Developer**, then click **Edit Config**.
1. Edit the `claude_desktop_config.json` file to add the following MCP Server entry. Replace `<paste-your-api-key>` with the token you saved.

    ```json
    {
      "mcpServers": {
        "meko": {
          "command": "npx",
          "args": [
            "-y",
            "mcp-remote",
            "https://mcp.mekodata.ai/mcp",
            "--allow-http"
          ],
          "env": {
            "API_KEY": "<paste-your-api-key>"
          }
        }
      }
    }
    ```

1. Quit and restart Claude Desktop.

    The result should look like the following:

    ![Claude Desktop](/images/integrations-claude-desktop.png)

    > [!TIP] Handling errors
    >
    > If you see errors when restarting Claude, examine the log file that appears. Verify that you have the correct version of node.js binaries in your path, use absolute paths, and consult online MCP Server configuration troubleshooting guides.

1. Verify the connection by entering the following in chat:

    ```text
    List my datapacks in Meko
    ```

### Uninstall

How you remove Meko from Claude Desktop depends on how you installed it, because each installation method creates a different artifact.

##### Automated setup

Run the following:

```bash
npx @yugabytedb/meko-mcp --client claude-desktop --uninstall
```

If you installed under a non-default server name, pass `--server-name <name>`. The uninstall removes only the MCP server entry; if you also imported a Meko skill bundle, remove it manually via **Settings > Extensions**.

##### OAuth

In Claude Desktop:

1. Navigate to **Settings**, and under **Customize** select **Connectors**.
1. Search for **Meko** in the Connectors search box and delete it.

##### Manual setup

In Claude Desktop:

1. Navigate to **Settings**, and under **Desktop app** select **Developer**, then click **Edit Config**.
1. Edit the `claude_desktop_config.json` file to delete the `mcpServers.meko` entry.
1. Restart Claude Desktop.

## Claude Code

##### Automated setup

In the Meko portal, select **Connect** and follow the steps.

##### OAuth

1. Run the setup command in your shell to add the Meko MCP server:

    ```sh
    claude mcp add --scope user --transport http meko https://mcp.mekodata.ai/mcp
    ```

1. Complete the OAuth handshake when the browser opens.

##### Manual setup

1. Run the setup command in your shell to add the Meko MCP server to Claude Code:

    ```sh
    claude mcp add --scope user --transport http meko https://mcp.mekodata.ai/mcp --header "API_KEY: <paste-your-api-key>"
    ```

    Replace `<paste-your-api-key>` with the API key you saved from **Settings>API Keys**.

1. Verify the connection by entering the following at the command-line console:

    ```txt
    List my datapacks in Meko
    ```

### Updating the skills plugin

Skill content (the behavioral guides that teach the agent how to use Meko's MCP tools) ships in the `meko-agent-skills` plugin and versions independently of the MCP server. The marketplace source is `https://github.com/yugabyte/meko-skills.git`.

##### Inside an active Claude Code session

```text
/plugin marketplace update meko-agent-skills
/plugin update meko-agent-skills@meko-agent-skills
/reload-plugins
```

If `/reload-plugins` warns that it would re-read the conversation, rerun it as `/reload-plugins --force`. Then run `/doctor` to confirm there are no plugin load errors.

##### From the shell

```sh
claude plugin marketplace update meko-agent-skills
claude plugin update meko-agent-skills@meko-agent-skills
```

The qualified `plugin@marketplace` form is required. Re-running the installer (`npx @yugabytedb/meko-mcp@latest`) updates an existing install automatically.

##### Enable auto-update

Auto-update is per-marketplace and **disabled by default** for third-party marketplaces. To opt in:

1. Open `/plugin`, switch to the **Marketplaces** tab, select `meko-agent-skills`, and toggle **Enable auto-update**.
1. Or add `"autoUpdate": true` to the existing `meko-agent-skills` entry in `~/.claude/settings.json` (user scope) or a project's `.claude/settings.json`. Keep the `source` field — an `autoUpdate`-only replacement drops the marketplace source:

    ```json
    {
      "extraKnownMarketplaces": {
        "meko-agent-skills": {
          "source": {
            "source": "git",
            "url": "https://github.com/yugabyte/meko-skills.git"
          },
          "autoUpdate": true
        }
      }
    }
    ```

Once enabled, Claude Code refreshes the marketplace and updates installed plugins in the background after startup. The running session keeps the versions it loaded at launch; you'll be prompted to `/reload-plugins`, or the new versions load on the next launch.

## Open AI Codex Desktop

##### Automated setup

In the Meko portal, select **Connect** and follow the steps.

##### OAuth

In Codex Desktop:

1. Choose **Codex>Settings>MCP servers>Add server>Streamable HTTP**.
1. Enter the URL `https://mcp.mekodata.ai/mcp` and leave the Bearer token field empty.
1. Click **Authenticate** beside Meko in the list of MCP servers and complete the OAuth handshake when the browser opens.

##### Manual setup

In Codex Desktop:

1. Choose **Codex>Settings>MCP servers>Add server>Streamable HTTP**.
1. Enter a name for the Meko MCP server.
1. Enter the Meko MCP server URL (`https://mcp.mekodata.ai/mcp`).
1. Enter a name (such as `MEKO_MCP_TOKEN`) for the Bearer token environment variable, and click **Save**.

    The result should look like the following:

    ![Codex Desktop](/images/integrations-codex-desktop.png)

1. Set the `MEKO_MCP_TOKEN` environment variable with your Meko API key in your terminal using the following command:

    ```sh
    launchctl setenv MEKO_MCP_TOKEN <paste-your-api-key>
    ```

1. Verify that you set `MEKO_MCP_TOKEN` correctly with your API key.

    ```sh
    launchctl getenv MEKO_MCP_TOKEN
    ```

1. Quit and restart Codex Desktop.

1. Verify the connection by entering the following in chat:

    ```text
    List my datapacks in Meko
    ```

## VS Code

1. Run the following command:

    ```sh
    echo "### Installing the latest Node.js ###"
    curl -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.2/install.sh | bash
    source ~/.$(basename $SHELL)rc
    nvm install --lts
    echo "### Registering the Meko MCP server ###"
    npx mcp-add@latest \
      --name meko \
      --type stdio \
      --command 'npx -y mcp-remote https://mcp.mekodata.ai/mcp --allow-http' \
      --env 'MEKO_API_KEY=<paste-your-api-key>' \
      --headers 'MEKO_API_KEY=<paste-your-api-key>' \
      --scope global \
      --clients 'vscode'
    ```

    Replace `<paste-your-api-key>` with the API key you saved from **Settings>API Keys**.

1. Verify the connection by entering the following in chat:

    ```text
    List my datapacks in Meko
    ```

<!-- ## Windsurf, GitHub Copilot CLI, Gemini Anti-Gravity, and others

For Windsurf, GitHub Copilot CLI, Gemini Anti-Gravity, Continue, Goose, OpenCode, and Windsurf, use the "Automated Setup" described in the [Quick Start](/quick-start/#connect-your-ai-client).  

Additional support for the below is coming soon
* Opencode
* OpenAI chatgpt Desktop
* Lovable
* Replit -->
