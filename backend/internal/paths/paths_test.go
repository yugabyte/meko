package paths

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	if got, err := Resolve(""); err != nil || got != filepath.Join(home, ".meko") {
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
	if got, err := Resolve(""); err != nil || got != filepath.Join(xdg, "meko") {
		t.Fatalf("default on NFS: got %q, %v", got, err)
	}
	// With no $XDG_STATE_HOME either, it moves to /var/tmp/<user>/meko, made private.
	t.Setenv("XDG_STATE_HOME", "")
	oldTmp := SharedTmp
	SharedTmp = t.TempDir()
	t.Cleanup(func() { SharedTmp = oldTmp })
	oldUser := CurrentUser
	t.Cleanup(func() { CurrentUser = oldUser })
	CurrentUser = func() (*user.User, error) { return &user.User{Username: "mekouser"}, nil }
	want := filepath.Join(SharedTmp, "mekouser", "meko")
	got, err := Resolve("")
	if err != nil || got != want {
		t.Fatalf("default on NFS without XDG: got %q, %v", got, err)
	}
	fi, err := os.Stat(want)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("fallback folder: %v, %v", fi, err)
	}
	// A uid with no account the system can look up (a container's arbitrary uid) uses the number.
	CurrentUser = func() (*user.User, error) { return nil, user.UnknownUserIdError(os.Getuid()) }
	want = filepath.Join(SharedTmp, strconv.Itoa(os.Getuid()), "meko")
	if got, err = Resolve(""); err != nil || got != want {
		t.Fatalf("fallback for an unknown uid: got %q, %v", got, err)
	}

	// A folder the user named on NFS is refused, with the reason.
	_, err = Resolve(filepath.Join(home, "data"))
	if err == nil || !strings.Contains(err.Error(), "network filesystem (nfs4)") {
		t.Fatalf("explicit on NFS: got %v", err)
	}
}

// shortDir is a temp folder with a short path: t.TempDir() on macOS can be too deep for the data
// folder's sockets.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mk-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestEnsurePermissions(t *testing.T) {
	l := New(filepath.Join(shortDir(t), "data"))
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

// A data folder whose sockets' paths would pass the OS's limit is refused before anything is made.
func TestEnsureRefusesADeepFolder(t *testing.T) {
	deep := filepath.Join(shortDir(t), strings.Repeat("d", MaxSocketPath))
	err := New(deep).Ensure()
	if err == nil || !strings.Contains(err.Error(), "too deep") {
		t.Fatalf("a deep data folder: %v", err)
	}
	if _, err := os.Stat(deep); !os.IsNotExist(err) {
		t.Errorf("the refused folder was made: %v", err)
	}
}

func TestEnsurePrivateDir(t *testing.T) {
	gid := os.Getgid()
	cases := []struct {
		name       string
		parentMode os.FileMode
		want       string // "" for success, else a substring of the error
	}{
		{name: "private parent", parentMode: 0o700},
		{name: "world-writable parent", parentMode: 0o777, want: "writable by other users"},
		{name: "group-writable parent", parentMode: 0o770, want: "group"}, // decided below by its gid
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "user")
			dir := filepath.Join(parent, "meko")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(parent, tc.parentMode); err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if want == "group" { // allowed only for the user's own primary group
				want = "writable by other users"
				fi, err := os.Stat(parent)
				if err != nil {
					t.Fatal(err)
				}
				if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Gid) == gid {
					want = ""
				}
			}
			err := EnsurePrivateDir(dir)
			if want != "" {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("got %v, want %q", err, want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
				t.Errorf("mode %v, want 0700", fi.Mode().Perm())
			}
		})
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
