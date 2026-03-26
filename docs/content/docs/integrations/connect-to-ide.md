---
title: Connect to IDE
linkTitle: Connect to an IDE
description: Integrate Meko with Cursor, Claude Desktop, Claude Code, and other IDEs via MCP.
weight: 3010
type: docs
---

Integrate with IDEs for interactive use cases, such as saving personal, team, and organizational preferences, standards, and policies.

### Cursor Desktop

1. In Cursor Desktop, choose **Cursor>Settings>Cursor Settings>Tools & MCPs>New MCP Server**.

1. Modify the `mcp.json` file that Cursor opens to add the following MCP server configuration.

    ```json
    {
      "mcpServers": {
        "meko": {
          "url": "<YOUR DATAPACK'S URL>",
          "headers": {
            "API_KEY": "<YOUR DATAPACK'S API KEY>"
          }
        }
      }
    }
    ```

    The result should look like the following:

    ![Cursor Desktop](/images/integrations-cursor-settings.png)

1. Verify the Connection by entering the following in chat.

    ```text
    You have access to the Meko platform through MCP server. Have Meko remember all my personal preferences.
    ```

    If you're connected, Cursor will respond by indicating that it will use Meko's MCP tools to store memories.

### Cursor Web

> [!NOTE] Coming soon
>
> This section is under development. Check back soon.

Feel free to contribute here with info!

### Claude Desktop

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
            "<YOUR DATAPACK'S URL>",
            "--allow-http"
          ]
        }
      },
    }
    ```

1. Quit and restart Claude Desktop.

    The result should look like the following:

    ![Claude Desktop](/images/integrations-claude-desktop.png)

> [!TIP] Handling errors
>
> If you see errors when restarting Claude, examine the log file that appears. Verify that you have the correct version of node.js binaries in your path, use absolute paths, and consult online MCP Server configuration troubleshooting guides.

### Claude Code

Run the setup command. Run this command in your terminal to add the Meko MCP server to Claude Code.

```sh
> claude mcp add --scope user --transport http meko <your-datapak-url> --header "API_KEY: <YOUR DATAPACK'S API KEY>"
```

To verify correct configuration, ask Claude at the command-line Console this question:

```txt
Is Claude configured with an MCP Server for Meko?
```

You should see a response like the following:

![Claude Code](/images/integrations-claudecode-response.png)

### Coming Soon

* OpenAI Codex
* VS Code / GitHub CoPilot
* Opencode
* Google Anti-gravity
* OpenAI chatgpt Desktop
* Lovable
* Replit

Feel free to contribute here with info!