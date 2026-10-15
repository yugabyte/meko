# event-consumers

Consumers of the durable event log, `meko_system.events`. A tool such as `context_search` inserts one event
and returns; a consumer reads the event a few seconds later and does the work that used to run inline in the
request. Each subdirectory is one consumer: its own Go module, `Dockerfile`, image and Deployment, so it
deploys, scales and rolls back on its own.

| Consumer | Applies | Group |
|---|---|---|
| [`datapack-metrics-consumer`](datapack-metrics-consumer/README.md) | `search.completed` to the datapack metrics tables (daily activity, token savings, free-tier audit) | `analytics` |

| Piece | Where |
|---|---|
| The tables (`events`, `event_consumers`, `event_dead_letters`, `event_applied`) | `meko/api_server/internal/db/migrations/system/000029` and `000030` |
| The producer, `emit()` | `src/events/` in `yugabyte/meko-mcp-server` |

The consumers are separate from the MCP server on purpose: its image is about 20 GB (torch, the CUDA wheels,
spaCy, mem0) and a consumer needs none of it.

## How consumers are organised

A consumer is a *consumer group*: one row per shard in `event_consumers`, with its own offsets and leases, so
two consumers read the same log independently. A new one needs a migration that seeds its group (see
`000030_event_consumers_seed`), its own directory here, and its own image and chart value.

The engine (leases, batches, the offset compare-and-swap, dead letters, metrics, health) currently lives inside
`datapack-metrics-consumer/internal`, and the settings (`MEKO_EVENTS_CONSUMER_*`) and metrics
(`meko_events_consumer_*`) are named for the engine, not for one consumer. When a second consumer is added, lift
the engine into a shared package at this level instead of copying it.
