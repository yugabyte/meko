---
title: Meko MCP Server
linkTitle: Meko MCP Server
description: MCP SERVER and MCP Tools description
weight: 40
---

The Meko MCP Server provides the primary tools for managing datapacks, agents, knowledge bases, and connecting to the PostgreSQL compatible database YugabyteDB for executing database commands

**Base URL:** `/mcp/v1`

## Authentication

Most endpoints require a Bearer token obtained from the login endpoint.

```text
Authorization: Bearer {access_token}
```

## Auth endpoints

### Sign up

Register a new user account.

```text
POST /auth/signup
```

**Request body:**

| Field | Type | Required | Description |
| :---- | :--- | :------- | :---------- |
| `email` | string | Yes | User email address |
| `password` | string | Yes | Account password |
| `first_name` | string | Yes | First name |
| `last_name` | string | Yes | Last name |

**Responses:** `201 Created`, `400 Bad Request`, `409 Conflict`

### Confirm sign up

Confirm email with verification code.

```text
POST /auth/confirm-signup
```

**Request body:**

| Field | Type | Required | Description |
| :---- | :--- | :------- | :---------- |
| `email` | string | Yes | User email address |
| `code` | string | Yes | Verification code |

**Responses:** `200 OK`, `400 Bad Request`

### Login

Authenticate and receive tokens.

```text
POST /auth/login
```

**Request body:**

| Field | Type | Required | Description |
| :---- | :--- | :------- | :---------- |
| `email` | string | Yes | User email address |
| `password` | string | Yes | Account password |

**Response body (200):**

| Field | Type | Description |
| :---- | :--- | :---------- |
| `access_token` | string | JWT access token |
| `refresh_token` | string | Refresh token |
| `id_token` | string | ID token |
| `expires_in` | integer | Token expiry in seconds |

**Responses:** `200 OK`, `400 Bad Request`, `401 Unauthorized`

### Logout

Invalidate all sessions.

```text
POST /auth/logout
```

**Headers:** `Authorization: Bearer {access_token}` (required)

**Responses:** `200 OK`, `401 Unauthorized`

## Tool reference

This section summarizes the available Meko MCP Server tools, including what each tool does, the inputs it typically expects, and when to use it.

