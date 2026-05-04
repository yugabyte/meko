---
title: Work with knowledge bases
linkTitle: Knowledge bases
description: Build, query, and manage knowledge bases in Meko
weight: 4400
---

Knowledge bases let you load documents into your datapack and query them using semantic search (RAG). Meko handles chunking, embedding, and indexing automatically.

<!--
## Add a knowledge base

### Via CLI

```bash
meko datapack add_knowledge_base --name my-datapack --url s3://bucket/docs/
```

You can configure chunking settings:

```bash
meko datapack add_knowledge_base --name my-datapack \
  --url s3://bucket/docs/ \
  --settings '{"chunk_size": 512}'
```

### Via API

```text
POST /api/v1/datapacks/{name}/knowledge-bases
```

```json
{
  "url": "s3://bucket/docs/"
}
```
-->

## Supported sources

| Source | Example URL |
| :----- | :---------- |
| S3 | `s3://bucket/docs/` |
| Local filesystem | `/path/to/docs/` |
| NFS | NFS mount path |

## Supported document formats

PDF, Parquet, Iceberg, JSON, images, and video.

## Query knowledge

Once your documents are indexed, query them through the MCP server from your coding agent:

```text
Search the knowledge base for information about deployment procedures
```

The MCP server embeds your query, performs similarity search against the indexed chunks, and returns the most relevant results.


<!-- TODO: Add documentation for trigger_knowledge_base_build and check_index_status MCP tools -->
<!-- TODO: Add documentation for add_source_to_index to add new sources to an existing knowledge base -->
<!-- TODO: Add documentation for cron-based knowledge base refresh -->

## Next Steps

- [Learn about knowledge bases](../../architecture/core-concepts/knowledgebase/)
- [Access your database](../database-access/)
