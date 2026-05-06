
---
hide:
  - navigation
---

---
title: OLTP data
linkTitle: OLTP data
description: Direct PostgreSQL access via YugabyteDB YSQL for structured data
weight: 50
---

Meko's database layer is powered by [YugabyteDB](https://www.yugabyte.com/) YSQL, a distributed PostgreSQL-compatible database. Every datapack includes a dedicated YSQL database that you can access directly using standard PostgreSQL tools and drivers.

## Direct SQL access

Each datapack provides a PostgreSQL connection string:

```text
postgresql://db_my-datapack:password@host:5433/account_id_my-datapack
```

You can connect with any PostgreSQL client:

```bash
meko datapack connect --name my-datapack
```

Or use the connection string directly with `psql`, pgAdmin, or your application's database driver.

## What's available

Your datapack database includes:

- **Standard PostgreSQL tables**. Create and query any relational schema.
- **pgvector extension**. Vector columns and similarity search operators are pre-enabled.
- **Conversation history**. Raw message logs stored by the MCP server.
- **Memory tables**. The underlying storage for mem0 memory items.
- **Knowledge base indexes**. Vector indexes created by pg_dist_rag.

## YugabyteDB benefits

YugabyteDB provides distributed PostgreSQL with:

- **Horizontal scaling**. Add nodes to increase capacity without downtime.
- **High availability**. Automatic failover with no single point of failure.
- **PostgreSQL compatibility**. Use existing PostgreSQL tools, drivers, and SQL knowledge.

<!-- TODO: Add detailed usage patterns for direct SQL queries against datapack tables -->
<!-- TODO: Document specific table schemas available in each datapack -->
<!-- TODO: Add examples of combining SQL queries with vector search -->
