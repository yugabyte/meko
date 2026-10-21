package auth

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestClientTokenIsMadeOnceAndKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.token")
	if _, err := ReadClientToken(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing token: %v", err)
	}
	tok, err := EnsureClientToken(path)
	if err != nil || len(tok) != 64 {
		t.Fatalf("token %q, %v", tok, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	again, err := EnsureClientToken(path)
	if err != nil || again != tok {
		t.Fatalf("second ensure %q, %v (want %q)", again, err, tok)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".client.token.*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestClientTokenCreatorsAgree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.token")
	toks := make([]string, 8)
	var wg sync.WaitGroup
	for i := range toks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := EnsureClientToken(path)
			if err != nil {
				t.Error(err)
			}
			toks[i] = tok
		}()
	}
	wg.Wait()
	for _, tok := range toks {
		if tok != toks[0] {
			t.Fatalf("creators disagree: %v", toks)
		}
	}
}

func TestClientTokenRefusesAnUnsafeFile(t *testing.T) {
	dir := t.TempDir()
	tok, _ := NewToken()
	cases := []struct {
		name  string
		setup func(path string) error
		want  string
	}{
		{"readable by others", func(p string) error {
			return os.WriteFile(p, []byte(tok), 0o644) //nolint:gosec // G306: the unsafe file under test
		}, "open to other users"},
		{"writable by its group", func(p string) error {
			if err := os.WriteFile(p, []byte(tok), 0o600); err != nil {
				return err
			}
			return os.Chmod(p, 0o620)
		}, "open to other users"},
		{"not a token", func(p string) error { return os.WriteFile(p, []byte("hello"), 0o600) }, "doesn't hold a token"},
		{"a symlink", func(p string) error {
			target := filepath.Join(dir, "target")
			if err := os.WriteFile(target, []byte(tok), 0o600); err != nil {
				return err
			}
			return os.Symlink(target, p)
		}, "isn't a regular file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-"))
			if err := tc.setup(path); err != nil {
				t.Fatal(err)
			}
			if _, err := EnsureClientToken(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}
