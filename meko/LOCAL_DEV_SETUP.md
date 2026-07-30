# Local Dev Setup (MinIO)

## Prereqs
YugabyteDB on `127.0.0.1:5433`, Docker, Go 1.25, Python 3.11
`rag_agent` venv, `~/code/meko/.env` populated.

### YugabyteDB:
1. (OPTIONAL, langfuse only): Create the langfuse database
1. Ensure ybdb has the gflag ysql_yb_enable_mage=true for master and tserver
1. enable the extensions: `create extension mage cascade; create extension pg_dist_rag cascade;`


## 1. MinIO

```bash
cd ~/code/meko/meko_ui
docker compose -f docker-compose.minio.yml up -d
```

S3 API `localhost:19094`, console `localhost:19095`, creds
`meko / mekosecret`, bucket `meko-kb-uploads` auto-created.

MinIO console (UI):

- `http://localhost:19095` (when using `meko_ui/docker-compose.minio.yml`)
- In the currently running Langfuse stack (`langfuse-minio-1`): `http://localhost:19091`

Login credentials:

- `meko / mekosecret` for `meko-minio`


Verify the container is up:

```bash
docker ps --format '{{.Names}}\t{{.Ports}}' | grep meko-minio
```

List objects in the bucket:

```bash
docker exec meko-minio mc alias set local http://localhost:9000 meko mekosecret
docker exec meko-minio mc ls --recursive local/meko-kb-uploads
```

## 2. `.env`

Source with `set -a; source ~/code/meko/.env; set +a` before launching
api_server or rag_agent.

```bash
# ---------- Local infrastructure -----------------------------------
MEKO_API_URL=http://localhost:25433
MEKO_MCP_URL=http://localhost:8000/mcp
LANGFUSE_HOST=http://localhost:3000
LANGFUSE_ADMIN_API_KEY=meko-local-admin-key

# Local YugabyteDB (meko meta DB)
META_HOST=127.0.0.1
META_PORT=5433
META_DB=yugabyte
META_USER=yugabyte
META_PW=yugabyte
# psql honors these without -h/-p/-U/-d flags
PGHOST=127.0.0.1
PGPORT=5433
PGUSER=yugabyte
PGPASSWORD=yugabyte
PGDATABASE=yugabyte

# Langfuse Postgres (separate YB universe)
LANGFUSE_DB_HOST=127.0.0.1
LANGFUSE_DB_PORT=5433
LANGFUSE_DB_NAME=langfuse
LANGFUSE_DB_USER=yugabyte
LANGFUSE_DB_PASSWORD=yugabyte

# ---------- Cognito (shared dev pool) -------------------------------
# api_server reads COGNITO_REGION / COGNITO_POOL_ID; meko-mcp-server's
# providers.py reads COGNITO_AWS_REGION / COGNITO_USER_POOL_ID. Set both.
COGNITO_REGION=us-east-2
COGNITO_AWS_REGION=us-east-2
COGNITO_POOL_ID=us-east-2_XXXXXXXXX
COGNITO_USER_POOL_ID=us-east-2_XXXXXXXXX
COGNITO_CLIENT_ID=<cognito-client-id>
COGNITO_CLIENT_SECRET=<cognito-client-secret>
COGNITO_DOMAIN=auth.mekodev.com

# ---------- meko-mcp-server -----------------------------------------
MEKO_BASE_URL=http://localhost:25433/api/v1
MCP_BASE_URL=http://localhost:8000

# mem0_client reads generic names (HOST/PORT/DBNAME/USER/PASSWORD) — NOT META_*.
# Re-export right before launching the MCP server, since USER collides with shell.
HOST=127.0.0.1
PORT=5433
DBNAME=yugabyte
USER=yugabyte
PASSWORD=yugabyte
OPENAI_MODEL=gpt-4o

# pgvector connection — must be libpq keyword form, not postgresql:// URL.
YUGABYTEDB_URL=host=127.0.0.1 port=5433 dbname=yugabyte user=yugabyte password=yugabyte

# ---------- Test account --------------------------------------------
ACCOUNT_EMAIL=you@yugabyte.com
MEKO_LOGIN_PASSWORD=<cognito-password>

# ---------- OpenAI --------------------------------------------------
OPENAI_API_KEY=sk-...
OPENAI_LLM_MODEL=gpt-4o
OPENAI_EMBEDDING_MODEL=text-embedding-3-small

# ---------- Object storage (MinIO) ----------------------------------
S3_AUTH_MODE=minio
S3_ENDPOINT=http://localhost:19094
S3_ACCESS_KEY=meko
S3_SECRET_KEY=mekosecret
S3_BUCKET=meko-kb-uploads
S3_REGION=us-east-2
```

## 3. api_server

