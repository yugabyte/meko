---
title: Database access
linkTitle: Database access
description: Connect directly to your datapack's PostgreSQL database
weight: 4500
toc_hide: true
---

Every datapack includes a dedicated YugabyteDB (YSQL) database that you can access directly using standard PostgreSQL tools.

## Connect

### Via connection string

Each datapack provides a PostgreSQL connection string when created:

```text
postgresql://db_my-datapack:password@host:5433/account_id_my-datapack
```

Use this with any PostgreSQL client: `psql`, pgAdmin, DBeaver, or your application's database driver.

## What you can do

### Read-only queries

Query your datapack's data directly. For example, to list the tables in your database:

```sql
SELECT * FROM information_schema.tables WHERE table_schema = 'public';
```

> [!NOTE]
>
> Conversation history and memory are managed internally by Meko and are not exposed as user-facing tables in this database. Use the [`conversation_*`](../../reference/mcp-server/#conversation-tools) and [`memory_*`](../../reference/mcp-server/#memory-tools) MCP tools, or the Meko portal, to access those.

### Write queries

You can also write data directly to your datapack's database:

```sql
-- Create custom tables for your agent's data
CREATE TABLE project_notes (
  id SERIAL PRIMARY KEY,
  title TEXT NOT NULL,
  content TEXT,
  created_at TIMESTAMP DEFAULT NOW()
);
```

### Schema summary

Query the database schema to understand what tables are available:

```sql
SELECT table_name FROM information_schema.tables
WHERE table_schema = 'public';
```

<!-- TODO: Add documentation for db_* MCP tools (read, write, schema operations) -->
<!-- TODO: Add detailed table schema documentation for each datapack table -->
<!-- TODO: Add examples of combining direct SQL with MCP tool operations -->

## Next steps

- [Learn about OLTP data](../../architecture/core-concepts/oltp-data/)
- [Work with knowledge bases](../working-with-knowledge-bases/)
