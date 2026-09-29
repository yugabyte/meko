# Meko local deploy

One-command local deployment of the full Meko stack. YugabyteDB runs **natively** via `yugabyted`; every other component runs in Docker, wired together by a single `docker-compose.yml`:

| # | Component | Source | Runs as |
|---|---|---|---|
| 1 | Langfuse (web + worker) | `meko-langfuse/` | containers `meko-langfuse-web`, `meko-langfuse-worker` |
| 2 | mem0 | `meko-mem0/mem0/` | Python **library** inside the MCP server (bind-mounted) |
| 3 | MCP server | `meko-mcp-server/` | container `meko-mcp-server` (`:8000`) |
| 4 | Meko UI | `meko/meko/meko_ui/` | container `meko-ui` behind nginx |
| 5 | Inference gateway | `meko/meko/inference_gateway/` | container `meko-inference-gateway` (`:8088`) |
| 6 | API server | `meko/meko/api_server/` | container `meko-api-server` (`:25433`) + one-shot `meko-api-server-migrate` |
| 7 | RAG agent | `yugabyte-db/python/ai/rag_agent/` | containers `meko-rag-agent` (text) and `meko-rag-agent-pdf` (PDF), one image, work-queue pollers |
| — | Infra | — | `meko-redis`, `meko-minio`, `meko-proxy` (nginx), 2 one-shot Langfuse schema-init jobs |

All databases live on **one** YugabyteDB instance on `:5433` — `yugabyte` DB (meko_system schema, datapacks, mem0 pgvector, pg_dist_rag work queue) and `langfuse` DB (Langfuse metadata + tracing under a `clickhouse` schema). No ClickHouse, no Neo4j.

---

## 1. Prerequisites

