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

Currently, you must upload knowledge via the Meko portal:

1. Navigate to **Datapacks** and select your datapack.
1. Click **Upload Knowledge Files** (or **Add to Knowledge Base**).

Currently, Meko accepts PDF, TXT, TEXT, MD, CSV, JSON, XML, HTML, and HTM files up to 10 per batch. The per-file size cap depends on your plan: 5 MB on Free, 50 MB on Standard.

> [!NOTE] Free tier
>
> Free accounts are limited to **100 MB** of total storage across all datapacks for
> knowledge-base files and [artifacts](../working-with-artifacts/) combined, with
> individual files capped at **5 MB**. Standard accounts get **5 GB** of total storage
> and **50 MB** per file.

To upgrade, go to **Settings > Pricing**, choose **AWS Marketplace** under Standard ($25 / month), then **Continue to AWS Marketplace**. After you subscribe, click **Set up your account**, or the plan stays on Free. Stripe is marked coming soon. See [Meko pricing](https://mekodata.ai/pricing/) for plan details.

<!--
| Source | Example URL |
| :----- | :---------- |
| S3 | `s3://bucket/docs/` |
| Local filesystem | `/path/to/docs/` |
| NFS | NFS mount path |

## Supported document formats

PDF, Parquet, Iceberg, JSON, images, and video.
-->

## Query knowledge

Once your documents are indexed, query them through the MCP server from your coding agent:

```text
Search the knowledge base for information about deployment procedures
```

The MCP server embeds your query, performs similarity search against the indexed chunks, and returns the most relevant results.

## Remove a file

Files can be removed by the datapack owner, a maintainer, or the person who uploaded the file. To remove a file from the datapack:

1. In your datapack, under **Knowledge**, click **View all**.
1. On the **Files** tab under **Knowledge Base**, select the file and click **Remove from KB**.
1. Click **Remove** to confirm.

You can't undo a removal. **Remove from KB** is not available until the file finishes indexing.

To have an agent remove the file, use [`knowledgebase_delete_document`](../../reference/mcp-server/#knowledgebase-tools).


<!-- TODO: Add documentation for trigger_knowledge_base_build and check_index_status MCP tools -->
<!-- TODO: Add documentation for add_source_to_index to add new sources to an existing knowledge base -->
<!-- TODO: Add documentation for cron-based knowledge base refresh -->

## Next Steps

- [Learn about knowledge bases](../../architecture/core-concepts/knowledgebase/)
- [Access your database](../database-access/)
