---
title: AI Coding Tools
linkTitle: AI Coding Tools
description: Integrate Meko with AI-powered coding tools via MCP
weight: 10
---

Meko integrates with AI coding tools through the Model Context Protocol (MCP). Each datapack exposes an MCP server that coding tools can connect to, giving your AI assistant access to persistent memory, knowledge bases, and database operations.

## Integration Pattern

All AI coding tools follow the same basic pattern:

1. **Create a datapack** using `meko datapack create --name <name>`.
2. **Add the MCP server configuration** to your tool's settings.
3. **Start using Meko** through natural language prompts.

The MCP configuration is the same across tools:

```json
{
  "mcpServers": {
    "my-datapack": {
      "command": "meko",
      "args": ["mcp", "serve", "--name", "my-datapack"]
    }
  }
}
```

Where this configuration goes varies by tool --- see the individual pages below.

## Supported Tools

- **[Cursor](cursor/)** --- Add MCP config to `.cursor/mcp.json`
- **[Claude Code](claude-code/)** --- Configure via Developer settings, Connectors, or Marketplace
- **[OpenAI Codex](openai-codex/)** --- MCP server configuration
- **[VS Code](vs-code/)** --- Planned for future release

<!-- TODO: Add tool-specific feature matrices and comparison tables -->
