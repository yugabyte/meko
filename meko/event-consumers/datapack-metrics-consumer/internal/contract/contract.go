// Package contract holds what this consumer shares with the producers, restated rather than imported.
//
// The producer library is src/events/ in yugabyte/meko-mcp-server, and the tables are created by
// api_server migrations 000029 and 000030. This module depends on neither, so the two values it needs
// are written down here. Keep them in step with those: the migration seeds one consumer row per shard,
// so a different shard count means a migration, not an edit here.
package contract

// ShardCount is the number of shards; migration 000030 seeds one event_consumers row per shard 0..15.
const ShardCount = 16

// KnownEventTypes bounds the `type` metric label, which would otherwise be whatever a producer wrote.
var KnownEventTypes = []string{"search.completed"}

// IsKnownType reports whether t is one of KnownEventTypes.
func IsKnownType(t string) bool {
	for _, k := range KnownEventTypes {
		if k == t {
			return true
		}
	}
	return false
}
