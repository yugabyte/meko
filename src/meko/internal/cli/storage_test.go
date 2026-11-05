package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/memory"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/store"
)

// status's Storage row: none while there's room, a warning from 90% of the limit, failed when full,
// pointing to Meko Cloud and never to raising the limit.
func TestStorageStep(t *testing.T) {
	const limit = 200 << 20
	at := func(used int64) *memory.StorageStatus {
		return &memory.StorageStatus{UsedBytes: used, LimitBytes: limit, ContentUsage: store.ContentUsage{Memories: used}}
	}
	for _, c := range []struct {
		name   string
		s      *memory.StorageStatus
		linked bool
		show   bool
		kind   stepKind
		text   string
	}{
		{name: "an older server says nothing", s: nil},
		{name: "room", s: at(100 << 20)},
		{name: "just under 90%", s: at(limit*9/10 - 1)},
		{name: "90%", s: at(limit * 9 / 10), show: true, kind: stepWarn, text: "180.0 MB of 200 MB used"},
		{name: "full", s: at(limit), show: true, kind: stepFail,
			text: "full, 200.0 MB of 200 MB: delete what you don't need, or run meko cloud_link for larger datasets"},
		{name: "full and linked", s: at(limit + 5<<20), linked: true, show: true, kind: stepFail,
			text: "full, 205.0 MB of 200 MB: delete what you don't need, or keep larger datasets in Meko Cloud"},
	} {
		k, text, show := storageStep(c.s, c.linked)
		if show != c.show || show && (k != c.kind || text != c.text) {
			t.Errorf("%s: %v %v %q", c.name, show, k, text)
		}
	}
	if got := limitSize(1000 << 20); got != "1000 MB" {
		t.Errorf("limitSize %q", got)
	}
}

// [storage] max_content_mb in config.toml is the running server's limit: status says it, and a
// write past it is refused with what to do.
func TestStorageLimitFromConfig(t *testing.T) {
	withMemoryEngine(t)
	dir, port := setup(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.New(dir).Config, []byte("[storage]\nmax_content_mb = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	startReady(t, dir, port)
	code, out, _ := run("status", "--data-dir", dir, "--verbose")
	want(t, "status --verbose", code, 0, out, " of 1 MB used (memories ")
	code, out, _ = run("status", "--data-dir", dir)
	if code != 0 || strings.Contains(out, "Storage") {
		t.Errorf("status with room shows a Storage row: %d\n%s", code, out)
	}

	withStdin(t, strings.Repeat("a long note ", 100_000))
	code, out, errs := runSh("add_memory", "--data-dir", dir, "--datapack", "default")
	want(t, "add_memory past the limit", code, 1, out+errs, "Meko is full: its storage limit is 1 MB")
	if strings.Contains(out+errs, "max_content_mb") {
		t.Errorf("the error suggests raising the limit: %s", out+errs)
	}
}