| What | Details |
|---|---|
| **Docker** | Docker Desktop (macOS) or Docker Engine with the Compose v2 plugin (Linux); `docker info` must succeed. Podman's `docker` shim is refused: its builds cap open files at 1024, which breaks the UI and Langfuse builds. Docker Desktop → Resources: **≥ 12 GB RAM** and **≥ 120 GB disk** (first build downloads ~20 GB of layers; the Langfuse Next.js build alone peaks at ~8 GB RAM). On Linux, images and build cache (~41 GB after a first build) share the host disk with yugabyted. |
| **Repo layout** | `meko/`, `meko-mem0/`, `meko-mcp-server/`, `meko-langfuse/`, `yugabyte-db/` checked out as **siblings** under one parent directory — the default layout. You don't have to clone the `meko-*` siblings yourself: **`start.sh` auto-clones any missing one** (over HTTPS from `github.com/yugabyte/*`, checking out `patch/date-aware-extraction` for `meko-mem0`; the others track their default branch) — you just need access to the internal repos. They're private, so HTTPS clones need git credentials for github.com (e.g. `gh auth login && gh auth setup-git`), or set `MEKO_GIT_BASE=git@github.com:yugabyte` to clone over SSH. Override the host with `MEKO_GIT_BASE` or opt out with `MEKO_NO_CLONE=1`. Only `yugabyte-db` must be provided manually (next row). To point a component at a non-default checkout, set its `MEKO_*_DIR` var in `.env` (see below). |
| **`yugabyted` with the required extensions** | A YugabyteDB distribution that ships the `mage`, `vector`, and `pg_dist_rag` extensions. Obtain it one of two ways: **(a)** clone the source from GitHub (`git clone https://github.com/yugabyte/yugabyte-db.git`, `master` branch) and build it with `./yb_build.sh`; or **(b)** use a provided YugabyteDB release tarball for your OS and architecture that already includes the extensions. Either way, point `MEKO_YB_HOME` in `.env` at the resulting tree. |
| **Cognito user** (mekodev AWS account) | See §2. No signup flow needed — the user is created directly in Cognito. |
| **OpenAI API key** | For mem0's LLM + embeddings (`LLM_PROVIDER`/`EMBEDDER_PROVIDER` default to `openai`). |
| **AWS Bedrock credentials** | **Required** by default: the inference gateway defaults to `INFERENCE_LLM_PROVIDER=bedrock`, so the playground needs `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and `AWS_REGION` in `.env`, plus `AWS_SESSION_TOKEN` for temporary (`ASIA…`) keys. start.sh fails fast if any are missing. Not needed with `INFERENCE_LLM_PROVIDER=openai`. |
| **Host tools** | `git`, `openssl`, `curl`, `ss` or `lsof`, and on Linux `ip` (all standard on macOS and Linux dev images). The `aws` CLI is only needed for `create-cognito-user.sh` (§2) and isn't installed by default. |

### Component source directory overrides (optional)

Each component's location is overridable via `.env`. Leave unset to use the default
(the standard sibling layout). Relative paths resolve against `meko/scripts/local-deploy/`
(i.e. this directory, relative to the repo root); absolute paths also work.

| Env var | Default | Component |
|---|---|---|
| `MEKO_API_SERVER_DIR` | `../../api_server` | API server (in this repo) |
| `MEKO_INFERENCE_GATEWAY_DIR` | `../../inference_gateway` | Inference gateway (in this repo) |
| `MEKO_UI_DIR` | `../../meko_ui` | Meko UI (in this repo) |
| `MEKO_LANGFUSE_DIR` | `../../../../meko-langfuse` | Langfuse (sibling repo) |
| `MEKO_MCP_SERVER_DIR` | `../../../../meko-mcp-server` | MCP server (sibling repo) |
| `MEKO_MEM0_DIR` | `../../../../meko-mem0` | mem0 (sibling repo) |
| `MEKO_YB_HOME` | — (required) | YugabyteDB distribution: source build of `github.com/yugabyte/yugabyte-db` (`master`) *or* a provided release tarball |

---

## 2. Create your Cognito user (one time)

Auth is backed by the shared dev Cognito User Pool in the **mekodev** AWS account:

- **Region**: `eu-north-1`
- **User Pool ID**: `eu-north-1_8jgrVhzW6`

> **One-time, per Cognito instance.** You only need to do this once for a given Cognito User Pool / App Client. If you later point `.env` at a **different** Cognito instance (new pool, client, or app), the user from the old pool won't exist there — you'll have to rerun the steps below against the new instance.

There is **no self-service signup** — the signin page intentionally hides the signup link. Instead, create your user with the helper script, which wraps the two AWS CLI calls (`admin-create-user` + `admin-set-user-password`).

**First, log in to the mekodata-dev AWS account** in your terminal — e.g. `aws sso login --profile <your-profile>` (then pass `--profile`, or export `AWS_PROFILE`), or export `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` env credentials (plus `AWS_SESSION_TOKEN` for temporary `ASIA…` keys). `aws sso login` needs AWS CLI v2. The script preflights with `aws sts get-caller-identity` and stops early, printing AWS's error, if you're not authenticated.

The app client whose id/secret go in `.env` must have **`ALLOW_USER_PASSWORD_AUTH`** enabled and a client secret; otherwise every login fails with `USER_PASSWORD_AUTH flow not enabled for this client`. Users belong to the pool, so switching app clients doesn't need new users.

```bash
cd meko/meko/scripts/local-deploy

# Pool id + region come from .env (COGNITO_POOL_ID / COGNITO_REGION) by default.
# Password is prompted (hidden) if you omit --password.
./create-cognito-user.sh --email you@example.com

# If you authenticated against a named profile, pass it through:
./create-cognito-user.sh --email you@example.com --profile mekodata-dev
```

Run `./create-cognito-user.sh --help` for all flags (`--password`, `--pool-id`, `--region`, `--profile`).

That's it. On your **first login** (UI or curl), api_server JIT-provisions everything:

1. Creates your row in `meko_system.users`.
2. Creates your default datapack (`meko_default_datapack`).
3. Provisions a Langfuse org + project for the datapack and writes `meko_system.langfuse_project_mapping` (requires `LANGFUSE_ADMIN_API_KEY` in `.env` — set by default).

So a fresh user can go straight from "Cognito user exists" → "chatting in the playground" with zero extra steps.

---

## 3. Bring up the stack

```bash
cd meko/meko/scripts/local-deploy

