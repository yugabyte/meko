// Package memtable names the table mem0 keeps memories in. meko-mcp-server
// writes it through mem0 and the api-server reads it directly, so both take
// MEM0_COLLECTION_NAME and must be switched together: an api-server still on
// the old table misses every memory written after the switch.
package memtable

import (
	"fmt"
	"regexp"
)

// Default is partitioned on (datapack, user, agent). meko_mem0_memories (data
// migration 000006) is partitioned on the datapack alone.
const Default = "meko_mem0_collection"

// entitiesSuffix names mem0's entity store, {collection}_entities. Collection
// names stop 9 bytes short of Postgres' 63 so the entity table's name fits.
const entitiesSuffix = "_entities"

var (
	valid = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,53}$`)
	table = "datapacks." + Default
)

// Set selects the collection. It is read without locking, so call it once at
// startup, before any request is served. Empty selects Default.
func Set(collection string) error {
	if collection == "" {
		collection = Default
	}
	if !valid.MatchString(collection) {
		return fmt.Errorf("mem0 collection name %q is not a lowercase SQL identifier", collection)
	}
	table = "datapacks." + collection
	return nil
}

// Table is the schema-qualified table name. Set only admits identifiers, so it
// is safe to splice into SQL.
func Table() string { return table }

// EntitiesTable is the schema-qualified name of the collection's entity store.
func EntitiesTable() string { return table + entitiesSuffix }
