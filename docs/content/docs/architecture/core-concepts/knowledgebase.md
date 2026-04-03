---
title: Knowledge bases
linkTitle: Knowledge bases
description: RAG pipelines that chunk, embed, and index your documents
weight: 30
---

A **knowledge base** in Meko is a RAG (Retrieval-Augmented Generation) pipeline that ingests your documents, breaks them into chunks, generates vector embeddings, and indexes them for semantic search. Agents can then query the knowledge base to find relevant information from your documents.

## How it works

When you add a knowledge base to a datapack, Meko's `pg_dist_rag` pipeline:

1. **Fetches documents** from the source (S3, local filesystem, or NFS).
2. **Preprocesses** using [unstructured](https://github.com/Unstructured-IO/unstructured) to extract text from PDFs, images, parquet, iceberg, JSON, and more.
3. **Chunks** the text into segments (configurable chunk size).
4. **Embeds** each chunk using the configured embedding model.
5. **Indexes** the embeddings in pgvector for fast similarity search.

All of this happens within your datapack's database --- there's no separate vector database to manage.

## Supported document formats

Meko supports documents in:

- PDF
- Parquet
- Iceberg
- JSON
- Images
- Video

Documents can be loaded from S3, local filesystem, or NFS.

## Add a knowledge base

Using the CLI:

```bash
meko datapack add_knowledge_base --name my-datapack --url s3://bucket/docs/
```

You can configure chunking settings:

```bash
meko datapack add_knowledge_base --name my-datapack \
  --url s3://bucket/docs/ \
  --settings '{"chunk_size": 512}'
```

Using the API:

```text
POST /api/v1/datapacks/{name}/knowledge-bases
```

```json
{
  "url": "s3://bucket/docs/"
}
```

## Query knowledge

Once indexed, agents can query the knowledge base through the MCP server. The MCP tools handle embedding the query, performing similarity search, and returning relevant chunks.

## Manage knowledge bases

| Method | Endpoint / Command | Description |
| :----- | :----------------- | :---------- |
| CLI | `meko datapack add_knowledge_base --name <n> --url <path>` | Add a new knowledge base |
| API | `GET /api/v1/datapacks/{name}/knowledge-bases` | List knowledge bases |
| API | `POST /api/v1/datapacks/{name}/knowledge-bases` | Add a knowledge base |
| API | `DELETE /api/v1/datapacks/{name}/knowledge-bases` | Delete a knowledge base |

## Next steps

- [Work with knowledge bases](../../../guides/working-with-knowledge-bases/) - How to build and query knowledge bases
- [Learn about datapacks](../datapacks/)
