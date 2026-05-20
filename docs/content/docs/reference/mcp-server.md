---
title: Meko MCP Server
linkTitle: Meko MCP Server
description: MCP server and tools reference
weight: 40
---

The Meko MCP Server provides the primary tools for managing datapacks, conversations, memories, knowledge base, and related search.

To connect from IDEs and MCP-compatible clients, use the Meko MCP URL `https://mcp.mekodata.ai/mcp` and a user **API key** from **Settings>API Keys**. See [Integrations](../../integrations/) and [Quick start](../../quick-start/).

## Tools

The Meko MCP Server provides the following tools.

| Use | For |
| :--- | :--- |
| [Conversation](#conversation-tools)<br>`conversation_*` | Full saved chat history and thread replay.<br><br>Example: _Save or retrieve this chat_ |
| [Memory](#memory-tools)<br>`memory_*` | Durable facts, preferences, decisions, and useful outcomes you want to recall across agents.<br><br>Example: _Remember this about me or the project_ |
| [Knowledge base](#knowledgebase-tools)<br>`knowledgebase_*` | Reusable, persistent, indexed shared documents, and collective memories that can be searched across agents.<br><br>Example: _Search this in the knowledge base_ |
| [Datapack](#datapack-tools)<br>`datapack_*` | Provisioning and managing Meko workspaces/environments.<br><br>Example: _Create or manage a workspace_ |

<!-- 
| [Database](#database-tools)<br>`db_*` | Structured operational data you want to query or modify with SQL.<br><br>Example: _Run SQL on my data_ |
-->

### Conversation tools

| Tool | Description |
| :--- | :---------- |
| `conversation_create` | Create a stored conversation container for all conversations in a datapack.<br>Use this to save a chat/thread, not just a fact or document.<br>Typical inputs: `scope, agent_id, session_id, datapack_id, title, run_id, metadata` |
| `conversation_add_message` | Add a user/assistant turn to a stored conversation.<br>Use this to preserve full exchanges rather than storing long-term facts.<br>Typical inputs: `scope, seed, datapack_id, conversation_id, agent_id, input, output, reasoning, metadata` |
| `conversation_get` | Fetch a stored conversation, optionally with messages.<br>Use this to review or replay a past conversation.<br>Typical inputs: `scope, datapack_id, agent_id, conversation_id, include_messages, limit, offset` |
| `conversation_list` | List stored conversations.<br>Use this to browse saved chat history.<br>Typical inputs: `scope, datapack_id, agent_id, limit, offset` |
| `conversation_update` | Update a conversation title or metadata.<br>Use this when organizing or relabeling stored conversations.<br>Typical inputs: `scope, datapack_id, conversation_id, agent_id, title, metadata` |
| `conversation_delete` | Permanently delete a stored conversation.<br>Use this to remove saved conversation history.<br>Typical inputs: `scope, datapack_id, conversation_id, agent_id` |

### Memory tools

| Tool | Description |
| :--- | :---------- |
| `memory_add` | Store durable facts, preferences, decisions, or useful outcomes for a better context.<br>Use this to add long-term reusable memory across threads.<br>Typical inputs: `scope, datapack_id, agent_id, conversation_id, text, messages, run_id, metadata` |
| `memory_search` | Search stored memories semantically and return related graph relations.<br>Use this to recall prior facts, preferences, or decisions.<br>Typical inputs: `scope, datapack_id, agent_id, conversation_id, query, limit`|
| `memory_get_all` | List all memories for an agent or user scope.<br>Use this to audit or inspect what's been remembered.<br>Typical inputs: `scope, datapack_id, agent_id, conversation_id, run_id` |
| `memory_get_by_id` | Retrieve a single memory by ID.<br>Use this to inspect a memory.<br>Typical inputs: `scope, datapack_id, agent_id, conversation_id, memory_id` |
| `memory_update` | Replace the text of an existing memory.<br>Use this to correct a remembered fact.<br>Typical inputs: `scope, datapack_id, agent_id, conversation_id, memory_id, text` |
| `memory_delete_by_id` | Delete one memory.<br>Use this to remove a specific stored memory.<br>Typical inputs: `scope, datapack_id, agent_id, conversation_id, memory_id` |
| `memory_delete_all` | Delete all memories in a scope.<br>Use this for a full reset or broad cleanup.<br>Typical inputs: `scope, datapack_id, agent_id, conversation_id, run_id` |

### Knowledgebase tools

| Tool | Description |
| :--- | :---------- |
| `knowledgebase_search` | Perform a similarity search based on the user query.<br>Use this when you want to search the knowledge base for your queries.<br>Typical inputs: `scope, datapack_id, agent_id, conversation_id` |

<!--
### Database tools

| Tool | Description |
| :--- | :---------- |
| `db_run_read_only_query` | Run a read-only SQL query and return JSON results.<br>Use this for direct structured data access using YSQL.<br>Typical inputs: `scope, datapack_id, conversation_id, query` |
| `db_run_write_query` | Run a write SQL statement in a transaction.<br>Use this to insert, update, or delete structured data.<br>Typical inputs: `scope, datapack_id, conversation_id, query` |
| `db_summarize_database` | Summarize tables, schema, and row counts.<br>Use this to explore a datapack's database before writing queries.<br>Typical inputs: `scope, datapack_id, conversation_id, schema` |
-->

### Datapack tools

| Tool | Description |
| :--- | :---------- |
| `datapack_create` | Provision a new datapack with database and MCP details.<br>Use this to set up a new isolated Meko environment.<br>Typical inputs: `scope, conversation_id, datapack_id, name` |
| `datapack_describe` | Return datapack details and optional status.<br>Use this to inspect an existing datapack.<br>Typical inputs: `scope, conversation_id, datapack_id, name, include_status` |
| `datapack_list` | List available datapacks.<br>Use this to see all workspaces/environments.<br>Typical inputs: `scope, conversation_id, datapack_id, name` |
| `datapack_update` | Update a datapack's database connection string.<br>Use this to point a datapack to a new database connection. Typically used when you have access to multiple datapacks. <br>Typical inputs: `scope, conversation_id, datapack_id, name, include_status, connection_string` |
| `datapack_delete` | Permanently delete a datapack.<br>Use this to completely delete an environment.<br>Typical inputs: `scope, conversation_id, datapack_id, name` |