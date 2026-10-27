package paths

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
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

// The cases AgentK's mekoHome() runs too (testdata/resolve-cases.json), so both pick the same
// folder. Their absolute paths are put under a temporary root, and the network folders made there,
// because Resolve checks the filesystem of a path's nearest folder that exists.
func TestResolveCases(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "resolve-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []struct {
			Name        string            `json:"name"`
			Env         map[string]string `json:"env"`
			NetworkDirs []string          `json:"networkDirs"`
			Want        string            `json:"want"`
			Error       bool              `json:"error"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(b, &file); err != nil {
		t.Fatal(err)
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range file.Cases {
		t.Run(c.Name, func(t *testing.T) {
			root, home, tmp := t.TempDir(), t.TempDir(), t.TempDir()
			oldTmp := SharedTmp
			SharedTmp = tmp
			t.Cleanup(func() { SharedTmp = oldTmp })
			expand := func(p string) string {
				p = strings.NewReplacer("$HOME", home, "$SHARED_TMP", tmp, "$USER", u.Username).Replace(p)
				if filepath.IsAbs(p) && !strings.HasPrefix(p, home) && !strings.HasPrefix(p, tmp) {
					p = filepath.Join(root, p)
				}
				return p
			}
			t.Setenv("HOME", home)
			for _, k := range []string{HomeEnv, "XDG_STATE_HOME"} {
				t.Setenv(k, "")
				if v, ok := c.Env[k]; ok {
					t.Setenv(k, expand(v))
				}
			}
			network := map[string]string{}
			for _, d := range c.NetworkDirs {
				if err := os.MkdirAll(expand(d), 0o700); err != nil {
					t.Fatal(err)
				}
				network[expand(d)] = "nfs4"
			}
			fakeFS(t, network)
			got, err := Resolve("")
			switch {
			case c.Error && err == nil:
				t.Fatalf("got %q, want an error", got)
			case !c.Error && (err != nil || got != expand(c.Want)):
				t.Fatalf("got %q, %v; want %q", got, err, expand(c.Want))
			}
		})
	}
}

// A folder named with --data-dir wins, and one on NFS is refused with the reason; the
// /var/tmp/<user>/meko fallback is made private.
func TestResolve(t *testing.T) {
	home, other := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv(HomeEnv, "")
	fakeFS(t, nil)
	if got, err := Resolve(other); err != nil || got != other {
		t.Fatalf("explicit: got %q, %v", got, err)
	}
	fakeFS(t, map[string]string{home: "nfs4"})
	oldTmp := SharedTmp
	SharedTmp = t.TempDir()
	t.Cleanup(func() { SharedTmp = oldTmp })
	got, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(got)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("fallback folder %s: %v, %v", got, fi, err)
	}
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

func TestLooksLikeDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	withDB, withLock, empty := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(New(withDB).DB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := New(withLock).Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(New(withLock).Lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(New(home).DB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]bool{
		withDB: true, withLock: false, empty: false, home: false, "/": false,
		filepath.Join(empty, "none"): false,
	} {
		if got := LooksLikeDataDir(dir); got != want {
			t.Errorf("LooksLikeDataDir(%s) = %v, want %v", dir, got, want)
		}
	}
}

// SameDir compares the folders themselves. A test can't make a bind mount, the case that needs it
// (one disk at two paths), so a hard link stands in: two paths to one file that following
// links can't make equal.
func TestSameDir(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}
	file, hard := filepath.Join(a, "f"), filepath.Join(b, "hard")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(file, hard); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{a, a, true}, {a, link, true}, {a + "/.", a, true}, {file, hard, true},
		{a, b, false}, {filepath.Join(a, "none"), filepath.Join(a, "none"), false}, {"", a, false},
	} {
		if got := SameDir(c.a, c.b); got != c.want {
			t.Errorf("SameDir(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