# Interactive (a real terminal): one command. It creates .env if missing, then walks you
# through EVERY required value (each pre-filled with its default — Enter to keep, or type to
# change; secrets hidden), saves them to .env, and brings the stack up.
./start.sh

# Non-interactive (piped / CI / -y): it can't prompt, so the first run copies
# .env.example → .env and exits; fill it in by hand, then re-run:
#   vi .env      # COGNITO_* (pool eu-north-1_8jgrVhzW6, client id/secret), OPENAI_API_KEY,
#                # MEKO_YB_HOME (your built yugabyte-db tree), AWS_* for Bedrock chat
#                # (or set INFERENCE_LLM_PROVIDER=openai)
#   ./start.sh
```

Run `./start.sh --help` for the full flag/subcommand list.

What `start.sh` does, in order:

1. Validates `.env` (required vars), Docker, and the `yugabyted`/`ysqlsh` binaries under `MEKO_YB_HOME`. On a real terminal it runs an **interactive setup**: on a first run or with `--reconfigure` it prompts for **every** required value, each pre-filled with its current `.env` value (the `.env.example` default on a first run) — Enter keeps it, or type to override; secrets are entered hidden. On a normal run it prompts only for values still blank. Non-interactively (piped, CI, or `-y`) it doesn't prompt — it reports the blanks and stops. Then checks disk space: it stops if the disk holding `YB_BASE_DIR` has less than 5% free plus 3 GB (YugabyteDB rejects writes below 5%), and asks before continuing if an image still has to be built and Docker's disk has less than ~45 GB free (plus that floor when both share a disk). `MEKO_SKIP_DISK_CHECK=1` skips it.
2. Auto-generates `MEKO_MCP_INTERNAL_SECRET` if blank (written back to `.env`).
3. **Verifies every component source dir** (resolved from the `MEKO_*_DIR` vars, defaulting to the sibling layout), prints each one's **git branch**, and classifies `MEKO_YB_HOME` as a *cloned/build repo* (shows its branch) or a *release tarball* (shows version + build number). Then **prompts for confirmation before starting** (the prompt warns about data loss only when `--clean` is set). Pass `-y`/`--yes` (or set `MEKO_ASSUME_YES=1`) to skip the prompt for non-interactive runs.
4. Starts yugabyted with `--master_flags="ysql_yb_enable_mage=true,tablet_replicas_per_gib_limit=0" --tserver_flags="ysql_yb_enable_mage=true"`, plus `--advertise_address=<default-route IP>` on Linux for a new `base_dir` (a resumed one keeps the address stored in its `yugabyted.conf`), then polls `yugabyted status` until `Status: Running` **and** `YSQL Status: Ready`. `tablet_replicas_per_gib_limit=0` lifts yugabyted's per-node tablet cap, which Meko's ~1,650-tablet schema exceeds on hosts under ~19 GB RAM; on hosts under 16 GB it prints a memory warning. **By default existing data is preserved** — a running cluster is reused, otherwise the existing `base_dir` is resumed. Pass **`-c`/`--clean`** (or set `MEKO_CLEAN=1`) to `yugabyted destroy` the prior state first and start fresh.
5. Creates extensions: `vector`, `mage`, `pg_dist_rag`.
6. Builds all component images, then starts the containers (`docker compose build` then `up -d` — shown as two sub-stages) — first run builds all images (~15–20 min; **the Langfuse image alone takes ~10–15 min**, peaking at ~8 GB RAM); later runs hit cache (~30 s). Startup ordering inside compose: redis/minio → Langfuse schema-init jobs → `meko-api-server-migrate` (one-shot `-migrate-only`, creates `meko_system.*`) → api_server → everything else. On a fresh database the one-shot jobs take ~25 min (Langfuse metadata ~7–9 min, migrations ~17 min), during which compose prints only `Waiting`.
7. Waits for api_server health, then repoints the seeded `meko_system.yb_universes` row at the local yugabyted.
8. Prints the URLs.

> ⚠️ `./start.sh` **preserves YugabyteDB data by default** — it resumes the existing cluster, so it's safe for day-to-day restarts. Only `./start.sh --clean` **wipes** the data (step 4) for a fresh stack. For reloading individual services without touching the DB, use the reload commands in §5.

**Changing config later:** `./start.sh --reconfigure` re-asks **every** required value (current value shown as the default — Enter keeps it, or type to change), then deploys. Nothing is wiped; you override only what you want. Needs a terminal; can't be combined with `--reload`/`--update`.

### URLs

| Service | URL | Notes |
|---|---|---|
| Meko UI | <http://localhost:5000/> | Sign in with your Cognito email/password at `/signin` |
| Embedded Langfuse | <http://localhost:5000/observe/> | Also reachable via the UI's Observe tab |
| Meko API server | <http://localhost:25433/api/v1/> | Health: `/api/v1/health`; Swagger: `:25433/swagger/index.html` |
| Inference gateway | <http://localhost:8088/> | `POST /chat`, health `/healthz` |
| MCP server | <http://localhost:8000/mcp> | Streamable-HTTP; Cognito JWTs **and** `mko_tkn_*` PATs accepted |
| MinIO console | <http://localhost:19091/> | Login: `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` from `.env` |

### Smoke test

```bash
curl http://localhost:25433/api/v1/health     # {"data":{"database":"ok","status":"ok"},...}
curl http://localhost:8088/healthz            # {"status":"ok"}
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:5000/   # 200
docker ps --format 'table {{.Names}}\t{{.Status}}'
```

### Teardown

```bash
./stop.sh              # compose down + yugabyted stop; volumes preserved
./stop.sh --volumes    # also wipe MinIO + Redis data
./stop.sh --keep-yb    # leave yugabyted running
```


---

## 5. Making changes: which edit needs which reload

All `docker compose` commands below run from this directory (`meko/meko/scripts/local-deploy`); add `-f docker-compose.yml` if invoking from elsewhere.

#### ⚠️ Linux: export `HOST_GATEWAY_IP` before any by-hand `--force-recreate` or rebuild

Only `start.sh` detects the IP that YSQL bound to and exports `HOST_GATEWAY_IP`, which the compose `extra_hosts` uses to map `host.docker.internal` → the host running yugabyted (`docker-compose.yml:13–17`). A `--force-recreate`/rebuild **recreates** the container, so it re-reads that mapping at creation time — run by hand with `HOST_GATEWAY_IP` unset, the new container falls back to the docker-bridge gateway and a DB-touching service (api-server, mcp-server, rag-agent) can no longer reach yugabyted. On **Linux**, export it first (macOS resolves `host.docker.internal` automatically, so skip this there):

```bash
export HOST_GATEWAY_IP=$(ss -ltnH "sport = :${YB_PORT:-5433}" | awk '{print $4}' | sed -E 's/:[0-9]+$//' | grep -vE '^(127\.|0\.0\.0\.0$|\[?::1?\]?$|\*$)' | head -1)
```

Plain `docker compose restart <svc>` is unaffected — it does not recreate the container, so it keeps the mapping `start.sh` baked in.

### Quick reference

| You changed… | Run | Time |
|---|---|---|
| `meko-mem0/mem0/**.py` | `docker compose restart meko-mcp-server` | ~5 s |
| `meko-mcp-server/src/**.py` | `docker compose restart meko-mcp-server` | ~5 s |
| `meko/meko/inference_gateway/**.py` | `docker compose restart meko-inference-gateway` | ~5 s |
| `yugabyte-db/python/ai/rag_agent/**.py` | `docker compose restart meko-rag-agent meko-rag-agent-pdf` (the source is shared by both workers) | ~5 s |
| `meko/meko/api_server/**.go` (or new migration) | `docker compose build meko-api-server && docker compose up -d --force-recreate meko-api-server-migrate meko-api-server && docker compose restart meko-proxy` | ~30–60 s |
| `meko/meko/meko_ui/**` (TS/React) | `docker compose build meko-ui && docker compose up -d --force-recreate meko-ui && docker compose restart meko-proxy` | ~30–60 s |
| `meko-langfuse/**` (TS/Next.js) | `docker compose build langfuse-web langfuse-worker && docker compose up -d --force-recreate langfuse-web langfuse-worker && docker compose restart meko-proxy` | ~10–15 min, ~8 GB RAM |
| Python deps (`requirements.txt` / `pyproject.toml` / `uv.lock`) | `docker compose build <svc> && docker compose up -d --force-recreate <svc>` | ~1–5 min |
| `.env` value (runtime) | `docker compose up -d --force-recreate <affected services>` (+ `restart meko-proxy` if api-server/ui/langfuse-web were recreated). **`docker restart` will NOT pick up the change** — see below. | ~10 s |
| `.env` value baked into the UI (`VITE_COGNITO_*`, `VITE_OBSERVE_EMBED_PATH`, `VITE_INFERENCE_GATEWAY_URL`) | `docker compose build meko-ui && docker compose up -d --force-recreate meko-ui && docker compose restart meko-proxy` (build-time args, so a plain recreate is not enough). **Exception:** `VITE_RUDDERSTACK_*` and the free-tier/feature flags are *runtime* since #312 (served via `/runtime-config.js`) — a plain `--force-recreate meko-ui` is enough for those. | ~30–60 s |
| `docker-compose.yml` | `docker compose up -d` (Compose recreates only what changed) | ~10 s |
| `nginx/default.conf` | `docker compose restart meko-proxy` | ~2 s |
| `init/*.sql`, yugabyted flags, or "give me a clean slate" | `./start.sh --clean` (wipes YB data; plain `./start.sh` resumes without wiping) | ~25–30 min (fresh-database setup, see §3 step 6) + cached builds |

For the per-component rows above, `./start.sh --reload <target> [target...]` runs the
correct command for you (targets: `mcp`, `gateway`, `rag`, `proxy`, `api`, `ui`,
`langfuse`; pass several at once, e.g. `--reload mcp rag`; `mcp-rebuild` and `rag-rebuild`
rebuild those images when their dependency files or Dockerfile change, which `--update`
picks automatically) and, on Linux, re-detects
`HOST_GATEWAY_IP` first so a `--force-recreate` keeps reaching yugabyted. To reload
**everything** in one pass, use `./start.sh --reload all` (or `--reload-all`): it rebuilds
all images (cache-fast) and force-recreates every container, so it picks up both compiled
and bind-mounted changes — the langfuse rebuild can take ~10-15 min. See `./start.sh --help`.

To pull the **latest code** for a component repo, `./start.sh --update <target> [target...]`
runs `git pull --rebase --autostash` on it — fetching the latest commit and replaying your
local commits and uncommitted edits on top. Targets: `mem0`, `mcp`, `langfuse`, and `rag`
(the `yugabyte-db` tree at `MEKO_YB_HOME`, **only when it's a git checkout** — a release
tarball can't be pulled). `api`/`ui`/`gateway` live in this meko repo and are refused —
`git pull` it yourself. **By default it then reloads whatever changed** — cheap restarts
(mcp/rag) run automatically, and a slow rebuild (langfuse) is confirmed first (`--apply`
skips that confirm, like `-y`; `--no-apply` pulls only and prints the reload command).
`./start.sh --update all` (or `--update-all`) pulls the sibling repos — `mem0`, `mcp`,
`langfuse` — but **not** `yugabyte-db` (usually a tarball, and a heavy repo; run `--update rag`
explicitly for it).

### Why three different reload types?

**Type 1 — restart only (Python services).** The compose file bind-mounts local source over the path Python imports from, e.g. for the MCP server:

```yaml
volumes:
  - ../../../../meko-mem0/mem0:/app/.venv/lib/python3.10/site-packages/mem0:ro
  - ../../../../meko-mcp-server/src:/app/src:ro
```

Your edits are instantly visible inside the container, but Python caches imported modules in memory — so the process must restart to re-import. `docker compose restart <svc>` does exactly that, no image rebuild. **Limitation:** dependency changes don't flow through a bind mount (the venv is baked into the image) — those need a rebuild (Type 2).

> ⚠️ **`restart` does not reload `.env`.** `docker compose restart <svc>` (and `docker restart`) restart the *same* container with the environment it was created with — they never re-read `.env`. To apply a changed `.env` value you must **recreate** the container so Compose injects the new env: `docker compose up -d --force-recreate <svc>` (then `restart meko-proxy` if it sits behind nginx). Values baked in at build time — the UI's `VITE_COGNITO_*` / `VITE_OBSERVE_EMBED_PATH` / `VITE_INFERENCE_GATEWAY_URL` args — need a rebuild on top of that (`docker compose build meko-ui && docker compose up -d --force-recreate meko-ui`). RudderStack + feature flags are runtime (`/runtime-config.js`) and only need the recreate.

**Type 2 — rebuild + recreate (compiled services).** api_server (Go) and meko-ui (Vite/esbuild bundle) compile to a single artifact at image-build time; the source isn't in the running container at all. Langfuse is the same but with a much heavier Next.js build. So: `build` to produce a new image, `up -d --force-recreate` to replace the container. For api_server, recreate **both** `meko-api-server-migrate` and `meko-api-server` (they share the image, and new migrations must run before the server starts).

**Type 3 — full `./start.sh --clean`.** Anything that lives in YugabyteDB bootstrap (extensions, Langfuse schema SQL, the universe seed row) or yugabyted flags. With `--clean`, start.sh destroys and recreates the DB, so all data is lost — that's by design for a reproducible fresh stack. (Plain `./start.sh` resumes the existing DB and re-runs the idempotent bootstrap/migration steps without wiping.)

### ⚠️ The nginx gotcha

nginx resolves backend hostnames (`meko-api-server`, `meko-ui`, `langfuse-web`) to container IPs **once at startup** and caches them forever. Any `--force-recreate` of those services gives the new container a new IP, and nginx keeps proxying to the dead one → **502 Bad Gateway** through `:5000` even though the service is healthy (e.g. `curl :25433/api/v1/health` works fine).

Fix is always the same:

```bash
docker compose restart meko-proxy
```

That's why every Type-2 row in the table above ends with `restart meko-proxy`. (Plain `restart` of a service keeps its IP, so Type-1 reloads don't need it.)

### Debugging helpers

```bash
docker compose logs -f <service>                 # follow logs
docker logs meko-mcp-server 2>&1 | tail -50      # one-shot tail
docker ps --format 'table {{.Names}}\t{{.Status}}'

# Inspect the DB directly. macOS: -h 127.0.0.1. Linux: yugabyted listens only on the
# advertise address, i.e. the IP start.sh printed ("starting yugabyted (… advertise=<ip>)").
$MEKO_YB_HOME/bin/ysqlsh -h <127.0.0.1 or advertise ip> -p ${YB_PORT:-5433} -U yugabyte -d yugabyte
```

---

## 6. Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `502 Bad Gateway` on `:5000` but the backing service is healthy | nginx cached a stale container IP after a force-recreate → `docker compose restart meko-proxy` (see §5). |
| start.sh: `extension mage.control missing` | `MEKO_YB_HOME` points at a YugabyteDB build without the required extensions. Use a source build or a release (matching your OS/arch) that ships `mage`, `vector`, and `pg_dist_rag`. |
| start.sh: yugabyted never reaches `YSQL Status: Ready` | Check `$MEKO_YB_HOME/bin/yugabyted status --base_dir=$YB_BASE_DIR` and logs under `$YB_BASE_DIR/logs/` and `$YB_BASE_DIR/data/yb-data/{master,tserver}/logs/*.FATAL`. |
| api_server logs `relation "meko_system.yb_universes" does not exist` | The migrate init container didn't run/succeed → `docker logs meko-api-server-migrate`. |
| `/chat` → `MCP tool discovery failed … 401` | Expired Cognito JWT (1 h lifetime) — re-login, or use an API key (`mko_tkn_…`, created in the UI under **Settings > API Keys**). |
| mcp-server logs `invalid connection option "load_balance"` | `load_balance` is a YugabyteDB smart-driver flag; stock psycopg rejects it. Remove it from `YUGABYTEDB_URL` (already removed in this compose). |
| `/chat` → `No Langfuse project found for datapack …` | Datapack created via UI/API lacks a Langfuse mapping — provision it as the datapack's owner: `curl -X POST http://localhost:25433/api/v1/datapacks/<datapack_id>/langfuse/project-keys -H "Authorization: Bearer <access token or mko_tkn_ API key>"`. |
| Login fails with Cognito error about password change | User was created without `--permanent` — re-run `admin-set-user-password … --permanent`. |
| First build fails with `No space left on device` | rag-agent's deps pull the torch/CUDA stack (~10 GB unpacked); Langfuse build adds more. Raise Docker Desktop → Resources → disk limit (≥ 120 GB). |
| Langfuse schema-init or a migration fails with `Write to tablet … rejected … insufficient disk space` | YugabyteDB rejects writes when its disk is under 5% free; on Linux, images and build cache share that disk. `docker builder prune` frees the build cache without touching images or volumes. |
| Langfuse build OOM (`exit code 137` / `cannot allocate memory`) | Raise Docker Desktop RAM to ≥ 12 GB for the build; can lower again afterwards. |
| KB uploads fail in rag-agent with S3/DNS errors | MinIO requires path-style S3 URLs — `AWS_S3_ADDRESSING_STYLE=path` must be set on `meko-rag-agent` (already set in this compose). |
| Langfuse schema-init logs "database already exists; skipping" | Expected on every run after the first. |
| Edited `.env` but the service still uses the old value | `docker restart` / `docker compose restart` keep the container's original env. Recreate instead: `docker compose up -d --force-recreate <svc>` (+ `restart meko-proxy` if behind nginx). UI `VITE_COGNITO_*`/`VITE_INFERENCE_GATEWAY_URL` are build-time — rebuild: `docker compose build meko-ui && docker compose up -d --force-recreate meko-ui`. RudderStack/feature flags are runtime — recreate is enough. |
| Observe page is empty (no project/datapack) **or** no default datapack appears after login | The browser cached a Cognito access token from a prior session, so the UI's background `/auth/me` lets api_server JIT-create your account *before* the login flow can provision it. Clear the site's cookies + storage and sign in fresh: Chrome → open `http://localhost:5000` → `F12` → **Application** tab → **Storage** → **Clear site data**, then hard-refresh (`Cmd+Shift+R`) and sign in. Simplest alternative: just use an **Incognito/Private window** (`Cmd+Shift+N`). |

---

## 7. What runs where

```
                            ┌─ nginx (meko-proxy) :5000 ───────────┐
                            │  /             →  meko-ui:5000       │
                            │  /observe/     →  langfuse-web:3000  │
                            │  /api/v1/      →  meko-api-server    │
                            │  /api/public/  →  langfuse-web       │
                            └──────────────────────────────────────┘
                                                │
   ┌────────────────────────────────────────────┼────────────────────────────────────────────┐
   │                                            │                                            │
meko-ui:5000      meko-api-server:25433   inference-gateway:8088   meko-mcp-server:8000   meko-rag-agent
   │                       │                       │                       │              (worker)
   │   /api/v1 proxy →     │                       │  /mcp ──→             │                 │
   │ ←─ api_server ─→      ├── meko_system schema  │  mcp-server (mem0     │ pg_dist_rag ←──┘
   │                       │   pg_dist_rag work_q  │  library, pgvector)   │ work queue
   │                       │   datapacks schema    │                       │
   │                       │                       │                       │
   ├── inference gateway ──┤                       └── Langfuse traces ────┤
   │                       │                                               │
   └────────── Langfuse "observe" embedded ────────────────────────────────┘
                                                                           │
                                          ┌────────────────────────────────┴───┐
                                          │      Redis (Langfuse queue)        │
                                          │      MinIO (langfuse + KB files)   │
                                          │      yugabyted on host:5433        │
                                          │        DB yugabyte   → meko_system │
                                          │                       datapacks    │
                                          │                       mem0         │
                                          │                       (pgvector)   │
                                          │        DB langfuse   → metadata    │
                                          │                       clickhouse   │
                                          │                       (schema)     │
                                          └────────────────────────────────────┘
```
