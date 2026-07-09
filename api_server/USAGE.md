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

---

## Environment Variables

Every flag that has an env-var equivalent is listed here. When both a flag and an env var are set, the flag takes precedence. Variables with no default shown are **required** when their feature is enabled.

### Logging

| Variable | Default | Description |
|---|---|---|
| `LOG_LEVEL` | `info` | slog level: `debug`, `info`, `warn`, `error` |
| `MEKO_DEPLOY_ENV` | `prod` | Deployment environment: `prod` or `dev` |

### System (control-plane) Database

The `meko_system.*` schema can live on a separate database from the data-plane. Leave these unset to inherit the `--db-*` values (single-database deployments).

| Variable | Default | Description |
|---|---|---|
| `SYSTEM_DB_HOST` | inherits `--db-host` | meko_system database host |
| `SYSTEM_DB_USER_NAME` | inherits `--db-user-name` | meko_system database user |
| `SYSTEM_DB_PASSWORD` | inherits `--db-password` | meko_system database password |
| `SYSTEM_DB_PORT` | inherits `--db-port` | meko_system database port |
| `SYSTEM_DB_NAME` | inherits `--db-name` | meko_system database name |
| `SYSTEM_DB_SSLMODE` | inherits `--db-sslmode` | meko_system database sslmode |

### Langfuse

| Variable | Default | Description |
|---|---|---|
| `LANGFUSE_HOST` | — | Langfuse base URL (e.g. `http://localhost:3000`) |
| `LANGFUSE_ADMIN_API_KEY` | — | Langfuse admin API key for per-account org/project provisioning |
| `LANGFUSE_DB_HOST` | — | Langfuse Postgres host (optional; required for observability endpoints) |
| `LANGFUSE_DB_PORT` | `5433` | Langfuse Postgres port |
| `LANGFUSE_DB_NAME` | `langfuse` | Langfuse Postgres database name |
| `LANGFUSE_DB_USER` | `yugabyte` | Langfuse Postgres user |
| `LANGFUSE_DB_PASSWORD` | — | Langfuse Postgres password |
| `LANGFUSE_DB_SSLMODE` | `disable` | Langfuse Postgres sslmode (`disable`, `require`, `verify-ca`, `verify-full`) |

### OpenAI

| Variable | Default | Description |
|---|---|---|
| `OPENAI_API_KEY` | — | OpenAI API key |
| `OPEN_AI_LLM_MODEL` | `gpt-4o` | OpenAI LLM model |
| `OPEN_AI_EMBEDDING_MODEL` | `text-embedding-3-small` | OpenAI embedding model |
| `MEMORY_LIST_SKIP_VECTOR_SORT` | `true` | Skip vector embedding for memory list; sort by `created_at DESC` instead |

### S3

| Variable | Default | Description |
|---|---|---|
| `S3_AUTH_MODE` | `minio` | S3 auth mode: `minio`, `iam-user`, `iam-role` |
| `S3_ENDPOINT` | `http://localhost:19094` | S3-compatible endpoint URL (minio / iam-user modes) |
| `S3_ACCESS_KEY` | `meko` | S3 access key (minio / iam-user modes) |
| `S3_SECRET_KEY` | `mekosecret` | S3 secret key (minio / iam-user modes) |
| `S3_BUCKET` | `meko-kb-uploads` | S3 bucket for KB file uploads |
| `S3_REGION` | `us-east-2` | S3 region |

### Email (SES)

| Variable | Default | Description |
|---|---|---|
| `SES_FROM_ADDRESS` | — | Verified SES sender address (e.g. `no-reply@example.com`). Empty disables invite emails. |
| `SES_REGION` | inherits `--cognito-region` | AWS region for SES |

### Stripe / Billing

All five Stripe variables must be set for the `/billing/*` routes to register. With any blank value the routes stay off and upgrade buttons are disabled.

| Variable | Default | Description |
|---|---|---|
| `STRIPE_SECRET_KEY` | — | Stripe API secret key (`sk_live_...` or `sk_test_...`) |
| `STRIPE_WEBHOOK_SECRET` | — | Stripe webhook signing secret (`whsec_...`) |
| `STRIPE_PRICE_STARTER` | — | Stripe Price ID for the Starter plan (`price_...`) |
| `STRIPE_PRICE_GROWTH` | — | Stripe Price ID for the Growth plan (`price_...`) |
| `STRIPE_PRICE_SCALE` | — | Stripe Price ID for the Scale plan (`price_...`) |

### MCP / PAT

| Variable | Default | Description |
|---|---|---|
| `MCP_INTERNAL_SECRET` | — | Shared secret for `/internal/meko-tokens/validate` (MCP server → api_server). Empty disables the endpoint. |
| `MCP_PAT_FLUSH_INTERVAL` | `30s` | Cadence for flushing PAT `last_used_at` touches to the DB. Accepts Go duration strings (e.g. `30s`, `2m`). Values < 1s are clamped to 1s. |
| `PAT_RATE_LIMIT_PER_MIN` | `60` | Max requests per minute per PAT on rate-limited routes. `0` or negative disables the limiter. |
| `PAT_RATE_LIMIT_BURST` | `10` | Burst allowance on top of `PAT_RATE_LIMIT_PER_MIN`. |

### Free-Tier Caps

Applied only to accounts with `user_type = "free"`. Mirrored in `meko_ui/src/lib/freeTier.ts` — both sides read the same env vars.

| Variable | Default | Description |
|---|---|---|
| `MEKO_FREE_TIER_DATAPACKS` | `10` | Max datapacks per free account |
| `MEKO_FREE_TIER_KB_BYTES` | `104857600` (100 MiB) | Max total KB storage in bytes per free account |
| `MEKO_FREE_TIER_CONVERSATIONS` | `1000` | Max conversations per free account |
| `MEKO_FREE_TIER_TOKEN_BURN` | `10000000` | Max token burn per free account |

### Artifacts

| Flag | Variable | Default | Description |
|---|---|---|---|
| `--max-artifact-size-bytes` | `MEKO_MAX_ARTIFACT_SIZE_BYTES` | `5242880` (5 MiB) | Maximum decoded file size accepted by `POST /datapacks/:id/artifacts`. Files exceeding this limit return `413`. |

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
