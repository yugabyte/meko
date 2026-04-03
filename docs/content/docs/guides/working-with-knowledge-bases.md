---
title: Working with knowledge bases
linkTitle: Knowledge bases
description: Build, query, and manage knowledge bases in Meko
weight: 4400
---

Knowledge bases let you load documents into your datapack and query them using semantic search (RAG). Meko handles chunking, embedding, and indexing automatically.

## Adding a Knowledge Base

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

## Supported Sources

| Source | Example URL |
|--------|-----------|
| S3 | `s3://bucket/docs/` |
| Local filesystem | `/path/to/docs/` |
| NFS | NFS mount path |

## Supported Document Formats

PDF, Parquet, Iceberg, JSON, images, and video.

## Querying Knowledge

Once your documents are indexed, query them through the MCP server from your IDE:

```text
Search the knowledge base for information about deployment procedures
```

The MCP server embeds your query, performs similarity search against the indexed chunks, and returns the most relevant results.

## Listing Knowledge Bases

### Via CLI

```bash
meko datapack status --name my-datapack
```

Shows knowledge base count as part of the datapack summary.

### Via API

```text
GET /api/v1/datapacks/{name}/knowledge-bases
```

## Deleting a Knowledge Base

### Via API

```text
DELETE /api/v1/datapacks/{name}/knowledge-bases
```

```json
{
  "url": "s3://bucket/docs/"
}
```

<!-- TODO: Add documentation for trigger_knowledge_base_build and check_index_status MCP tools -->
<!-- TODO: Add documentation for add_source_to_index to add new sources to an existing knowledge base -->
<!-- TODO: Add documentation for cron-based knowledge base refresh -->

## Next Steps

- [Knowledge Base concept](../../about/core-concepts/knowledgebase/) --- How the RAG pipeline works
- [Database Access](../database-access/) --- Query your datapack directly via SQL
