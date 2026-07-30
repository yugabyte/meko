# Meko Inference Gateway

FastAPI service backing the Meko **Playground** chat. Wires LangChain → the
meko MCP server (memory + conversations on `mcp.mekodata.ai`) → an LLM
(OpenAI or AWS Bedrock).

## What it does

For every `POST /chat`:

1. **Ensure conversation** — on the first turn of a session, call MCP
   `conversation_create`; reuse its UUID on subsequent turns.
2. **`memory_search`** — query MCP with the user's prompt, format results as
   bullets, fold into the system prompt as context.
3. **Load history** — last ≤10 prior `(role, content)` pairs from an
   in-memory session store.
4. **LLM call** — single shot, no streaming. Provider configurable.
5. **`conversation_add_message`** — persist the turn (Langfuse audit trail).
6. **Update session store** — append both user + assistant turns.

Failure modes:

- `conversation_create` failing is fatal (502).
- `memory_search` failing degrades to no memory context (logged, not raised).
- `conversation_add_message` failing logs a warning; the user still gets
  their answer.

## Run

### Install

Requires Python 3.11+.

```bash
cd meko/inference_gateway
python -m venv .venv && source .venv/bin/activate
pip install -e ".[dev]"
```

### Choose a provider

The gateway runs against either **OpenAI** or **AWS Bedrock**. Pick one of
the three knobs below — they all flip the same switch (`cfg.llm_provider`,
which `inference_gateway/llm.py:build_llm()` branches on):

| Knob | When to use |
|---|---|
| `--llm-provider openai\|bedrock` flag | one-off / quick switch |
| `INFERENCE_LLM_PROVIDER=openai\|bedrock` env var | set once per shell or in `.env` |
| edit `main.py` line ~35 — the `default=os.getenv(..., "openai")` string | your team is locked to one provider and never wants to type the flag |

### Configure credentials

Provide via environment. **MCP bearer tokens never go through env** — they
arrive in each `POST /chat` request body and are forwarded as
`Authorization: Bearer <token>` to the MCP server.

| Provider | Required env vars | Notes |
|---|---|---|
| OpenAI | `OPENAI_API_KEY` | one var, that's it |
| Bedrock | `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY` (+ `AWS_SESSION_TOKEN` if STS), `AWS_REGION` | OR set `AWS_PROFILE` if you use named profiles. OR rely on instance role / SSO. Test with `aws sts get-caller-identity`. |

### Start — OpenAI path

```bash
# Pick ONE of the three credential-loading styles below

# Style 1 — inline export (ephemeral, easiest first time)
export OPENAI_API_KEY="sk-..."
python -m inference_gateway --llm-provider openai --llm-model gpt-4o-mini

# Style 2 — one-shot prefix (no shell pollution)
OPENAI_API_KEY="sk-..." python -m inference_gateway --llm-provider openai

# Style 3 — .env file (best for repeated runs)
cat > .env <<'EOF'
OPENAI_API_KEY=sk-...
INFERENCE_LLM_PROVIDER=openai
INFERENCE_LLM_MODEL=gpt-4o-mini
EOF
echo ".env" >> .gitignore
set -a; source .env; set +a
python -m inference_gateway
```

### Start — Bedrock path

```bash
# Style 1 — inline export
export AWS_ACCESS_KEY_ID="AKIA..."
export AWS_SECRET_ACCESS_KEY="..."
export AWS_REGION="us-east-1"
python -m inference_gateway \
  --llm-provider bedrock \
  --llm-model us.amazon.nova-2-lite-v1:0 \
  --mcp-endpoint https://mcp.mekodata.ai/mcp


# Style 2 — named profile
export AWS_PROFILE="meko-dev"
python -m inference_gateway --llm-provider bedrock

# Style 3 — .env file
cat > .env <<'EOF'
AWS_ACCESS_KEY_ID=AKIA...
AWS_SECRET_ACCESS_KEY=...
AWS_REGION=us-east-1
INFERENCE_LLM_PROVIDER=bedrock
INFERENCE_LLM_MODEL=us.amazon.nova-2-lite-v1:0
EOF
echo ".env" >> .gitignore
set -a; source .env; set +a
python -m inference_gateway
```

> **Bedrock pre-flight:** your AWS account needs explicit *model access*
> for the chosen Anthropic model in the chosen region (toggled in the
> Bedrock console). If you see `AccessDeniedException: invocation of model ...`,
> that's the cause.

Health check:

```bash
curl http://localhost:8088/healthz
# → {"status":"ok"}
```

### Try a turn

