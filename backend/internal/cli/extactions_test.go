package cli

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/paths"
)

// The Extensions page's Start and Stop run the registered extension's own commands, and a start
// makes the next request ask the extension where it listens at once.
func TestRunExtensionStartsAndStops(t *testing.T) {
	dir, _ := setup(t)
	noRealExtensions(t)
	ext := filepath.Join(shortDir(t), "agentk")
	log := fakeExtension(t, ext)
	reg, _ := extensions.Load(dir)
	reg.Entries["agentk"] = extensions.Entry{Command: ext, InstalledByMeko: true}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	w := newWebListener(paths.New(dir), nil, nil, slog.New(slog.DiscardHandler))
	if got := w.Installed(); len(got) != 1 || got[0] != "agentk" {
		t.Errorf("installed %v", got)
	}
	w.sockets["agentk"], w.asked["agentk"] = "/gone.sock", time.Now()
	var out bytes.Buffer
	if err := w.RunExtension(context.Background(), "agentk", "start", &out); err != nil || out.String() != "ext start\n" {
		t.Fatalf("start: %v %q", err, out.String())
	}
	if w.sockets["agentk"] != "" || !w.asked["agentk"].IsZero() {
		t.Errorf("still remembered: %q %v", w.sockets["agentk"], w.asked["agentk"])
	}
	out.Reset()
	if err := w.RunExtension(context.Background(), "agentk", "stop", &out); err != nil || out.String() != "ext stop\n" {
		t.Fatalf("stop: %v %q", err, out.String())
	}
	if got := readLog(t, log); got != "start\nstop\n" {
		t.Errorf("extension ran %q", got)
	}

	delete(reg.Entries, "agentk")
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := w.RunExtension(context.Background(), "agentk", "start", &out); err == nil ||
		!strings.Contains(err.Error(), "isn't installed") {
		t.Errorf("not installed: %v", err)
	}
}

// The page's Install sets AgentK up as meko install_extension does, never asking questions, and the
// client token it passes never reaches the output.
func TestRunExtensionInstallsWithoutATerminal(t *testing.T) {
	dir, port := setup(t)
	noRealExtensions(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	_, tok, _ := run("token", "--data-dir", dir)
	tok = strings.TrimSpace(tok)
	home, _ := os.UserHomeDir()
	ak := filepath.Join(home, ".local", "bin", "agentk")
	if err := os.MkdirAll(filepath.Dir(ak), 0o700); err != nil {
		t.Fatal(err)
	}
	// It echoes its arguments, the key among them, as an installer might.
	script := "#!/bin/sh\necho \"agentk $*\"\n"
	if err := os.WriteFile(ak, []byte(script), 0o700); err != nil { //nolint:gosec // G306: test script
		t.Fatal(err)
	}
	w := newWebListener(paths.New(dir), nil, nil, slog.New(slog.DiscardHandler))
	var b bytes.Buffer
	if err := w.RunExtension(context.Background(), "agentk", "install", &b); err != nil {
		t.Fatalf("install: %v (%s)", err, b.String())
	}
	got := b.String()
	if tok == "" || strings.Contains(got, tok) || !strings.Contains(got, "--meko-key [hidden] --non-interactive") ||
		!strings.Contains(got, "AgentK uses this Meko") {
		t.Errorf("output %q", got)
	}
	if reg, _ := extensions.Load(dir); reg.Entries["agentk"].Command != ak {
		t.Errorf("registry %+v", reg.Entries)
	}
}

// The redactor blanks a secret split across writes, and holds back only what could start one.
func TestRedactor(t *testing.T) {
	var out bytes.Buffer
	r := &redactor{w: &out, secret: []byte("s3cr3t-token")}
	for _, p := range []string{"a s3c", "r3t-token b\nc s3", "x s3cr3t-tokens3cr3t-token", " end s3cr"} {
		_, _ = r.Write([]byte(p))
	}
	if got := out.String(); got != "a [hidden] b\nc s3x [hidden][hidden] end " {
		t.Errorf("before Flush: %q", got)
	}
	r.Flush()
	if got := out.String(); !strings.HasSuffix(got, " end s3cr") {
		t.Errorf("after Flush: %q", got)
	}
	out.Reset()
	plain := &redactor{w: &out}
	_, _ = plain.Write([]byte("no secret"))
	if out.String() != "no secret" {
		t.Errorf("no secret: %q", out.String())
	}
}
