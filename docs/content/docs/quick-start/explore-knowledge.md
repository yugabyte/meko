---
title: Explore Knowledge
linkTitle: Explore Knowledge
description: Add memories, query knowledge, and see Meko in action
weight: 1020
---

Meko allows you add knowledge to your datapack by simply uploading documents. We do this using the [meko CLI](../../reference/cli-overview/).

In the following example, we'll add documents to your datapack and query them using RAG.

## Upload knowledge

Upload documents to your datapack's knowledge base:

```bash
meko datapack add_knowledge_base --name my-datapack --url s3://bucket/docs/
```

<!-- TODO: Add details on portal-based document upload once the web portal is available -->

## Query your knowledge

Once your documents are indexed, query them through your IDE:

```text
Search the knowledge base for information about <your topic>
```

The MCP server performs semantic search across your indexed documents and returns relevant chunks.

## Observe your datapack

Check what's happening inside your datapack:

- **Memories** --- View stored memories and their extracted entities
- **Knowledge bases** --- See indexed document sources and their status
- **Traces** --- Review LLM reasoning traces and execution logs

<!-- TODO: Add LangFuse observability walkthrough once integration details are finalized -->
<!-- TODO: Add screenshots showing costs, calls, and timings -->

## Next

- [Examples](../../examples/). Explore more real-world use cases.
- [Working with Memory](../../guides/working-with-memory/). Deep dive into memory operations.
- [Working with Knowledge Bases](../../guides/working-with-knowledge-bases/). Advanced knowledge base management.
