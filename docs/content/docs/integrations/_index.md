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
      "url": "http://ae3ff3f1aeb2b4962923691977e31bab-5fb355c3a1c93f91.elb.us-east-2.amazonaws.com/mcp",
      "headers": {
        "API_KEY": "value"
      }
    }
  }
}
```

**Cursor Web**
* Not working yet.   Feel free to contribute here with info!
#### Claude Desktop / Claude Code / Claude CoWork




- [ ] OpenAI Codex
- [ ] VS Code			- P1
- [ ] Opencode			- P1
- [ ] Google Anti-gravity	- P1

### Programmatic Use
- [ ] Overview
- [ ] Python (direct MCP client)
- [ ] LangChain
- [ ] OpenAI Agents SDK

### w/ Vibe coding platforms                 
- [ ] Lovable                         
- [ ] Replit


### ─ w/ Consumer AI LLMs     		- P1
- [ ] Claude Desktop
- [ ] OpenAI chatgpt Desktop

Above section is awaiting:  Heather & Yu-Shen