```bash
curl -X POST http://localhost:8088/chat \
  -H "Content-Type: application/json" \
  -d '{
    "message": "Plan a 1-week European hiking trip under $3k",
    "user_token": "<bearer token issued by Meko auth>",
    "user_id": "<account_id>",
    "session_id": "demo-1",
    "datapack_id": "<optional UUID>"
  }'
```

Response:

```json
{
  "message": "...",
  "session_id": "demo-1",
  "conversation_id": "<MCP conversation UUID>"
}
```

Re-use the same `session_id` on follow-ups to keep history in the sliding
window. `conversation_id` is also returned for client telemetry / Langfuse
deep-links.

## Configuration reference

| Flag | Env var | Default | Notes |
|---|---|---|---|
| `--llm-provider` | `INFERENCE_LLM_PROVIDER` | `openai` | `openai` \| `bedrock` |
| `--llm-model` | `INFERENCE_LLM_MODEL` | `gpt-4o-mini` (openai) / `us.amazon.nova-2-lite-v1:0` (bedrock) | provider-specific model id |
| `--mcp-endpoint` | `INFERENCE_MCP_ENDPOINT` | `https://mcp.mekodata.ai/mcp` | |
| `--aws-region` | `AWS_REGION` | `us-east-1` | bedrock only |
| `--port` | `INFERENCE_PORT` | `8088` | |
| `--cors-origin` (repeatable) | `INFERENCE_CORS_ORIGINS` (comma-sep) | `https://app.mekodev.com`, `https://cloud.mekodata.ai`, `http://localhost:5173` | |
| `--agent-id` | `INFERENCE_AGENT_ID` | `agent` | Hard-coded by the meko MCP convention. |
| `--max-history` | `INFERENCE_MAX_HISTORY` | `10` | per-session sliding window (messages, not turns) |
| `--memory-top-k` | `INFERENCE_MEMORY_TOP_K` | `5` | passed to `memory_search` |
| `--log-level` | `INFERENCE_LOG_LEVEL` | `INFO` | |

## Tests

```bash
pytest
```

Smoke tests cover:

- the full 6-step turn wires `conversation_create` / `memory_search` /
  LLM / `conversation_add_message` with the right args
- a second turn reuses `conversation_id` and includes prior history
- a `memory_search` failure degrades cleanly to no-context (no exception)
- a missing required tool (`conversation_create`) raises a clear error

No network or LLM credentials are needed for `pytest` — all upstream calls
are mocked.

## Layout

```
inference_gateway/
├── pyproject.toml
├── README.md
├── inference_gateway/
│   ├── __init__.py
│   ├── __main__.py        # python -m inference_gateway
│   ├── main.py            # argparse + uvicorn boot
│   ├── app.py             # FastAPI app factory + CORS
│   ├── config.py          # AppConfig dataclass + defaults
│   ├── llm.py             # build_llm(cfg) → BaseChatModel
│   ├── mcp_client.py      # per-request MultiServerMCPClient
│   ├── session_store.py   # in-memory sliding window + conv_id map
│   ├── chain.py           # 6-step run_chat_turn
│   └── routes/
│       └── chat.py        # POST /chat, GET /healthz
└── tests/
    └── test_chain_smoke.py
```

## Constraints / known limits (v1)

- **Single-shot, no streaming.** Returns the full assistant reply in one
  response. Acceptable for Playground; SSE is a follow-up.
- **In-memory session store.** A process restart drops history. A second
  gateway instance behind a load balancer doesn't see the same sessions.
  Move to Redis if/when this hurts.
- **Best-effort persistence.** A failure to call
  `conversation_add_message` does *not* fail the user's request; the
  session still works locally but the right-rail "Recent conversations"
  panel won't show the turn until the next successful write.
- **Trust model.** The gateway treats the caller (the React app) as
  authoritative for `user_token`. Any front-door auth happens upstream.

## Docker build and deployment

```sh
docker build -t 161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/inference-gateway:0.0.1-20260509 .

docker run --rm -p 8088:8088 \
  -e AWS_ACCESS_KEY_ID="<YOUR_AWS_ACCESS_KEY_ID>" \
  -e AWS_SECRET_ACCESS_KEY="<YOUR_AWS_SECRET_ACCESS_KEY>" \
  -e AWS_REGION=us-east-2 \
  161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/inference-gateway:0.0.1-20260509 \
  --llm-provider bedrock \
  --llm-model us.amazon.nova-2-lite-v1:0 \
  --aws-region us-east-1 \
  --mcp-endpoint https://mcp.mekodev.com/mcp

```