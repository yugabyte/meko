package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeFS makes every path under one of the prefixes report that filesystem type.
func fakeFS(t *testing.T, types map[string]string) {
	t.Helper()
	old := FSType
	FSType = func(p string) (string, error) {
		for prefix, typ := range types {
			if p == prefix || strings.HasPrefix(p, prefix+"/") {
				return typ, nil
			}
		}
		return "ext4", nil
	}
	t.Cleanup(func() { FSType = old })
}

func TestResolve(t *testing.T) {
	home, xdg, other := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", xdg)
	t.Setenv(HomeEnv, "")

	fakeFS(t, nil)
	if got, err := Resolve(""); err != nil || got != filepath.Join(home, ".meko-local") {
		t.Fatalf("default: got %q, %v", got, err)
	}
	if got, err := Resolve(other); err != nil || got != other {
		t.Fatalf("explicit: got %q, %v", got, err)
	}
	t.Setenv(HomeEnv, other)
	if got, err := Resolve(""); err != nil || got != other {
		t.Fatalf("env: got %q, %v", got, err)
	}

	// A home folder on NFS moves the default to $XDG_STATE_HOME on local disk.
	t.Setenv(HomeEnv, "")
	fakeFS(t, map[string]string{home: "nfs4"})
	if got, err := Resolve(""); err != nil || got != filepath.Join(xdg, "meko-local") {
		t.Fatalf("default on NFS: got %q, %v", got, err)
	}
	// A folder the user named on NFS is refused, with the reason.
	_, err := Resolve(filepath.Join(home, "data"))
	if err == nil || !strings.Contains(err.Error(), "network filesystem (nfs4)") {
		t.Fatalf("explicit on NFS: got %v", err)
	}
}

func TestEnsurePermissions(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "data"))
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{l.Root, l.RunDir, l.LogDir} {
		if fi, err := os.Stat(d); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: mode %v, err %v", d, fi.Mode().Perm(), err)
		}
	}
	if err := WriteFile(l.Pid, []byte("1\n")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(l.Pid); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("pid file: mode %v, err %v", fi.Mode().Perm(), err)
	}
}

func TestEnsurePrivateDirRefusesSymlink(t *testing.T) {
	base := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	err := EnsurePrivateDir(filepath.Join(base, "link"))
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("got %v", err)
	}
}
