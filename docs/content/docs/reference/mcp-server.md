---
title: Meko MCP Server
linkTitle: Meko MCP Server
description: MCP server and tools reference
weight: 40
---

The Meko MCP Server provides the primary tools for managing datapacks, conversations, memories, knowledge bases, and connecting to the PostgreSQL-compatible YugabyteDB database for executing database commands.

To connect from IDEs and MCP-compatible clients using your datapack URL and API key, see [Quick start — Connect your AI client](../../quick-start/#connect-your-ai-client).

## Tools

The Meko MCP Server provides the following tools.

| Use | For |
| :--- | :--- |
| [Conversation](#conversation-tools)<br>`conversation_*` | Full saved chat history and thread replay.<br><br>Example: _Save or retrieve this chat_ |
| [Memory](#memory-tools)<br>`memory_*` | Durable facts, preferences, decisions, and useful outcomes you want to recall across agents.<br><br>Example: _Remember this about me or the project_ |
| [Knowledge base](#knowledgebase-tools)<br>`knowledgebase_*` | Reusable indexed documents and document retrieval across agents.<br><br>Example: _Index these documents for reuse_ |
| [Database](#database-tools)<br>`db_*` | Structured operational data you want to query or modify with SQL.<br><br>Example: _Run SQL on my data_ |
| [Datapack](#datapack-tools)<br>`datapack_*` | Provisioning and managing Meko workspaces/environments.<br><br>Example: _Create or manage a workspace_ |

### Conversation tools

| Tool | Description |
| :--- | :---------- |
| `conversation_create` | Create a stored conversation container.<br>Use this to save a chat/thread, not just a fact or document.<br>Typical inputs: `agent_id, title, user_id, app_id, run_id, metadata` |
| `conversation_add_message` | Add a user/assistant turn to a stored conversation.<br>Use this to preserve full exchanges rather than storing long-term facts.<br>Typical inputs: `conversation_id, agent_id, input, output, reasoning, metadata` |
| `conversation_get` | Fetch a stored conversation, optionally with messages.<br>Use this to review or replay a past conversation.<br>Typical inputs: `conversation_id, agent_id, include_messages` |
| `conversation_list` | List stored conversations.<br>Use this to browse saved chat history.<br>Typical inputs: `agent_id, user_id, limit, offset` |
| `conversation_update` | Update a conversation title or metadata.<br>Use this when organizing or relabeling stored conversations.<br>Typical inputs: `conversation_id, agent_id, title, metadata` |
| `conversation_delete` | Permanently delete a stored conversation.<br>Use this to remove saved chat history.<br>Typical inputs: `conversation_id, agent_id` |

### Memory tools

| Tool | Description |
| :--- | :---------- |
| `memory_add` | Store durable facts, preferences, decisions, or useful outcomes.<br>Use this to add long-term reusable memory across threads.<br>Typical inputs: `agent_id, conversation_id, text, user_id, app_id, run_id, metadata` |
| `memory_search` | Search stored memories semantically and return related graph relations.<br>Use this to recall prior facts, preferences, or decisions.<br>Typical inputs: `agent_id, conversation_id, query, user_id, limit`|
| `memory_get_all` | List all memories for an agent or user scope.<br>Use this to audit or inspect what's been remembered.<br>Typical inputs: `agent_id, conversation_id, user_id, app_id, run_id` |
| `memory_get_by_id` | Retrieve a single memory by ID.<br>Use this to inspect a memory.<br>Typical inputs: `agent_id, conversation_id, memory_id` |
| `memory_update` | Replace the text of an existing memory.<br>Use this to correct a remembered fact.<br>Typical inputs: `agent_id, conversation_id, memory_id, text` |
| `memory_delete_by_id` | Delete one memory.<br>Use this to remove a specific stored memory.<br>Typical inputs: `agent_id, conversation_id, memory_id` |
| `memory_delete_all` | Delete all memories in a scope.<br>Use this for a full reset or broad cleanup.<br>Typical inputs: `agent_id, conversation_id, user_id, app_id, run_id` |

### Knowledgebase tools

| Tool | Description |
| :--- | :---------- |
| `knowledgebase_trigger_index_creation` | Create a new indexed document knowledge base and start ingestion.<br>Use this when you want reusable document retrieval across threads.<br>Typical inputs: `conversation_id, source_uris, index_name, ai_provider, embedding_model, embedding_dimensions, chunk_size, chunk_overlap, datapack_id` |
| `knowledgebase_add_source_to_index` | Add a document source to an existing index.<br>Use this to expand an existing knowledge base instead of creating a duplicate.<br>Typical inputs: `conversation_id, index_name, source_id, chunk_size, chunk_overlap, datapack_id` |
| `knowledgebase_check_index_status` | Check indexing/build progress and readiness.<br>Use this to verify a knowledge base is ready for retrieval.<br>Typical inputs: `conversation_id, index_name, datapack_id` |

### Database tools

| Tool | Description |
| :--- | :---------- |
| `db_run_read_only_query` | Run a read-only SQL query and return JSON results.<br>Use this for direct structured data access using YSQL.<br>Typical inputs: `conversation_id, query, datapack_id` |
| `db_run_write_query` | Run a write SQL statement in a transaction.<br>Use this to insert, update, or delete structured data.<br>Typical inputs: `conversation_id, query, datapack_id` |
| `db_summarize_database` | Summarize tables, schema, and row counts.<br>Use this to explore a datapack's database before writing queries.<br>Typical inputs: `conversation_id, schema, datapack_id` |

### Datapack tools

| Tool | Description |
| :--- | :---------- |
| `datapack_create` | Provision a new datapack with database and MCP details.<br>Use this to set up a new isolated Meko environment.<br>Typical inputs: `name` |
| `datapack_describe` | Return datapack details and optional status.<br>Use this to inspect an existing datapack.<br>Typical inputs: `name, include_status` |
| `datapack_list` | List available datapacks.<br>Use this to see all workspaces/environments.<br>Typical inputs: N/A |
| `datapack_update` | Update a datapack's database connection string.<br>Use this to point a datapack to a new database connection.<br>Typical inputs: `name, connection_string` |
| `datapack_delete` | Permanently delete a datapack.<br>Use this to completely delete an environment.<br>Typical inputs: `name` |