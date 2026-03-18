# Meko API Server Usage

## Starting the Server

```bash
./meko-api-server [flags]
```

### Flags

| Flag             | Default       | Description            |
|------------------|---------------|------------------------|
| `-port`          | `25433`       | API server listen port |
| `-db-host`       | `127.0.0.1`   | Database host          |
| `-db-port`       | `5433`        | Database port          |
| `-db-user-name`  | `yugabyte`    | Database user name     |
| `-db-password`   | (empty)       | Database password      |

### Example

```bash
./meko-api-server -port 25433 -db-host 127.0.0.1 -db-port 5433 -db-user-name yugabyte -db-password secret
```

The server runs database migrations on startup, then listens on the configured port.

## Running the migration

```bash
# Run migrations only, then exit
./meko-api-server -migrate-only -db-host 127.0.0.1 -db-port 5433

```

---

## Base URL

```
http://localhost:25433/api/v1
```

## Response Envelope

All endpoints return a JSON envelope:

```json
{
  "data": { ... },
  "error": null
}
```

On error:

```json
{
  "data": null,
  "error": {
    "code": "VALIDATION_ERROR",
    "message": "name is required"
  }
}
```

## Rate Limiting

Management endpoints are rate-limited to 10 req/s sustained with a burst of 20, per client IP. Exceeding the limit returns `429`:

```json
{
  "status": "error",
  "code": "RATE_LIMITED",
  "error": "too many requests — try again shortly"
}
```

---

## Swagger

Interactive API docs are available at:

```
http://localhost:25433/swagger/index.html
```

---

## Endpoints

### Health

#### `GET /api/v1/health`

Returns server and database health status.

```bash
curl http://localhost:25433/api/v1/health
```

---

### Datapacks

#### `POST /api/v1/datapacks` — Create a datapack

**Request body:**

| Field                | Type   | Required | Description                     |
|----------------------|--------|----------|---------------------------------|
| `name`               | string | yes      | Unique datapack name            |
| `seed_knowledge_base`| string | yes      | URL of the seed knowledge base  |
| `embed_model`        | string | no       | Embedding model name            |
| `inference_model`    | string | no       | Inference/LLM model name        |

```bash
curl -X POST http://localhost:25433/api/v1/datapacks \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-datapack",
    "seed_knowledge_base": "https://example.com/docs",
    "embed_model": "text-embedding-3-small",
    "inference_model": "gpt-4"
  }'
```

**Response (201):**

```json
{
  "data": {
    "datapack_id": "uuid",
    "agent_id": "uuid",
    "name": "my-datapack",
    "mcp_server": {
      "url": "https://mekodev.com/<account-id>/mcp",
      "api_key": "generated-hex-key"
    }
  }
}
```

A default agent (`meko_user_agent`) is automatically created and linked to the seed knowledge base.

---

#### `GET /api/v1/datapacks` — List all datapacks

```bash
curl http://localhost:25433/api/v1/datapacks
```

**Response (200):**

```json
{
  "data": [
    {
      "id": "uuid",
      "name": "my-datapack",
      "account_id": "uuid",
      "llm_model": "gpt-4",
      "embedding_model": "text-embedding-3-small",
      "created_at": "2026-03-17T00:00:00Z",
      "updated_at": "2026-03-17T00:00:00Z"
    }
  ]
}
```

---

#### `GET /api/v1/datapacks/:name` — Describe a datapack

```bash
curl http://localhost:25433/api/v1/datapacks/my-datapack
```

Returns the full datapack object. `404` if not found.

---

#### `PATCH /api/v1/datapacks/:name` — Update a datapack

**Request body (all fields optional):**

| Field             | Type   | Description              |
|-------------------|--------|--------------------------|
| `llm_model`       | string | New LLM model name       |
| `embedding_model` | string | New embedding model name  |

```bash
curl -X PATCH http://localhost:25433/api/v1/datapacks/my-datapack \
  -H "Content-Type: application/json" \
  -d '{"llm_model": "gpt-4o"}'
```

**Response (200):**

```json
{
  "data": { "message": "datapack updated" }
}
```

---

