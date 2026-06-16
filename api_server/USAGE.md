# Meko API Server Usage

## Starting the Server

```bash
./meko-api-server [flags]
```

### Flags

| Flag                | Default       | Description                      |
|---------------------|---------------|----------------------------------|
| `-port`             | `25433`       | API server listen port           |
| `-db-host`          | `127.0.0.1`   | Database host                    |
| `-db-port`          | `5433`        | Database port                    |
| `-db-user-name`     | `yugabyte`    | Database user name               |
| `-db-password`      | (empty)       | Database password                |
| `-cognito-region`   | (empty)       | AWS region for Cognito User Pool |
| `-cognito-pool-id`  | (empty)       | Cognito User Pool ID             |
| `-cognito-client-id`| (empty)       | Cognito App Client ID            |
| `-cognito-client-secret`| (empty)  | Cognito App Client secret        |
| `-cognito-domain`       | (empty)  | Cognito Hosted UI domain (e.g. `myapp.auth.us-east-2.amazoncognito.com`) |

### Example

```bash
./meko-api-server -port 25433 -db-host 127.0.0.1 -db-port 5433 -db-user-name yugabyte -db-password secret
```

The server runs database migrations on startup, then listens on the configured port.

### Running the migration

```bash
# Run migrations only, then exit
./meko-api-server -migrate-only -db-host 127.0.0.1 -db-port 5433

```

### Running with cognito

```bash
./meko-api-server \
  --port 25433 \
  --db-host 127.0.0.1 \
  --cognito-region us-east-1 \
  --cognito-pool-id us-east-1_XXXXX \
  --cognito-client-id abc123def456 \
  --cognito-client-secret your-client-secret \
  --cognito-domain myapp.auth.us-east-1.amazoncognito.com
```

The `--cognito-domain` flag is required for the `/success` callback endpoint to exchange OAuth2 authorization codes. The value is the full domain from **Cognito Console > App Integration > Domain name** (without `https://`).

### Docker

```bash
docker build -t meko/meko-api-server:latest api_server/

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

### Authentication

When Cognito flags are provided, all `/datapacks` endpoints require a valid JWT in the `Authorization` header. The `/auth` endpoints are always public.

#### `POST /api/v1/auth/signup` — Register a new user

**Request body:**

| Field        | Type   | Required | Description       |
|--------------|--------|----------|-------------------|
| `email`      | string | yes      | User email        |
| `password`   | string | yes      | User password     |
| `first_name` | string | yes      | First name        |
| `last_name`  | string | yes      | Last name         |

```bash
curl -X POST http://localhost:25433/api/v1/auth/signup \
  -H "Content-Type: application/json" \
  -d '{
    "email": "jane@example.com",
    "password": "SecureP@ss1",
    "first_name": "Jane",
    "last_name": "Doe"
  }'
```

**Response (201):**

```json
{
  "data": {
    "account_id": "uuid",
    "email": "jane@example.com",
    "message": "confirmation code sent to jane@example.com"
  }
}
```

---

#### `POST /api/v1/auth/confirm-signup` — Confirm email

**Request body:**

| Field   | Type   | Required | Description                   |
|---------|--------|----------|-------------------------------|
| `email` | string | yes      | User email                    |
| `code`  | string | yes      | 6-digit code from email       |

```bash
curl -X POST http://localhost:25433/api/v1/auth/confirm-signup \
  -H "Content-Type: application/json" \
  -d '{"email": "jane@example.com", "code": "123456"}'
```

**Response (200):**

```json
{
  "data": { "message": "email confirmed" }
}
```

---

#### `POST /api/v1/auth/login` — Authenticate and receive tokens

**Request body:**

| Field      | Type   | Required | Description   |
|------------|--------|----------|---------------|
| `email`    | string | yes      | User email    |
| `password` | string | yes      | User password |

```bash
curl -X POST http://localhost:25433/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email": "jane@example.com", "password": "SecureP@ss1"}'
```

**Response (200):**

```json
{
  "data": {
    "access_token": "eyJ...",
    "id_token": "eyJ...",
    "refresh_token": "eyJ...",
    "expires_in": 3600
  }
}
```

---

#### `POST /api/v1/auth/logout` — Invalidate all sessions

Requires the access token in the `Authorization` header.

```bash
curl -X POST http://localhost:25433/api/v1/auth/logout \
  -H "Authorization: Bearer <access_token>"
```

**Response (200):**

```json
{
  "data": { "message": "logged out" }
}
```

---

#### `GET /success` — Cognito Hosted UI callback

This endpoint handles the redirect from the Cognito Managed Login (Hosted UI) after a user signs up or logs in. Cognito redirects to this URL with an authorization code in the query string.

**Flow:**

1. User signs up or logs in via the Cognito Hosted UI
2. Cognito redirects to `https://mekodev.com/success?code=<authorization_code>`
3. The server exchanges the code for tokens by calling Cognito's `/oauth2/token` endpoint
4. A local account is created automatically (JIT provisioning) if one doesn't exist
5. A success page is rendered

**Cognito configuration required:**