| Use | For |
| :--- | :--- |
| [Conversation](#conversation-tools)<br>`conversation_*` | Full saved chat history and thread replay. |
| [Memory](#memory-tools)<br>`memory_*` | Durable facts, preferences, decisions, and useful outcomes you want to recall across agents. |
| [Knowledge base](#knowledgebase-tools)<br>`knowledgebase_*` | Reusable indexed documents and document retrieval across agents. |
| [Database](#database-tools)<br>`db_*` | Structured operational data you want to query or modify with SQL. |
| [Datapack](#datapack-tools)<br>`datapack_*` | Provisioning and managing Meko workspaces/environments. |

### Conversation tools

| Tool | Description |
| :--- | :---------- |
| `conversation_create` | Creates a stored conversation container.<br>Use this When you want to save a chat/thread, not just a fact or document.<br>Typical inputs: `agent_id, title, user_id, app_id, run_id, metadata` |
| `conversation_add_message` | Adds a user/assistant turn to a stored conversation. Use this when preserving full exchanges rather than storing long-term facts. Typical inputs: `conversation_id, agent_id, input, output, reasoning, metadata` |
| `conversation_get` | Fetches a stored conversation, optionally with messages. Use this when reviewing or replaying a past conversation. Typical inputs: `conversation_id, agent_id, include_messages` |
| `conversation_list` | Lists stored conversations. Use this when browsing saved chat history. Typical inputs: `agent_id, user_id, limit, offset` |
| `conversation_update` | Updates a conversation title or metadata. Use this when organizing or relabeling stored conversations. Typical inputs: `conversation_id, agent_id, title, metadata` |
| `conversation_delete` | Permanently deletes a stored conversation. Use this when you intentionally want to remove saved chat history. Typical inputs: `conversation_id, agent_id` |

### Memory tools

| Tool | Description |
| :--- | :---------- |
| `memory_add` | Stores durable facts, preferences, decisions, or useful outcomes.<br>Use this when you want long-term reusable memory across threads.<br>Typical inputs: `agent_id, conversation_id, text, user_id, app_id, run_id, metadata` |
| `memory_search` | Searches stored memories semantically and returns related graph relations.<br>Use this when recalling prior facts, preferences, or decisions.<br>Typical inputs: `agent_id, conversation_id, query, user_id, limit`|
| `memory_get_all` | Lists all memories for an agent or user scope.<br>Use this when auditing or inspecting what has been remembered.<br>Typical inputs: `agent_id, conversation_id, user_id, app_id, run_id` |
| `memory_get_by_id` | Retrieves one memory by ID.<br>Use this when you already know the exact memory to inspect.<br>Typical inputs: `agent_id, conversation_id, memory_id` |
| `memory_update` | Replaces the text of an existing memory.<br>Use this when a remembered fact needs correction.<br>Typical inputs: `agent_id, conversation_id, memory_id, text` |
| `memory_delete_by_id` | Deletes one memory.<br>Use this when a specific stored memory is no longer wanted.<br>Typical inputs: `agent_id, conversation_id, memory_id` |
| `memory_delete_all` | Deletes all memories in a scope.<br>Use this when doing a full reset or broad cleanup.<br>Typical inputs: `agent_id, conversation_id, user_id, app_id, run_id` |

### Knowledgebase tools

| Tool | Description |
| :--- | :---------- |
| `knowledgebase_trigger_index_creation` | Creates a new indexed document knowledge base and starts ingestion. Use this when you want reusable document retrieval across threads.<br>Typical inputs: `conversation_id, source_uris, index_name, ai_provider, embedding_model, embedding_dimensions, chunk_size, chunk_overlap, datapack_id` |
| `knowledgebase_add_source_to_index` | Adds another document source to an existing index. Use this when expanding an existing knowledge base instead of creating a duplicate.<br>Typical inputs: `conversation_id, index_name, source_id, chunk_size, chunk_overlap, datapack_id` |
| `knowledgebase_check_index_status` | Checks indexing/build progress and readiness. Use this before assuming a knowledge base is ready for retrieval.<br>Typical inputs: `conversation_id, index_name, datapack_id` |

### Database tools

| Tool | Description |
| :--- | :---------- |
| `db_run_read_only_query` | Runs a read-only SQL query and returns JSON results.<br>Use this when you need direct structured data access and know the SQL to run.<br>Typical inputs: `conversation_id, query, datapack_id` |
| `db_run_write_query` | Runs a write SQL statement in a transaction. Use this when inserting, updating, or deleting structured data.<br>Typical inputs: `conversation_id, query, datapack_id` |
| `db_summarize_database` | Summarizes tables, schema, and row counts. Use this when exploring a datapack database before writing queries.<br>Typical inputs: `conversation_id, schema, datapack_id` |

### Datapack tools

| Tool | Description |
| :--- | :---------- |
| `datapack_create` | Provisions a new datapack/workspace with DB and MCP details.<br>Use this when setting up a new isolated Meko environment.<br>Typical inputs: `name` |
| `datapack_describe` | Returns datapack details and optional status.<br>Use this when inspecting an existing datapack.<br>Typical inputs: `name, include_status` |
| `datapack_list` | Lists available datapacks.<br>Use this when you want to see all workspaces/environments.<br>Typical inputs: N/A |
| `datapack_update` | Updates a datapack's database connection string.<br>Use this when repointing a datapack to a new database connection.<br>Typical inputs: `name, connection_string` |
| `datapack_delete` | Permanently deletes a datapack.<br>Use this when removing an environment entirely.<br>Typical inputs: `name` |

## Fast rule of thumb

- "Remember this about me or the project" -> `memory_*`
- "Index these docs for reuse" -> `knowledgebase_*`
- "Run SQL on my data" -> `db_*`
- "Save or retrieve this chat" -> `conversation_*`
- "Create or manage a workspace" -> `datapack_*`

## Health check

```text
GET /health
```

**Auth:** Not required

**Responses:** `200 OK`

## Common response format

All endpoints return responses in the following format:

```json
{
  "data": { ... },
  "error": {
    "code": "string",
    "message": "string",
    "details": ["string"]
  }
}
```

The `error` field is only present when the request fails.