#### `DELETE /api/v1/datapacks/:name` — Delete a datapack

```bash
curl -X DELETE http://localhost:25433/api/v1/datapacks/my-datapack
```

Cascades to agents and knowledge bases. `404` if not found.

**Response (200):**

```json
{
  "data": { "message": "datapack deleted" }
}
```

---

#### `GET /api/v1/datapacks/:name/status` — Datapack status

```bash
curl http://localhost:25433/api/v1/datapacks/my-datapack/status
```

**Response (200):**

```json
{
  "data": {
    "id": "uuid",
    "name": "my-datapack",
    "llm_model": "gpt-4",
    "embedding_model": "text-embedding-3-small",
    "created_at": "2026-03-17T00:00:00Z",
    "agent_count": 1,
    "knowledge_base_count": 2
  }
}
```

---

### Agents

Agents are scoped to a datapack. A default agent (`meko_user_agent`) is created automatically when a datapack is created.

#### `POST /api/v1/datapacks/:name/agents` — Create an agent

**Request body:**

| Field            | Type   | Required | Description                    |
|------------------|--------|----------|--------------------------------|
| `agent_name`     | string | yes      | Unique name within the datapack|
| `agent_metadata` | string | no       | JSON metadata string           |

```bash
curl -X POST http://localhost:25433/api/v1/datapacks/my-datapack/agents \
  -H "Content-Type: application/json" \
  -d '{"agent_name": "research-agent"}'
```

**Response (201):**

```json
{
  "data": {
    "agent_id": "uuid",
    "agent_name": "research-agent",
    "datapack_name": "my-datapack"
  }
}
```

---

#### `GET /api/v1/datapacks/:name/agents` — List agents

```bash
curl http://localhost:25433/api/v1/datapacks/my-datapack/agents
```

**Response (200):**

```json
{
  "data": [
    {
      "id": "uuid",
      "datapack_id": "uuid",
      "agent_name": "meko_user_agent",
      "agent_metadata": "{}",
      "created_at": "2026-03-17T00:00:00Z",
      "updated_at": "2026-03-17T00:00:00Z"
    }
  ]
}
```

---

#### `DELETE /api/v1/datapacks/:name/agents/:agent` — Delete an agent

```bash
curl -X DELETE http://localhost:25433/api/v1/datapacks/my-datapack/agents/research-agent
```

**Response (200):**

```json
{
  "data": { "message": "agent deleted" }
}
```

---

### Knowledge Bases

Knowledge bases are scoped to a datapack. A seed knowledge base is added during datapack creation.

#### `POST /api/v1/datapacks/:name/knowledge-bases` — Add a knowledge base

**Request body:**

| Field | Type   | Required | Description          |
|-------|--------|----------|----------------------|
| `url` | string | yes      | Knowledge base URL   |

```bash
curl -X POST http://localhost:25433/api/v1/datapacks/my-datapack/knowledge-bases \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com/more-docs"}'
```

**Response (201):**

```json
{
  "data": {
    "knowledge_base_id": "uuid",
    "url": "https://example.com/more-docs",
    "datapack_name": "my-datapack"
  }
}
```

---

#### `GET /api/v1/datapacks/:name/knowledge-bases` — List knowledge bases

```bash
curl http://localhost:25433/api/v1/datapacks/my-datapack/knowledge-bases
```

**Response (200):**

```json
{
  "data": [
    {
      "id": "uuid",
      "datapack_id": "uuid",
      "knowledge_base_url": "https://example.com/docs",
      "created_at": "2026-03-17T00:00:00Z",
      "updated_at": "2026-03-17T00:00:00Z"
    }
  ]
}
```

---

#### `DELETE /api/v1/datapacks/:name/knowledge-bases` — Delete a knowledge base

**Request body:**

| Field | Type   | Required | Description                   |
|-------|--------|----------|-------------------------------|
| `url` | string | yes      | URL of the knowledge base     |

```bash
curl -X DELETE http://localhost:25433/api/v1/datapacks/my-datapack/knowledge-bases \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com/more-docs"}'
```

**Response (200):**

```json
{
  "data": { "message": "knowledge base deleted" }
}
```
