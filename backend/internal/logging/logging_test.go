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
