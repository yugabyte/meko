---
title: Explore Knowledge
linkTitle: Explore Knowledge
description: Add memories, query knowledge, and see Meko in action
weight: 1020
---

## Demo 2: Build a Knowledge Base

Add documents to your datapack and query them using RAG.

### Upload Knowledge

Upload documents to your datapack's knowledge base:

```bash
meko datapack add_knowledge_base --name my-datapack --url s3://bucket/docs/
```

<!-- TODO: Add details on portal-based document upload once the web portal is available -->

### Query Your Knowledge

Once your documents are indexed, query them through your IDE:

```text
Search the knowledge base for information about <your topic>
```

The MCP server performs semantic search across your indexed documents and returns relevant chunks.

## Observe Your Datapack

Check what's happening inside your datapack:

- **Memories** --- View stored memories and their extracted entities
- **Knowledge bases** --- See indexed document sources and their status
- **Traces** --- Review LLM reasoning traces and execution logs

<!-- TODO: Add LangFuse observability walkthrough once integration details are finalized -->
<!-- TODO: Add screenshots showing costs, calls, and timings -->

## Next

- **[Examples](../../examples/)** --- Explore more real-world use cases
- **[Working with Memory guide](../../guides/working-with-memory/)** --- Deep dive into memory operations
- **[Working with Knowledge Bases guide](../../guides/working-with-knowledge-bases/)** --- Advanced knowledge base management
