package storagecontroller

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveBinKeepsExplicitPaths(t *testing.T) {
	for _, p := range []string{
		"/usr/local/bin/storage_controller",
		"./storage_controller",
		"bin/storage_controller",
	} {
		if got := resolveBin(p); got != p {
			t.Errorf("resolveBin(%q) = %q, want it unchanged", p, got)
		}
	}
}

func TestResolveBinPrefersSiblingOfExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	dir := filepath.Dir(exe)

	name := "sc_resolve_sentinel"
	sibling := filepath.Join(dir, name)
	if err := os.WriteFile(sibling, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Skipf("cannot write next to test binary: %v", err)
	}
	t.Cleanup(func() { os.Remove(sibling) })

	if got := resolveBin(name); got != sibling {
		t.Errorf("resolveBin(%q) = %q, want the bundled sibling %q", name, got, sibling)
	}
}

func TestResolveBinFallsBackToBareName(t *testing.T) {
	// A bare name with no sibling stays as-is for exec's PATH lookup.
	const name = "definitely_not_a_bundled_binary_xyz"
	if got := resolveBin(name); got != name {
		t.Errorf("resolveBin(%q) = %q, want it unchanged for PATH lookup", name, got)
	}
}
