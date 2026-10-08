package memtable

import (
	"strings"
	"testing"
)

func TestSet(t *testing.T) {
	t.Cleanup(func() { _ = Set("") }) //nolint:errcheck // "" always succeeds

	if got := Table(); got != "datapacks.meko_mem0_collection" {
		t.Fatalf("default Table() = %q", got)
	}
	if err := Set("meko_mem0_memories"); err != nil {
		t.Fatalf("Set(meko_mem0_memories): %v", err)
	}
	if got := Table(); got != "datapacks.meko_mem0_memories" {
		t.Fatalf("Table() = %q", got)
	}
	if err := Set(""); err != nil || Table() != "datapacks.meko_mem0_collection" {
		t.Fatalf("Set(\"\") = %v, Table() = %q; want the default", err, Table())
	}
	for _, bad := range []string{"x; DROP TABLE y", "Meko", "datapacks.meko", "1abc", `a"b`} {
		if err := Set(bad); err == nil {
			t.Errorf("Set(%q) accepted a name that is not a plain identifier", bad)
		}
		if Table() != "datapacks.meko_mem0_collection" {
			t.Errorf("a rejected Set(%q) changed Table() to %q", bad, Table())
		}
	}
}

func TestEntitiesTableFollowsTheCollection(t *testing.T) {
	t.Cleanup(func() { _ = Set("") }) //nolint:errcheck // "" always succeeds

	if got := EntitiesTable(); got != "datapacks.meko_mem0_collection_entities" {
		t.Fatalf("default EntitiesTable() = %q", got)
	}
	if err := Set("meko_mem0_memories"); err != nil {
		t.Fatalf("Set(meko_mem0_memories): %v", err)
	}
	if got := EntitiesTable(); got != "datapacks.meko_mem0_memories_entities" {
		t.Fatalf("EntitiesTable() = %q", got)
	}
}

// The entity store's name adds "_entities", so a collection name must leave
// room for it under Postgres' 63-byte identifier limit.
func TestSetLeavesRoomForTheEntitiesSuffix(t *testing.T) {
	t.Cleanup(func() { _ = Set("") }) //nolint:errcheck // "" always succeeds

	longest := "a" + strings.Repeat("b", 53)
	if err := Set(longest); err != nil {
		t.Fatalf("Set(%d bytes): %v", len(longest), err)
	}
	if err := Set(longest + "b"); err == nil {
		t.Fatalf("Set(%d bytes) accepted a name whose entity table exceeds 63 bytes", len(longest)+1)
	}
}
