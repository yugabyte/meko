# Meko at 1M datapacks

**Status:** Draft · **Tickets:** [MEKO-785](https://yugabyte.atlassian.net/browse/MEKO-785) (harness), [MEKO-784](https://yugabyte.atlassian.net/browse/MEKO-784) (search), [MEKO-781](https://yugabyte.atlassian.net/browse/MEKO-781) (listing), [MEKO-787](https://yugabyte.atlassian.net/browse/MEKO-787), [MEKO-788](https://yugabyte.atlassian.net/browse/MEKO-788) · **Idea:** MI-6 · **Last updated:** 2026-09-30

We seeded 1,050,000 datapacks into a Meko stack on a 3-node RF3 universe and
measured every query and MCP tool against them. Four things broke at that
scale, and each now has a fix in review:

| What broke | At 1M, before | After the fix | Doc |
|---|---|---|---|
| `memory_search` read all 128 partitions | over 35 min per search | 2.5 ms exact, 29.5 ms through the index | [memory-and-kb-search.md](memory-and-kb-search.md) |
| `GET /datapacks` built the whole list in memory | 13.3 s, then an api-server OOM kill | 104-194 ms per 100-row page at 50k datapacks | [datapack-listing.md](datapack-listing.md) |
| Each KB upload re-queued every pending document | 657,782 tasks for 5,690 documents | one source queued per upload (not yet measured) | [ingestion.md](ingestion.md) |
| Concurrent creates on one universe | 12 of 50 failed with a false "no capacity" | 0 of 400 failed | [ingestion.md](ingestion.md) |

How the numbers were produced, and what the harness can't tell you, is in
[test-harness.md](test-harness.md). The results so far are in the
[interim results report](https://claude.ai/artifact/4HQzG12VPRxduEdPAztsGd).

## Pull requests

| PR | Change |
|---|---|
| [meko#1592](https://github.com/yugabyte/meko/pull/1592) | The harness in `meko/test/scale/`, plus reading an account whose `cognito_sub` is NULL |
| [meko#1591](https://github.com/yugabyte/meko/pull/1591) | Data migration 000006, `MEM0_COLLECTION_NAME` on the api-server, chart `memory.*` |
| [meko-mem0#25](https://github.com/yugabyte/meko-mem0/pull/25) | Exact search for small datapacks, `hnsw.ef_search`, tables partitioned by datapack (2.0.14.5) |
| [meko-mcp-server#384](https://github.com/yugabyte/meko-mcp-server/pull/384) | Configurable memory collection, the same routing for KB search |
| [meko#1590](https://github.com/yugabyte/meko/pull/1590) | Paged and streamed `GET /datapacks`, `/datapacks/count` (draft) |
| [meko-mcp-server#383](https://github.com/yugabyte/meko-mcp-server/pull/383) | `datapack_search`, a bounded `datapack_list` |
| [meko#1610](https://github.com/yugabyte/meko/pull/1610) | Universe assignment waits for the row lock instead of skipping it |
| [meko#1611](https://github.com/yugabyte/meko/pull/1611) | An upload queues only its own source's documents |

All are open as of the date above.

## Diagrams

The sources live in [`diagrams/`](diagrams/): Archify JSON specs for the
architecture, sequence, lifecycle, workflow and dataflow charts, and a Python
generator for the Excalidraw explainers. [`diagrams/README.md`](diagrams/README.md)
says how to regenerate the images.
