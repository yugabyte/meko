# Datapack scale test

Measures how Meko's search and metadata queries behave as a deployment grows to
1,000,000 datapacks and beyond, spread over many accounts. It seeds synthetic datapacks straight into the tables that
api_server and mem0 write, then runs the production SQL against them and checks
latency and recall.

Everything runs as Kubernetes Jobs next to the database. `run.sh` ships the Python
files as a ConfigMap and starts the Jobs from your laptop.

## Prerequisites

- A cluster with a YugabyteDB universe built from the Meko image (it carries
  `vector`, `pg_dist_rag` and `mage`), migrated with
  `api_server -migrate-only`. The files in `infra/` set this up on amp-bench:
  `nodegroups.yaml` adds tainted nodes, and `yugabyte-values.yaml` is an RF3
  universe for the `yugabytedb/yugabyte` chart.
- Nodes labelled `workload=meko-onemil-app` for the Jobs. Change `CTX` and `NS`
  in `run.sh` to point elsewhere.

Never point this at a database with real data. The seeder raises
`yb_universes.max_datapacks` and writes a million `meko_system.datapacks` rows.

## Run it

```bash
./run.sh seed 0 6000 16           # datapacks [0, 6000) with 16 workers
./run.sh probe 6000 base6k --list # latency + recall of the production queries
./run.sh variants 6000 v6k        # compare query rewrites on the same datapacks
./run.sh backfill --parallel 12   # copy memories into the 000006 tables
./run.sh compact                  # compact them before measuring (see below)
./run.sh logs <job>
```

Seeding is idempotent per datapack, so a rerun over the same range skips what is
already there. Seed in steps (6k, 100k, 300k, 1M) and measure at each.

Compact after a bulk backfill. A freshly backfilled vector index answered about
15x slower (270 ms against 17 ms p50) until it was compacted.

## Synthetic data

`workload.py` derives everything about datapack `i` from `i` and a fixed seed, so
the probes can recompute a datapack's vectors instead of reading them back. That
keeps the database under test out of its own ground truth.

| Property | Value |
|---|---|
| Accounts | `SCALE_ACCOUNTS` (default 10,000) enterprise users with lognormal sizes: at 1M datapacks a median of 15 datapacks, p99 1,514, largest about 18,000; plus one 50,000-datapack account. One agent (`meko_agent`). `SCALE_ACCOUNTS=1` gives the single-account layout |
| Active datapacks | 20%; the other 80% are idle and hold 0-5 memories and no KB chunks |
| Small active datapacks (99% of active) | lognormal sizes: median 50 memories and 20 KB chunks, capped at 2,000 each |
| Large active datapacks (1% of active) | 5,000 memories and 5,000 KB chunks |
| Vectors | 1536 dimensions, unit length |
| Totals at 1M datapacks | about 45.5M vectors, 280 GB of raw embeddings |

Vectors are clustered, not uniform. There are 2,000 shared topic centroids; each
datapack draws its vectors from 3 of them, with noise sized to put a vector at an
expected cosine of 0.89 from its centroid. Uniform random vectors in 1536 dimensions are
nearly orthogonal, which makes nearest-neighbour search unrealistically hard.
Shared topics also mean other datapacks' vectors sit close to every query, which
is the case a vector index filtered by tenant gets wrong.

Text payloads are placeholders (`synthetic memory 3 of datapack 17`). Nothing here
tests keyword search or embedding quality.

## Queries

Each query is a fresh draw from the probed datapack's own topics, never a stored
vector. Recall@10 compares the database's top 10 with an exact brute-force search
over that datapack's vectors.

`probe.py` runs the production SQL verbatim:

| Query | Source | Per run |
|---|---|---|
| KB search | `meko-mcp-server` `hybrid_search._SEMANTIC_SQL` | 5 queries on each of 90 small and 10 large datapacks |
| Memory search | `meko-mem0` `PGVector.search`, with the filters `memory_search` passes (datapack + user, no agent) | the same 5 x 100 |
| Access check | `meko-mcp-server` `_has_datapack_access` | 50 |
| Datapack list | `api_server` `DatapackRepo.List` (unpaginated) | 1, with `--list` |

`variants.py` runs rewrites of the two searches on 10 small and 5 large datapacks,
5 queries each: with and without the agent filter, exact ranking through a
`MATERIALIZED` CTE, `hnsw.ef_search` of 100, 400 and 1000, and the tables from data
migration 000006.

The same datapacks are probed at every seed size, so results compare across sizes.

One client sends queries one at a time, with no think time between them. The
latencies are single-query service times, not behaviour under concurrent load.

## MCP surface and load

`stack/` stands up what an MCP client needs beside the universe, in the same
namespace: LiteLLM (an OpenAI-compatible proxy backed by Bedrock, reached through
the `litellm` service account's IAM role), Redis, MinIO, Langfuse, the api-server
and meko-mcp-server.

```bash
AWS_PROFILE=... eksctl create iamserviceaccount -f infra/litellm-irsa.yaml --approve
AWS_PROFILE=... eksctl create iamserviceaccount -f infra/builder-irsa.yaml --approve
stack/build-images.sh <tag>     # Kaniko on the cluster; see its header for inputs
stack/deploy.sh released        # or: branch (meko_mem0_memories + search routing)
stack/seed_stack.sh             # a PAT per seeded account, a Langfuse mapping per datapack
./run.sh mcp surface surface-1  # every tool once, with checks
./run.sh mcp load load-a10 --agents 10 --minutes 10
kubectl port-forward svc/meko-api-server 18080:80 &
kubectl get secret mcp-load-pats -o jsonpath='{.data.pats}' | base64 -d > pats
MCP_PATS_FILE=pats python listbench.py          # GET /datapacks for the 50k, largest and median accounts
```

- With Cognito off, the api-server maps every request to its `dev-local` account.
  `deploy.sh branch` passes `--dev-auth-accept-pats`, so a Meko PAT resolves to
  its own account instead; the released api-server has no such flag and serves
  only the `dev-local` account.
- Every datapack maps to one Langfuse project (traces per account). MCP fails
  every tool call for a datapack with no mapping row.
- `mcp_suite.py` gives each call a hard deadline (`--timeout`, default 30s): a
  server can keep a slow call's stream alive long past any httpx timeout.
  Abandoned calls keep running on the server and hold a mem0 connection until
  they finish, so restart meko-mcp-server between runs on the released images.
- `load` runs N agents. Each uses the PAT of the account that owns its datapack, so
  agents on one account share a token. An agent opens a conversation on
  an active datapack (Zipf-weighted: a few are hot), makes 3-8 calls from a fixed
  mix (30% memory_search, 15% memory_add, 12% context_search, 12%
  conversation_add_message, 10% knowledgebase_search, the rest reads and
  track_token_usage), pauses `--think` seconds (0.5) between calls, then moves on.
