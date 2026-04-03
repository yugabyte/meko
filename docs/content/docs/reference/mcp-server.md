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

---

# Meko MCP Server tools reference

This document summarizes the Meko MCP Server tools available in this session, including what each tool does, the inputs it typically expects, and when to use it.

## Tool selection

| Use | For |
| :--- | :--- |
| `memory_*` | Durable facts, preferences, decisions, and useful outcomes you want to recall across agents. |
| `knowledgebase_*` | Reusable indexed documents and document retrieval across agents. |
| `db_*` | Structured operational data you want to query or modify with SQL. |
| `conversation_*` | Full saved chat history and thread replay. |
| `datapack_*` | Provisioning and managing Meko workspaces/environments. |

## Conversation tools

| Tool | What it does | Typical inputs | When to use it |
|---|---|---|---|
| `conversation_create` | Creates a stored conversation container. | `agent_id`, `title`, `user_id`, `app_id`, `run_id`, `metadata` | When you want to save a chat/thread, not just a fact or document. |
| `conversation_add_message` | Adds a user/assistant turn to a stored conversation. | `conversation_id`, `agent_id`, `input`, `output`, `reasoning`, `metadata` | When preserving full exchanges rather than storing long-term facts. |
| `conversation_get` | Fetches a stored conversation, optionally with messages. | `conversation_id`, `agent_id`, `include_messages` | When reviewing or replaying a past conversation. |
| `conversation_list` | Lists stored conversations. | `agent_id`, `user_id`, `limit`, `offset` | When browsing saved chat history. |
| `conversation_update` | Updates a conversation title or metadata. | `conversation_id`, `agent_id`, `title`, `metadata` | When organizing or relabeling stored conversations. |
| `conversation_delete` | Permanently deletes a stored conversation. | `conversation_id`, `agent_id` | When you intentionally want to remove saved chat history. |

## Memory tools

| Tool | What it does | Typical inputs | When to use it |
|---|---|---|---|
| `memory_add` | Stores durable facts, preferences, decisions, or useful outcomes. | `agent_id`, `conversation_id`, `text`, `user_id`, `app_id`, `run_id`, `metadata` | When you want long-term reusable memory across threads. |
| `memory_search` | Searches stored memories semantically and returns related graph relations. | `agent_id`, `conversation_id`, `query`, `user_id`, `limit` | When recalling prior facts, preferences, or decisions. |
| `memory_get_all` | Lists all memories for an agent or user scope. | `agent_id`, `conversation_id`, `user_id`, `app_id`, `run_id` | When auditing or inspecting what has been remembered. |
| `memory_get_by_id` | Retrieves one memory by ID. | `agent_id`, `conversation_id`, `memory_id` | When you already know the exact memory to inspect. |
| `memory_update` | Replaces the text of an existing memory. | `agent_id`, `conversation_id`, `memory_id`, `text` | When a remembered fact needs correction. |
| `memory_delete_by_id` | Deletes one memory. | `agent_id`, `conversation_id`, `memory_id` | When a specific stored memory is no longer wanted. |
| `memory_delete_all` | Deletes all memories in a scope. | `agent_id`, `conversation_id`, `user_id`, `app_id`, `run_id` | When doing a full reset or broad cleanup. |

## Knowledgebase tools

| Tool | What it does | Typical inputs | When to use it |
|---|---|---|---|
| `knowledgebase_trigger_index_creation` | Creates a new indexed document knowledgebase and starts ingestion. | `conversation_id`, `source_uris`, `index_name`, `ai_provider`, `embedding_model`, `embedding_dimensions`, `chunk_size`, `chunk_overlap`, `datapack_id` | When you want reusable document retrieval across threads. |
| `knowledgebase_add_source_to_index` | Adds another document source to an existing index. | `conversation_id`, `index_name`, `source_id`, `chunk_size`, `chunk_overlap`, `datapack_id` | When expanding an existing knowledgebase instead of creating a duplicate. |
| `knowledgebase_check_index_status` | Checks indexing/build progress and readiness. | `conversation_id`, `index_name`, `datapack_id` | Before assuming a knowledgebase is ready for retrieval. |

## Database tools

| Tool | What it does | Typical inputs | When to use it |
|---|---|---|---|
| `db_run_read_only_query` | Runs a read-only SQL query and returns JSON results. | `conversation_id`, `query`, `datapack_id` | When you need direct structured data access and know the SQL to run. |
| `db_run_write_query` | Runs a write SQL statement in a transaction. | `conversation_id`, `query`, `datapack_id` | When inserting, updating, or deleting structured data. |
| `db_summarize_database` | Summarizes tables, schema, and row counts. | `conversation_id`, `schema`, `datapack_id` | When exploring a datapack database before writing queries. |

## Datapack tools

| Tool | What it does | Typical inputs | When to use it |
|---|---|---|---|
| `datapack_create` | Provisions a new datapack/workspace with DB and MCP details. | `name` | When setting up a new isolated Meko environment. |
| `datapack_describe` | Returns datapack details and optional status. | `name`, `include_status` | When inspecting an existing datapack. |
| `datapack_list` | Lists available datapacks. | None | When you want to see all workspaces/environments. |
| `datapack_update` | Updates a datapack's database connection string. | `name`, `connection_string` | When repointing a datapack to a new database connection. |
| `datapack_delete` | Permanently deletes a datapack. | `name` | When removing an environment entirely. |

## Fast rule of thumb

- "Remember this about me or the project" -> `memory_*`
- "Index these docs for reuse" -> `knowledgebase_*`
- "Run SQL on my data" -> `db_*`
- "Save or retrieve this chat" -> `conversation_*`
- "Create or manage a workspace" -> `datapack_*`


## Health

### Health check

```text
GET /health
```

**Auth:** Not required

**Responses:** `200 OK`

---

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
