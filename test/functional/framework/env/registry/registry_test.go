package registry_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/env/registry"
)

func TestAddGetRoundTrip(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	stored, err := reg.Add(registry.Entry{
		Name: "soak-1", Shape: "docker", Provider: "docker", TTL: time.Hour,
		Data: map[string]string{"host": "1.2.3.4", "containers": "ep,yb"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if stored.ID == "" {
		t.Fatal("Add should generate an id")
	}
	if stored.CreatedAt.IsZero() {
		t.Fatal("Add should stamp CreatedAt")
	}

	got, ok, err := reg.Get(stored.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatalf("Get: entry %s not found", stored.ID)
	}
	if got.Name != "soak-1" || got.Provider != "docker" || got.TTL != time.Hour {
		t.Errorf("round-tripped entry wrong: %+v", got)
	}
	if got.Data["host"] != "1.2.3.4" {
		t.Errorf("data map not preserved: %+v", got.Data)
	}
}

func TestGetMissingReturnsFalseNotError(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	_, ok, err := reg.Get("nope")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for a missing id")
	}
}

func TestListOrderedOldestFirst(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	first, err := reg.Add(registry.Entry{Name: "a", CreatedAt: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	second, err := reg.Add(registry.Entry{Name: "b", CreatedAt: time.Now()})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	entries, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 || entries[0].ID != first.ID || entries[1].ID != second.ID {
		t.Errorf("List order wrong: %+v", entries)
	}
}

func TestRemove(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	e, err := reg.Add(registry.Entry{Name: "gone"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	removed, err := reg.Remove(e.ID)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Fatal("Remove should report true for an existing entry")
	}
	_, ok, err := reg.Get(e.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("entry should be gone after Remove")
	}
	removedAgain, err := reg.Remove(e.ID)
	if err != nil {
		t.Fatalf("second Remove: %v", err)
	}
	if removedAgain {
		t.Error("second Remove of the same id should report false")
	}
}

func TestExpired(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		e    registry.Entry
		want bool
	}{
		{"no ttl never expires", registry.Entry{CreatedAt: now.Add(-24 * time.Hour), TTL: 0}, false},
		{"within ttl", registry.Entry{CreatedAt: now, TTL: time.Hour}, false},
		{"past ttl", registry.Entry{CreatedAt: now.Add(-2 * time.Hour), TTL: time.Hour}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.e.Expired(now); got != c.want {
				t.Errorf("Expired = %v, want %v", got, c.want)
			}
		})
	}
}

func TestFileFormatIsHumanReadableTTL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "envs.json")
	reg := registry.New(path)
	if _, err := reg.Add(registry.Entry{Name: "x", TTL: 90 * time.Minute}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	entries := raw["entries"].([]any)
	ttl := entries[0].(map[string]any)["ttl"].(string)
	if ttl != "1h30m0s" {
		t.Errorf("ttl on disk = %q, want 1h30m0s", ttl)
	}
}

// TestConcurrentAddsBestEffort documents the accepted last-writer-wins
// trade-off: concurrent Add calls each read-modify-write independently, so
// some may be lost, but the file must never end up corrupt/unparsable and at
// least one write must survive.
// TestConcurrentAddsAllSurvive: the file lock serializes writers, so EVERY
// concurrent Add lands — no last-writer-wins entry loss that would orphan real
// infrastructure with no record to reap it by.
func TestConcurrentAddsAllSurvive(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := reg.Add(registry.Entry{Name: "concurrent"}); err != nil {
				t.Errorf("Add %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	entries, err := reg.List()
	if err != nil {
		t.Fatalf("List after concurrent writes: %v (file must stay parsable)", err)
	}
	if len(entries) != n {
		t.Fatalf("surviving entries = %d, want all %d (lock must prevent last-writer-wins loss)", len(entries), n)
	}
}

// TestConcurrentAddRemoveNoLoss: Adds and Removes interleaving under contention
// must leave exactly the entries that were never removed — a Remove's save must
// not clobber a concurrent Add's entry, nor vice versa. Half the writers add a
// keeper; the other half add-then-remove a throwaway.
func TestConcurrentAddRemoveNoLoss(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	const keepers = 12
	var wg sync.WaitGroup
	for i := 0; i < keepers; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := reg.Add(registry.Entry{Name: "keep"}); err != nil {
				t.Errorf("Add keeper: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			e, err := reg.Add(registry.Entry{Name: "throwaway"})
			if err != nil {
				t.Errorf("Add throwaway: %v", err)
				return
			}
			if _, err := reg.Remove(e.ID); err != nil {
				t.Errorf("Remove throwaway: %v", err)
			}
		}()
	}
	wg.Wait()

	entries, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != keepers {
		t.Fatalf("surviving entries = %d, want %d keepers (Add/Remove clobbered each other)", len(entries), keepers)
	}
	for _, e := range entries {
		if e.Name != "keep" {
			t.Errorf("unexpected surviving entry %q, want only keepers", e.Name)
		}
	}
}