| Setting                     | Value                                     |
|-----------------------------|-------------------------------------------|
| Allowed callback URLs       | `https://mekodev.com/success`             |
| Allowed OAuth Flows         | Authorization code grant                  |
| Allowed OAuth Scopes        | `openid`, `email`, `profile`              |

**Hosted UI signup link for users:**

```
https://<cognito-domain>/signup?client_id=<client-id>&response_type=code&scope=openid+email+profile&redirect_uri=https://mekodev.com/success
```

After the callback completes successfully, users can authenticate with the CLI using `POST /api/v1/auth/login` with their email and password.

---

#### Using authenticated endpoints

After login, include the access token in all `/datapacks` requests:

```bash
curl http://localhost:25433/api/v1/datapacks \
  -H "Authorization: Bearer <access_token>"
```

Unauthenticated requests return `401`:

```json
{
  "data": null,
  "error": {
    "code": "UNAUTHORIZED",
    "message": "missing or malformed authorization header"
  }
}
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

#### `GET /api/v1/datapacks/:datapack_id` — Describe a datapack

Datapacks are addressed by their UUID (`datapack_id`, returned from `POST /datapacks`), not by their name.

```bash
curl http://localhost:25433/api/v1/datapacks/<datapack-id>
```

Returns the full datapack object. `404` if not found.

---

#### `PATCH /api/v1/datapacks/:datapack_id` — Update a datapack

**Request body (all fields optional):**

| Field             | Type   | Description              |
|-------------------|--------|--------------------------|
| `llm_model`       | string | New LLM model name       |
| `embedding_model` | string | New embedding model name  |

```bash
curl -X PATCH http://localhost:25433/api/v1/datapacks/<datapack-id> \
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

#### `DELETE /api/v1/datapacks/:datapack_id` — Delete a datapack

```bash
curl -X DELETE http://localhost:25433/api/v1/datapacks/<datapack-id>
```

Cascades to agents and knowledge bases. `404` if not found.

**Response (200):**

```json
{
  "data": { "message": "datapack deleted" }
}
```

---

#### `GET /api/v1/datapacks/:datapack_id/status` — Datapack status

```bash
curl http://localhost:25433/api/v1/datapacks/<datapack-id>/status
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

#### `POST /api/v1/datapacks/:datapack_id/agents` — Create an agent

**Request body:**

| Field            | Type   | Required | Description                    |
|------------------|--------|----------|--------------------------------|
| `agent_name`     | string | yes      | Unique name within the datapack|
| `agent_metadata` | string | no       | JSON metadata string           |

```bash
curl -X POST http://localhost:25433/api/v1/datapacks/<datapack-id>/agents \
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

#### `GET /api/v1/datapacks/:datapack_id/agents` — List agents

```bash
curl http://localhost:25433/api/v1/datapacks/<datapack-id>/agents
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

#### `DELETE /api/v1/datapacks/:datapack_id/agents/:agent_id` — Delete an agent

```bash
curl -X DELETE http://localhost:25433/api/v1/datapacks/<datapack-id>/agents/<agent-id>
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

#### `POST /api/v1/datapacks/:datapack_id/knowledge-bases` — Add a knowledge base

**Request body:**

| Field | Type   | Required | Description          |
|-------|--------|----------|----------------------|
| `url` | string | yes      | Knowledge base URL   |

```bash
curl -X POST http://localhost:25433/api/v1/datapacks/<datapack-id>/knowledge-bases \
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

#### `GET /api/v1/datapacks/:datapack_id/knowledge-bases` — List knowledge bases

```bash
curl http://localhost:25433/api/v1/datapacks/<datapack-id>/knowledge-bases
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

#### `DELETE /api/v1/datapacks/:datapack_id/knowledge-bases/:rag_source_id` — Delete a knowledge base

The knowledge base is identified by its `rag_source_id` (UUID) in the path.

```bash
curl -X DELETE http://localhost:25433/api/v1/datapacks/<datapack-id>/knowledge-bases/<rag-source-id>
```

**Response (200):**

```json
{
  "data": { "message": "knowledge base deleted" }
}
```

---

## Running Tests

All tests are run from the `api_server/` directory.

### Unit tests (no database required)

These tests use mocked dependencies and run quickly without any external services.

```bash
# Auth package -- Cognito client, JWT validation, middleware
go test ./internal/auth/ -v

# Auth service -- signup, login, logout business logic
go test ./internal/service/ -run TestAuthService -v

# Auth handler -- HTTP endpoint tests
go test ./internal/handlers/ -run TestAuth_ -v
```

### Integration tests (requires YugabyteDB)

These tests need a running YugabyteDB instance on `localhost:5433`. They exercise the full stack including database operations.

```bash
# All handler integration tests (datapacks, agents, knowledge bases)
go test ./internal/handlers/ -v

# Router and middleware integration tests
go test ./internal/api/ -v
```

### Run all tests

```bash
go test ./... -v
```

### Run tests with count (no caching)

```bash
go test ./... -v -count=1
```

### Run a specific test by name

```bash
go test ./internal/auth/ -run TestValidateAccessToken_Valid -v
go test ./internal/handlers/ -run TestAuth_Login_Success -v
```

### Linting

Run the project linter before committing:

```bash
golangci-lint run ./...
```
