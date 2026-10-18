package logging

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingFileKeepsN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	r, err := OpenRotating(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		fmt.Fprintf(r, "%s\n", strings.Repeat(string(rune('a'+i)), 59)) // 60 bytes: one per file
	}
	r.Close()
	for _, p := range []string{path, path + ".1", path + ".2"} {
		if fi, err := os.Stat(p); err != nil || fi.Size() > 100 {
			t.Errorf("%s: %v %v", p, fi, err)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Errorf(".3 should not exist: %v", err)
	}
	if b, _ := os.ReadFile(path); !strings.HasPrefix(string(b), "jjj") {
		t.Errorf("current file holds %q, want the last write", b)
	}
}

func TestNewWritesJSONAndText(t *testing.T) {
	var file, term bytes.Buffer
	log := New(&file, &term, slog.LevelInfo)
	log.Debug("hidden")
	log.Info("shown", "n", 1)
	if !strings.Contains(file.String(), `"msg":"shown"`) || strings.Contains(file.String(), "hidden") {
		t.Errorf("file: %q", file.String())
	}
	if !strings.Contains(term.String(), "msg=shown") {
		t.Errorf("terminal: %q", term.String())
	}
}

// A rotation that can't rename keeps logging to the current file instead of stopping.
func TestRotatingFileSurvivesFailedRotation(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can rename in a read-only folder")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	r, err := OpenRotating(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	fmt.Fprint(r, "0123456789")
	if err := os.Chmod(dir, 0o500); err != nil { // renames in it now fail
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	fmt.Fprint(r, "more")
	if _, err := fmt.Fprint(r, "still"); err != nil {
		t.Fatalf("write after a failed rotation: %v", err)
	}
	if b, _ := os.ReadFile(path); !strings.HasSuffix(string(b), "still") {
		t.Errorf("file holds %q", b)
	}
}
