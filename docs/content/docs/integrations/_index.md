---
title: Meko Integrations
headerTitle: Meko Integrations
linkTitle: Integration
headcontent: Explore how to integrate Meko into existing tools and environments
description: Explore how to integrate Meko into existing tools and environments
weight: 400
type: docs
---
# Integrations

Meko can be integrated 
a) for direct interactive use (e.g. with Claude, ChatGPT, Cursor), or
b) for programmatic use by your AI agent application

## Interactive Use       

Interactive use involves registering a connector, MCP Server, or equivalent with your preferred interactive productivity tool.
#### Cursor

**Cursor Desktop**
* On a Mac, go to the menu item Cursor -> Settings -> Cursor Settings -> Tools & MCPs -> New MCP Server
* Modify the mcp.json file that Cursor opens to add the below MCP server configuration 
```sh
{
  "mcpServers": {
    "meko-mcp-server": {
      "url": "<YOUR DATAPACK'S URL>",
      "headers": {
        "API_KEY": "<YOUR DATAPACK'S API KEY>"
      }
    }
  }
}
```

The result should look like this:
![[Pasted image 20260320163105.png]]

**Cursor Web**
* Not working yet.   Feel free to contribute here with info!
#### Claude Desktop (for Claude Chat, Cowork, and Code)

In Claude Desktop (whether used for Chat, Cowork, or Code), 
* On a Mac, go to the menu item Claude -> Settings -> Developer ->  Edit Config
* Edit the claude_desktop_config.json file so that the below MCP Server entry is added
* Quit and restart your Claude Desktop app

```sh
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

The result should look like this:

![[Pasted image 20260320164527.png]]

{{< tip title="Tip" >}} If you see errors when restarting Claude, examine the log file that appears.  Verify that you have the correct version of node.js binaries in your path, use absolute paths, and consult online MCP Server configuration troubleshooting guides.{{< /tip >}}

#### Claude Code (via CLI)

Run the setup command.   Run this command in your terminal to add the Meko MCP server to Claude Code. 

```sh
> claude mcp add --scope user --transport http meko <YOUR DATAPACK\'S URL> --header "API_KEY: <YOUR DATAPACK'S API KEY>"
```

To verify correct configuration, ask Claude at the command-line Console this question: 
"Is Claude configured with an MCP Server for Meko?"

You should see a response like the below:
![[Pasted image 20260320172719.png]]



#### Coming Soon
* OpenAI Codex
* VS Code / GitHub CoPilot 
* Opencode
* Google Anti-gravity
* OpenAI chatgpt Desktop
* Lovable                         
* Replit

Feel free to contribute here with info!

### Programmatic Use

To integrate Meko programmatically, register the Meko MCP Server with your application’s internal MCP Client. Most AI frameworks include built-in clients to simplify this process.

#### LangChain

#### Python

#### Coming Soon

* LangGraph
* OpenAI Agents SDK





