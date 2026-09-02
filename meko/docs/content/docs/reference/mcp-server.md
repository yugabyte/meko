---
title: Meko MCP Server
linkTitle: Meko MCP Server
description: MCP server and tools reference
weight: 40
---

The Meko MCP Server provides the primary tools for managing datapacks, conversations, memories, knowledge base, and related search.

To connect from IDEs and MCP-compatible clients, use the Meko MCP URL `https://mcp.mekodata.ai/mcp` and a user **API key** from **Settings>API Keys**. See [Integrations](../../integrations/) and [Quick start](../../quick-start/).

## Free tier quotas

On the **Free** tier, MCP tool calls are metered per account:

| Metric | Limit (per month) | Example tools |
| :----- | :--------- | :------------ |
| Conversations | 1,000 | `conversation_add_message` |
| Retrievals | 10,000 | `memory_search`, `knowledgebase_search` |

When a quota is exceeded, further calls are blocked until you upgrade. Datapack creation and knowledge-base uploads are capped separately. See [Meko pricing](https://mekodata.ai/pricing/) for the full plan table.

## Tools

The Meko MCP Server provides 23 tools by default. Experimental conversation-search tools are hidden unless a deployment explicitly enables them.

Authorization comes from the authenticated user and their datapack grants.

> [!WARNING] Breaking change in v2.1.0
>
> Tool-level `scope` arguments (`"read"`, `"write"`, `"admin"`) were retired. Don't send them on MCP tool calls. New clients no longer see `scope` in tool schemas. A temporary compatibility shim currently strips the obsolete argument from older clients and will be removed in a future release. Rerun the installer or install the current plugin from [`yugabyte/meko-skills`](https://github.com/yugabyte/meko-skills) so hooks and skills stop sending it. Installer flags such as `--scope user|project` are unrelated and remain supported.

| Use | For |
| :--- | :--- |
| [Conversation](#conversation-tools)<br>`conversation_*` | Full saved chat history and thread replay.<br><br>Example: _Save or retrieve this chat_ |
| [Memory](#memory-tools)<br>`memory_*` | Durable facts, preferences, decisions, and useful outcomes you want to recall across agents.<br><br>Example: _Remember this about me or the project_ |
| [Knowledge base](#knowledgebase-tools)<br>`knowledgebase_*` | Reusable, persistent, indexed shared documents, and collective memories that can be searched across agents.<br><br>Example: _Search this in the knowledge base_ |
| [Datapack](#datapack-tools)<br>`datapack_*` | Provisioning and managing Meko workspaces/environments.<br><br>Example: _Create or manage a workspace_ |
| [Artifact](#artifact-tools)<br>`artifact_*` | Uploading and retrieving files (reports, CSVs, PDFs, code output) for later or cross-agent access.<br><br>Example: _Save this file for later_ |

<!-- 
| [Database](#database-tools)<br>`db_*` | Structured operational data you want to query or modify with SQL.<br><br>Example: _Run SQL on my data_ |
-->

### Conversation tools

| Tool | Description |
| :--- | :---------- |
| `conversation_create` | Create a stored conversation container.<br>Use this to save a chat/thread, not just a fact or document.<br>Typical inputs: `agent_id, session_id, datapack_id, title, run_id, metadata` |
| `conversation_add_message` | Add a user/assistant turn to a stored conversation. The optional `plan` is an array of action strings.<br>Use this to preserve full exchanges; Meko asynchronously extracts durable facts from the saved user turn.<br>Typical inputs: `conversation_id, input, output, agent_id, reasoning, plan, metadata, seed, datapack_id` |
| `conversation_get` | Fetch an agent-owned conversation, optionally with messages.<br>The supplied `agent_id` must own the conversation. Use this to review or replay a past conversation.<br>Typical inputs: `conversation_id, agent_id, include_messages, limit, offset, datapack_id` |
| `conversation_list` | List conversations for a datapack across the current user's agent namespaces.<br>`agent_id` is used for trace attribution, not result filtering.<br>Typical inputs: `datapack_id, conversation_id, agent_id, limit, offset` |
| `conversation_update` | Update an agent-owned conversation's title or metadata.<br>Use this when organizing or relabeling stored conversations.<br>Typical inputs: `conversation_id, agent_id, title, metadata, datapack_id` |
| `conversation_delete` | Permanently delete an agent-owned conversation and all its messages.<br>Use this to remove saved conversation history.<br>Typical inputs: `conversation_id, agent_id, datapack_id` |
| `track_token_usage` | Record a token-usage observation against a conversation trace, so that LLM, embedding, or extraction spend is attributed for free-tier accounting and dashboards.<br>Use this to pair a billable model call with a recorded usage entry.<br>Typical inputs: `conversation_id, name, input_tokens, output_tokens, total_tokens, model, message_id, datapack_id` |

### Memory tools

| Tool | Description |
| :--- | :---------- |
| `memory_add` | Store durable facts, preferences, decisions, or useful outcomes.<br>Use this for direct "remember this" requests, facts found only in assistant/tool output, and corrections. Saved conversation turns are extracted automatically.<br>Typical inputs: `text, conversation_id, agent_id, run_id, metadata, messages, datapack_id` |
| `memory_search` | Search the current user's memories across agent namespaces with semantic, keyword, and entity-aware ranking.<br>`agent_id` is used for trace attribution, not result filtering; don't fan out one search per agent.<br>`limit` sets the maximum number of returned results (default: `10`). Keep the result set small enough for the model context.<br>A response with `error` or an error-shaped `detail` means the search failed. Only a successful response with an empty `results` array means no matching memories were found.<br>Typical inputs: `query, conversation_id, agent_id, limit, datapack_id, run_id`|
| `memory_get_all` | Return a recent window of roughly 20 memories plus the total count across the current user's agent namespaces.<br>This is not a complete list or export and doesn't accept pagination arguments. Use `memory_search` for older memories or `memory_get_by_id` when you know the ID.<br>`agent_id` is used for trace attribution, not result filtering.<br>Typical inputs: `conversation_id, agent_id, datapack_id, promoted` |
| `memory_get_by_id` | Retrieve a single memory by ID.<br>Use this to inspect a memory.<br>Typical inputs: `memory_id, conversation_id, agent_id, datapack_id` |
| `memory_update` | Replace the text of an existing memory.<br>Use this to correct a remembered fact; `agent_id` must match the writer.<br>Typical inputs: `memory_id, text, conversation_id, agent_id, datapack_id` |
| `memory_delete_by_id` | Delete one memory.<br>Use this to remove a specific stored memory; `agent_id` must match the writer.<br>Typical inputs: `memory_id, conversation_id, agent_id, datapack_id` |
| `memory_delete_all` | Delete memories for an agent, optionally narrowed by run ID.<br>Use this for a full reset or broad cleanup.<br>Typical inputs: `conversation_id, agent_id, run_id, datapack_id` |
| `memory_promote` | Move memories into the datapack's shared knowledge layer, removing them from the user's private store.<br>Before this one-way action, preview the exact memories, explain team visibility and private-memory removal, and get explicit confirmation. Only datapack owners and maintainers may promote. See [Learnings](../../architecture/core-concepts/learnings/).<br>Typical inputs: `conversation_id, memory_ids, agent_id, datapack_id` |

### Knowledgebase tools

| Tool | Description |
| :--- | :---------- |
| `knowledgebase_search` | Perform hybrid semantic and keyword search over a datapack's knowledge base.<br>Use this to search uploaded and shared documents.<br>Typical inputs: `query, conversation_id, datapack_id, agent_id, limit` |

<!--
### Database tools

| Tool | Description |
| :--- | :---------- |
| `db_run_read_only_query` | Run a read-only SQL query and return JSON results.<br>Use this for direct structured data access using YSQL.<br>Typical inputs: `datapack_id, conversation_id, query` |
| `db_run_write_query` | Run a write SQL statement in a transaction.<br>Use this to insert, update, or delete structured data.<br>Typical inputs: `datapack_id, conversation_id, query` |
| `db_summarize_database` | Summarize tables, schema, and row counts.<br>Use this to explore a datapack's database before writing queries.<br>Typical inputs: `datapack_id, conversation_id, schema` |
-->

### Datapack tools

| Tool | Description |
| :--- | :---------- |
| `datapack_create` | Provision a new datapack.<br>Use this to set up a new isolated Meko environment.<br>Typical inputs: `name, conversation_id, datapack_id` |
| `datapack_describe` | Return datapack details and optional status.<br>Use this to inspect an existing datapack.<br>Typical inputs: `datapack_id, include_status, conversation_id` |
| `datapack_list` | List datapacks visible to the current user.<br>Use this to see all workspaces/environments.<br>Typical inputs: `conversation_id, datapack_id` |
| `datapack_update` | Update a datapack's name, description, and/or conversation-search opt-out.<br>At least one of `name`, `description`, or `conversation_search_opt_out` must be provided; clearing a description with an empty string isn't supported. Conversation search is on by default — opting out (restricted to the datapack's owner or a maintainer) is also available from the portal.<br>Typical inputs: `datapack_id, name, description, conversation_search_opt_out, conversation_id` |
| `datapack_delete` | Permanently delete a datapack.<br>This operation is irreversible and requires an explicit `datapack_id`.<br>Typical inputs: `datapack_id, conversation_id` |

### Artifact tools

| Tool | Description |
| :--- | :---------- |
| `artifact_put` | Upload a file artifact (report, CSV, PDF, code output, and so on) to a datapack for later retrieval or cross-agent sharing.<br>Use this when an agent has generated a file it wants to persist. Uploading the same bytes twice is idempotent and returns the same content hash, which is the lookup key for `artifact_get`.<br>Typical inputs: `filename, content_base64, content_type, conversation_id, datapack_id, agent_id` |
| `artifact_get` | Retrieve a previously uploaded artifact by its content hash.<br>Use this when an agent needs to read back a file it, or another agent, previously stored using `artifact_put`. Files smaller than 1 MB are returned inline; larger files are written to the client's local artifact directory and returned by path.<br>Typical inputs: `content_hash, conversation_id, datapack_id, agent_id` |

Files under 1 MB are stored inline in the database; files 1 MB and larger are stored in the datapack's object storage and written to a local path on retrieval. Artifacts are content-addressed by SHA-256, so uploads are idempotent. On the Free tier, artifacts are soft-expired 30 days after last access; Standard and Pro have no expiry. Artifact storage shares the knowledge-base caps (Free: 100 MB total / 5 MB per file; Standard: 5 GB / 50 MB; Pro: custom). Expired artifacts are recoverable by re-upload. See [Work with artifacts](../../guides/working-with-artifacts/).
