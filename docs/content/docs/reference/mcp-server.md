---
title: Meko MCP Server
linkTitle: Meko MCP Server
description: MCP SERVER and MCP Tools description
weight: 40
---

The Meko Datapack MCP Server provides the primary tools for managing datapacks, agents, knowledge bases, and connecting to the postgreSQL compatible database YugabyteDB for executing database commands

**Base URL:** `/mcp/v1`

## Authentication

Most endpoints require a Bearer token obtained from the login endpoint.

```text
Authorization: Bearer {access_token}
```

## Auth Endpoints

### Sign Up

Register a new user account.

```text
POST /auth/signup
```

**Request body:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `email` | string | Yes | User email address |
| `password` | string | Yes | Account password |
| `first_name` | string | Yes | First name |
| `last_name` | string | Yes | Last name |

**Responses:** `201 Created`, `400 Bad Request`, `409 Conflict`

### Confirm Sign Up

Confirm email with verification code.

```text
POST /auth/confirm-signup
```

**Request body:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
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
|-------|------|----------|-------------|
| `email` | string | Yes | User email address |
| `password` | string | Yes | Account password |

**Response body (200):**

| Field | Type | Description |
|-------|------|-------------|
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

## Datapack Endpoints

### List All Datapacks

```text
GET /datapacks
```

**Auth:** Required

**Responses:** `200 OK`

### Create a Datapack

```text
POST /datapacks
```

**Auth:** Required

**Request body:**

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Datapack name |
| `account_id` | string | Account ID |

**Responses:** `201 Created`, `400 Bad Request`

### Describe a Datapack

```text
GET /datapacks/{name}
```

**Auth:** Required

**Path parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `name` | string | Datapack name |

**Responses:** `200 OK`, `404 Not Found`

### Update a Datapack

```text
PATCH /datapacks/{name}
```

**Auth:** Required

**Path parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `name` | string | Datapack name |

**Request body:**

| Field | Type | Description |
|-------|------|-------------|
| `datapack_ysql_connection_string` | string | Updated connection string |

**Responses:** `200 OK`, `404 Not Found`

### Delete a Datapack

```text
DELETE /datapacks/{name}
```

**Auth:** Required

**Path parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `name` | string | Datapack name |

**Responses:** `200 OK`, `404 Not Found`

### Get Datapack Status

```text
GET /datapacks/{name}/status
```

**Auth:** Required

**Path parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `name` | string | Datapack name |

**Responses:** `200 OK`, `404 Not Found`

---

## Agent Endpoints

### List Agents in a Datapack

```text
GET /datapacks/{name}/agents
```

**Auth:** Required

**Path parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `name` | string | Datapack name |

**Responses:** `200 OK`

### Create an Agent

```text
POST /datapacks/{name}/agents
```

**Auth:** Required

**Path parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `name` | string | Datapack name |

**Request body:**

| Field | Type | Description |
|-------|------|-------------|
| `agent_name` | string | Agent name |
| `agent_metadata` | string | Optional agent metadata |

**Responses:** `201 Created`, `400 Bad Request`, `404 Not Found`

### Delete an Agent

```text
DELETE /datapacks/{name}/agents/{agent}
```

**Auth:** Required

**Path parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `name` | string | Datapack name |
| `agent` | string | Agent name |

**Responses:** `200 OK`, `404 Not Found`

---

## Knowledge Base Endpoints

### List Knowledge Bases

```text
GET /datapacks/{name}/knowledge-bases
```

**Auth:** Required

**Path parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `name` | string | Datapack name |

**Responses:** `200 OK`

### Add a Knowledge Base

```text
POST /datapacks/{name}/knowledge-bases
```

**Auth:** Required

**Path parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `name` | string | Datapack name |

**Request body:**

| Field | Type | Description |
|-------|------|-------------|
| `url` | string | Source URL (S3, local path, NFS) |

**Responses:** `201 Created`, `400 Bad Request`, `404 Not Found`

### Delete a Knowledge Base

```text
DELETE /datapacks/{name}/knowledge-bases
```

**Auth:** Required

**Path parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `name` | string | Datapack name |

**Request body:**

| Field | Type | Description |
|-------|------|-------------|
| `url` | string | URL of knowledge base to delete |

**Responses:** `200 OK`, `400 Bad Request`, `404 Not Found`

---

## Health

### Health Check

```text
GET /health
```

**Auth:** Not required

**Responses:** `200 OK`

---

## Common Response Format

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
