package cli

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/store"
)

// testListener is a web listener for the data folder at dir, with a store of its own (the
// handoff keeps the socket there), that never listens.
func testListener(t *testing.T, dir string) *webListener {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "meko.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err = st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return newWebListener(paths.New(dir), st, slog.New(slog.DiscardHandler), 0)
}

// webExtension writes a fake extension with a web UI at path: it logs its arguments, echoes
// "ext <action>" (all of them for install, the key among them, as an installer might), and names
// sock in its handoff. It returns the log's path.
func webExtension(t *testing.T, path, sock string) string {
	t.Helper()
	log := path + ".log"
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\n"+
		"[ \"$1\" = _handoff ] && { printf '{\"socket\": \"%s\"}\\n'; exit 0; }\n"+
		"[ \"$1\" = install ] && { echo \"agentk $*\"; exit 0; }\necho \"ext $1\"\n", log, sock)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // G306: test script
		t.Fatal(err)
	}
	return log
}

// listenAfter starts listening on the unix socket at sock after d, as an extension's server does
// a moment after its start returns.
func listenAfter(t *testing.T, sock string, d time.Duration) {
	t.Helper()
	ready := make(chan net.Listener, 1)
	go func() {
		time.Sleep(d)
		ln, err := net.Listen("unix", sock)
		if err != nil {
			ready <- nil
			return
		}
		ready <- ln
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	t.Cleanup(func() {
		if ln := <-ready; ln != nil {
			_ = ln.Close()
		}
	})
}

// The Extensions page's Start and Stop run the registered extension's own commands, and a start
// ends only once the extension answers on its socket.
func TestRunExtensionStartsAndStops(t *testing.T) {
	dir, _ := setup(t)
	noRealExtensions(t)
	short := shortDir(t)
	ext, sock := filepath.Join(short, "agentk"), filepath.Join(short, "web.sock")
	log := webExtension(t, ext, sock)
	reg, _ := extensions.Load(dir)
	reg.Entries["agentk"] = extensions.Entry{Command: ext, InstalledByMeko: true}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	w := testListener(t, dir)
	if got := w.Installed(); len(got) != 1 || got[0] != "agentk" {
		t.Errorf("installed %v", got)
	}
	// A look at the list while it starts forgot the socket and held off asking again.
	w.asked["agentk"] = time.Now()
	listenAfter(t, sock, 1500*time.Millisecond)
	var out bytes.Buffer
	begun := time.Now()
	if err := w.RunExtension(context.Background(), "agentk", "start", &out); err != nil || out.String() != "ext start\n" {
		t.Fatalf("start: %v %q", err, out.String())
	}
	if time.Since(begun) < time.Second || w.sockets["agentk"] != sock {
		t.Errorf("returned after %s, before the socket answered, or forgot it (%q)", time.Since(begun), w.sockets["agentk"])
	}
	out.Reset()
	if err := w.RunExtension(context.Background(), "agentk", "stop", &out); err != nil || out.String() != "ext stop\n" {
		t.Fatalf("stop: %v %q", err, out.String())
	}
	if got := readLog(t, log); !strings.HasPrefix(got, "start\n_handoff --json\n") || !strings.HasSuffix(got, "stop\n") {
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

// A start that never answers on its socket fails after ListenTimeout, saying so.
func TestRunExtensionStartThatNeverAnswers(t *testing.T) {
	dir, _ := setup(t)
	noRealExtensions(t)
	short := shortDir(t)
	ext := filepath.Join(short, "agentk")
	webExtension(t, ext, filepath.Join(short, "web.sock"))
	reg, _ := extensions.Load(dir)
	reg.Entries["agentk"] = extensions.Entry{Command: ext, InstalledByMeko: true}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	old := extensions.ListenTimeout
	extensions.ListenTimeout = 500 * time.Millisecond
	t.Cleanup(func() { extensions.ListenTimeout = old })
	w := testListener(t, dir)
	var out bytes.Buffer
	err := w.RunExtension(context.Background(), "agentk", "start", &out)
	if err == nil || !strings.Contains(err.Error(), "AgentK started, but isn't answering after 500ms") {
		t.Fatalf("start: %v", err)
	}
	if !w.asked["agentk"].IsZero() {
		t.Error("the next request won't ask it again at once")
	}
}

// The page's Install sets AgentK up as meko extension install does, never asking questions, and the
// client token it passes never reaches the output.
func TestRunExtensionInstallsWithoutATerminal(t *testing.T) {
	dir, port := setup(t)
	noRealExtensions(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	_, tok, _ := run("token", "--data-dir", dir)
	tok = strings.TrimSpace(tok)
	home, _ := os.UserHomeDir()
	ak := filepath.Join(home, ".local", "bin", "me_kode")
	sock := filepath.Join(shortDir(t), "web.sock")
	webExtension(t, ak, sock)
	listenAfter(t, sock, 0)
	w := testListener(t, dir)
	var b bytes.Buffer
	if err := w.RunExtension(context.Background(), "agentk", "install", &b); err != nil {
		t.Fatalf("install: %v (%s)", err, b.String())
	}
	got := b.String()
	if tok == "" || strings.Contains(got, tok) || !strings.Contains(got, "--meko-key [hidden] --non-interactive") ||
		!strings.Contains(got, "✓ AgentK is installed\n") || strings.Contains(got, "start and stop it") {
		t.Errorf("output %q", got)
	}
	if reg, _ := extensions.Load(dir); reg.Entries["agentk"].Command != ak {
		t.Errorf("registry %+v", reg.Entries)
	}
}

// Without a terminal, an installer that runs too long, or is stopped with Meko, is ended with what
// it started, and the error says which.
func TestRunAttachedEndsWithItsContext(t *testing.T) {
	cmd := filepath.Join(t.TempDir(), "installer")
	// What it starts holds the output too: ending only the script would leave it running.
	script := "#!/bin/sh\necho begun\nsleep 30 &\nwait\n"
	if err := os.WriteFile(cmd, []byte(script), 0o700); err != nil { //nolint:gosec // G306: test script
		t.Fatal(err)
	}
	cases := []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want string
	}{
		{"timeout", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 300*time.Millisecond)
		}, "it took too long and was stopped"},
		{"Meko stopping", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(300*time.Millisecond, cancel)
			return ctx, cancel
		}, "it was stopped: Meko is stopping"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.ctx()
			defer cancel()
			var out bytes.Buffer
			begun := time.Now()
			err := runAttached(ctx, cmd, nil, nil, nil, &out, &out)
			if err == nil || err.Error() != tc.want || out.String() != "begun\n" {
				t.Fatalf("%v %q", err, out.String())
			}
			if took := time.Since(begun); took > 3*time.Second {
				t.Errorf("took %s: what it started kept running", took)
			}
		})
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