```bash
cd ~/code/meko/api_server
set -a && source ../.env && set +a

go run . -port 25433 \
  -db-host "$META_HOST" -db-port "$META_PORT" \
  -db-user-name "$META_USER" -db-password "$META_PW" \
  -db-name "$META_DB" -db-sslmode disable \
  -langfuse-db-host "$LANGFUSE_DB_HOST" -langfuse-db-port "$LANGFUSE_DB_PORT" \
  -langfuse-db-name "$LANGFUSE_DB_NAME" -langfuse-db-user "$LANGFUSE_DB_USER" \
  -langfuse-db-password "$LANGFUSE_DB_PASSWORD" \
  -langfuse-host "$LANGFUSE_HOST" -langfuse-admin-api-key "$LANGFUSE_ADMIN_API_KEY" \
  -cognito-region "$COGNITO_REGION" -cognito-pool-id "$COGNITO_POOL_ID" \
  -cognito-client-id "$COGNITO_CLIENT_ID" -cognito-client-secret "$COGNITO_CLIENT_SECRET" \
  -cognito-domain "$COGNITO_DOMAIN" \
  -openai-api-key "$OPENAI_API_KEY" -openai-llm-model "$OPENAI_LLM_MODEL" \
  -openai-embedding-model "$OPENAI_EMBEDDING_MODEL" \
  -s3-auth-mode "$S3_AUTH_MODE" -s3-access-key "$S3_ACCESS_KEY" \
  -s3-secret-key "$S3_SECRET_KEY" -s3-bucket "$S3_BUCKET" -s3-region "$S3_REGION"
```

Healthy:

```
s3 service initialized mode=minio endpoint=http://localhost:19094 bucket=meko-kb-uploads region=us-east-2
running migrations ...
api server starting port=25433
```

## 4. rag_agent

```bash
cd ~/code/yugabyte-db/python/ai/rag_agent
set -a && source ~/code/meko/.env && set +a

export YUGABYTEDB_CONNECTION_STRING="postgresql://${META_USER}:${META_PW}@${META_HOST}:${META_PORT}/${META_DB}"
export AWS_ACCESS_KEY_ID="${S3_ACCESS_KEY}"
export AWS_SECRET_ACCESS_KEY="${S3_SECRET_KEY}"
export AWS_REGION="${S3_REGION}"
export AWS_S3_BUCKET_NAME="${S3_BUCKET}"

export AWS_ENDPOINT_URL_S3="${S3_ENDPOINT}"      # see "Why both overrides" below
export AWS_S3_ADDRESSING_STYLE=path
unset AWS_PROFILE AWS_SESSION_TOKEN

./venv/bin/python start_rag_agent.py
```

Healthy:

```
botocore.credentials - INFO - Found credentials in environment variables.
Registered processor for task type: CREATE_SOURCE
RAG Preprocessor is running. Press Ctrl+C to stop.
```

### Why both overrides

By default boto3 (and aws-sdk-go) build S3 request URLs like:

```
https://<bucket>.s3.<region>.amazonaws.com/<key>
```

Two implicit assumptions there:

1. **Host = AWS.** The hostname is hard-wired to `amazonaws.com`,
   selected from the configured region.
2. **Virtual-hosted addressing.** The bucket name is glued in as a
   DNS subdomain. AWS owns `*.s3.<region>.amazonaws.com`, so it
   resolves and TLS works.

MinIO breaks both:

1. It runs on **one fixed hostname** (`localhost:19094` in our case) —
   nothing under `amazonaws.com`.
2. It does **not own wildcard DNS / certs**, so it can't accept
   `<bucket>.localhost:19094`. It only understands path-style URLs
   that put the bucket in the path: `http://localhost:19094/<bucket>/<key>`.

So you have to flip both knobs:

| Override | Fixes |
|---|---|
| `AWS_ENDPOINT_URL_S3=http://localhost:19094` | Redirects the request away from `s3.<region>.amazonaws.com` and toward MinIO. |
| `AWS_S3_ADDRESSING_STYLE=path` | Stops the SDK from prepending `<bucket>.` as a subdomain; bucket goes in the path instead. |

Setting only one of them fails: just the endpoint still produces
`http://<bucket>.localhost:19094/...` (DNS miss); just path-style still
points at real AWS with credentials it doesn't recognise
(`InvalidAccessKeyId`).

### `AWS_S3_ADDRESSING_STYLE` values

boto3 / aws-sdk accept three values; controls how the bucket name is
placed in the request URL.

| Value | URL shape | When to use |
|---|---|---|
| `auto` (default) | virtual-hosted when the bucket name is DNS-safe, otherwise falls back to path | Real AWS S3 — let the SDK decide. |
| `virtual` | `https://<bucket>.s3.<region>.amazonaws.com/<key>` | Force virtual-hosted-style. Real AWS only; fails on MinIO. |
| `path` | `https://<host>/<bucket>/<key>` | **Required for MinIO** (and most S3-compatible servers — Ceph, LocalStack, GCS-XML, on-prem object stores) because they serve one fixed hostname. Also a workaround for AWS buckets whose names contain `.` (TLS cert wouldn't match the virtual-host name). |

Real AWS in dev/prod: leave `AWS_S3_ADDRESSING_STYLE` unset (= `auto`).
MinIO / any single-host S3-compatible server: set it to `path`.

## Teardown

```bash
pkill -f start_rag_agent.py
# Ctrl+C api_server
docker compose -f ~/code/meko/meko_ui/docker-compose.minio.yml down    # add -v to wipe data
```
