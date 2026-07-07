---
title: Knowledge bases
linkTitle: Knowledge base
description: RAG pipeline that parses, chunks, embeds, and indexes your documents
weight: 30
---

A **knowledge base** in Meko is a RAG (Retrieval-Augmented Generation) pipeline that ingests your documents, breaks them into chunks, generates vector embeddings, and indexes them for semantic search. Agents can then query the knowledge base to find relevant information from your documents.

## How it works

When you add a knowledge base to a datapack using the Meko UI, Meko's `pg_dist_rag` pipeline:

1. **Fetches documents** from the source (S3, local filesystem, a web page, or an NFS mount).
2. **Preprocesses** to extract text from PDFs, HTML, markdown files, text files, images, parquet, iceberg, JSON, and more.
3. **Chunks** the text into segments (configurable chunk size).
4. **Embeds** each chunk using the configured embedding model.
5. **Indexes** the embeddings in pgvector for fast similarity search.

All of this happens within your datapack's database; there's no separate vector database to manage.

## Supported document formats

Currently, Meko accepts PDF, TXT, MD, JSON, and CSV files up to 5MB each, up to 10 per batch.

> [!NOTE] Free tier
>
> Free accounts are limited to **100 MB** of total knowledge-base storage across all datapacks, with individual files capped at **5 MB**. See [Meko pricing](https://mekodata.ai/pricing/) for plan details.

Documents can be uploaded to your datapacks via the Meko portal.

<!--
Meko supports documents in:

- PDF
- Parquet
- Iceberg
- JSON
- Images
- Video

Documents can be loaded from S3, local filesystem, or an NFS mounted directory using the Meko UI.
-->

## Query knowledge

Once indexed, agents can query the knowledge base through the MCP server. The MCP tool for knowledge search handles embedding the query, performing similarity search, and returning relevant chunks.

## Next steps

- [Work with knowledge bases](../../../guides/working-with-knowledge-bases/) - How to build and query knowledge bases
- [Learn about datapacks](../datapacks/)
