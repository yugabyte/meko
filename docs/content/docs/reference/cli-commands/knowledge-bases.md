---
title: Knowledge Base Commands
linkTitle: Knowledge Bases
description: CLI commands for managing knowledge bases
weight: 20
---

## Add a Knowledge Base

```bash
meko datapack add_knowledge_base --name <datapack-name> --url <source-url>
```

Adds a new knowledge base source to a datapack. Meko will fetch the documents, preprocess them, chunk the text, generate embeddings, and index them for semantic search.

### Options

| Flag | Description |
|------|-------------|
| `--name` | Datapack name (required) |
| `--url` | Source URL --- S3, local path, or NFS (required) |
| `--settings` | JSON string with chunking settings (optional) |

### Examples

```bash
# Add from S3
meko datapack add_knowledge_base --name my-datapack --url s3://bucket/docs/

# With custom chunk size
meko datapack add_knowledge_base --name my-datapack \
  --url s3://bucket/docs/ \
  --settings '{"chunk_size": 512}'
```

<!-- TODO: Add documentation for additional KB CLI commands as they become available -->
<!-- TODO: Document list, status, and delete operations for knowledge bases via CLI -->
