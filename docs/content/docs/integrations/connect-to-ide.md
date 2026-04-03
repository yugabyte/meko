---
title: Connect to an IDE
linkTitle: AI tools
description: Integrate Meko with Cursor, Claude Desktop, Claude Code, and more
weight: 3010
type: docs
---

Integrate with IDEs for interactive use cases, such as saving personal, team, and organizational preferences, standards, and policies.

## Cursor Desktop

1. In Cursor Desktop, choose **Cursor>Settings>Cursor Settings>Tools & MCPs>New MCP Server**.

1. Modify the `mcp.json` file that Cursor opens to add the following MCP server configuration.

    ```json
    {
      "mcpServers": {
        "meko": {
          "url": "<mcp-server-url>",
          "headers": {
            "API_KEY": "<mcp-server-api-key>"
          }
        }
      }
    }
    ```

    The result should look like the following:

    ![Cursor Desktop](/images/integrations-cursor-settings.png)

1. Verify the Connection by entering the following in chat.

    ```text
    Is Cursor configured with an MCP Server for Meko?
    ```

## Cursor Web

> [!NOTE] Coming soon
>
> This section is under development. Check back soon.

## Claude Desktop

In Claude Desktop (whether used for Chat, Cowork, or Code):

1. On a Mac, choose **Claude>Settings>Developer>Edit Config**.
1. Edit the `claude_desktop_config.json` file to add the following MCP Server entry.

    ```json
    {
      "mcpServers": {
        "meko": {
          "command": "npx",
          "args": [
            "-y",
            "mcp-remote",
            "<mcp-server-url>",
            "--allow-http"
          ],
          "env": {
            "API_KEY": "<mcp-server-api-key>"
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
    Is Claude configured with an MCP Server for Meko?
    ```

## Claude Code

1. Run the setup command in your shell to add the Meko MCP server to Claude Code:

    ```sh
    > claude mcp add --scope user --transport http meko <mcp-server-url> --header "API_KEY: <mcp-server-api-key>"
    ```

1. Verify the connection by entering the following at the command-line console:

    ```txt
    Is Claude configured with an MCP Server for Meko?
    ```

    You should see a response like the following:

    ![Claude Code](/images/integrations-claudecode-response.png)

## Open AI Codex Desktop

1. In Codex Desktop, choose **Codex>Settings>MCP servers>Add server>Streamable HTTP**.
1. Enter a name for the Meko MCP server.
1. Enter the URL of the Meko MCP server.
1. Enter a name such as 'MEKO_MCP_TOKEN' for the Bearer token env var, and click **Save**.

   The result should look like the following:

    ![Codex Desktop](/images/integrations-codex-desktop.png)

1. Set the MEKO_MCP_TOKEN environment variable with your Meko API key in your terminal using the following command:

    ```txt
    launchctl setenv MEKO_MCP_TOKEN <mcp-server-api-key>
    ```

1. Verify that you set the MEKO_MCP_TOKEN correctly with your Meko MCP Server API Key.

    ```txt
    launchctl getenv MEKO_MCP_TOKEN
    ```

1. Quit and restart Codex Desktop.

1. Verify the connection by entering the following in chat:

    ```text
    Is Codex Desktop configured with an MCP Server for Meko?
    ```

## Coming Soon

* VS Code / GitHub CoPilot
* Opencode
* Google Anti-gravity
* OpenAI chatgpt Desktop
* Lovable
* Replit
