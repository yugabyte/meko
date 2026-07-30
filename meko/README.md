<picture>
  <source media="(prefers-color-scheme: dark)" srcset="meko_ui/public/meko-logo.svg">
  <img src="meko_ui/public/Meko_Logo_Blk.svg" alt="meko" width="240">
</picture>

An open-source data platform for AI agents, backed by YugabyteDB. One command gives you PostgreSQL, vectors, graph memory, RAG, and inference — all in one system.

Agent SDKs handle orchestration well, but the data layer underneath — vectors, relational tables, graph memory, caches, RAG pipelines, LLM runtimes — is still a stack of independent systems you have to wire together, operate separately, and scale independently. Knowledge and inference layers end up isolated from each other. Observability, security, and RBAC are afterthoughts bolted on per-component. Data preprocessing is manual. All of that gets in the way fast, especially for private cloud and air-gapped deployments.

Meko replaces that with a single system. `meko start` and you have everything.

---

## What You Get

Each agent gets an isolated **datapack** with:

- **PostgreSQL** (YugabyteDB YSQL) — structured data, conversation history
- **Vector search** (pgvector, hnswlib, scaNN) — embeddings and similarity
- **Graph memory** (mem0) — long-term entity/relationship extraction from conversations
- **RAG pipeline** (pg_dist_rag + unstructured + LangChain) — document chunking, embedding, indexing, with built-in data preprocessing
- **Inference caching** (LMCache / KDN) — reduces redundant LLM calls without a separate cache layer
- **MCP server** — agents discover and query their own datapack
- **Audit trails** — LLM reasoning traces and execution logs
- **Observability, security, RBAC** — spans the full stack, not bolted on per-component

The inference, knowledge, and database layers share state — they operate as one system, not three isolated tiers.

Agents interact via MCP. Developers interact via CLI, API, or direct PostgreSQL connection.

Works with CrewAI, LangChain, LangGraph, and other agent SDKs.

---

## Quick Start

```bash
# Start the platform
meko start

# Create a datapack
meko datapack create --agent_name my-agent \
  --embed_model sentence-transformers/all-MiniLM-L6-v2 \
  --inference_model meta-llama/Llama-3.1-8B-Instruct

# Load documents
meko datapack add_knowledge_base --name my-agent --url s3://bucket/docs/

# Connect to the database
meko datapack connect --name my-agent
```

---

## CLI

```bash
# Platform
meko start                              # Start everything
meko start --host <ip> --inference_node  # With host binding and local inference
meko status                             # Health check
meko connect                            # Database shell
meko upgrade
meko destroy

# Datapacks
meko datapack create --agent_name <n> --embed_model <m> --inference_model <m> --kbase <src>
meko datapack list
meko datapack describe --agent_name <n>
meko datapack status --name <n>
meko datapack connect --name <n>
meko datapack update --name <n>
meko datapack clone --source <n> --target <n>
meko datapack delete --name <n>

# Knowledge
meko datapack add_knowledge_base --name <n> --url <path> --settings '{"chunk_size": 512}'

# Memory
meko datapack search_memories --name <n> --user_prompt "query"
meko datapack clear_memories --name <n>
meko datapack compress_memories --name <n>
```

---

## MCP

Each datapack exposes an MCP server:

```json
{
  "mcpServers": {
    "my-agent": {
      "command": "meko",
      "args": ["mcp", "serve", "--name", "my-agent"]
    }
  }
}
```

---

## Architecture

```
Agent SDK (CrewAI, LangChain, LangGraph, etc.)
  │
  ▼
MEKO
  ├── Inference:  LLMs (Llama, OpenAI, HF) · vLLM* · LMCache/KDN*
  ├── Knowledge:  pg_dist_rag · data lineage · MCP server* · reranking**
  ├── Database:   YugabyteDB YSQL · pgvector · Apache AGE** · BM25** · Valkey**
  └── Observability, audit, RBAC: LangFuse*
  │
  ▼
YugabyteDB (distributed PostgreSQL)

* in progress  ** roadmap
```

Supports documents in PDF, parquet, iceberg, JSON, images, video from S3, local filesystem, or NFS.

---

## Deployment

Runs anywhere — local, cloud, on-prem, air-gapped. Same config everywhere, no architectural rewrites between dev and production. Add YugabyteDB nodes to scale out; remove them without downtime.

---

## Built On

[YugabyteDB](https://www.yugabyte.com/) · [pgvector](https://github.com/pgvector/pgvector) · [mem0](https://github.com/mem0ai/mem0) · pg_dist_rag · [unstructured](https://github.com/Unstructured-IO/unstructured) · [LangChain](https://github.com/langchain-ai/langchain) · [LangFuse](https://github.com/langfuse/langfuse) · [vLLM](https://github.com/vllm-project/vllm) · [LMCache](https://github.com/LMCache/LMCache)

## License

Apache 2.0

## Links

[Slack](https://yugabyte.com/slack) · [Discord](https://discord.gg/yugabyte) · info@yugabyte.com
