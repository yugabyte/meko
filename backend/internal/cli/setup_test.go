package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/service"
	"github.com/yugabyte/meko/backend/internal/update"
)

// fakeService is a service manager that runs serve the way start does, as this test binary.
type fakeService struct {
	file  string // stands for the service file: holds the data folder once installed
	spec  service.Spec
	calls []string
}

func (f *fakeService) Kind() string       { return "fake" }
func (f *fakeService) File() string       { return f.file }
func (f *fakeService) StatusHint() string { return "fake status" }
func (f *fakeService) Preflight() error   { return nil }
func (f *fakeService) Installed() bool {
	_, err := os.Stat(f.file)
	return err == nil
}

func (f *fakeService) DataDir() string {
	b, _ := os.ReadFile(f.file)
	return string(b)
}

func (f *fakeService) Install(s service.Spec) error {
	f.calls = append(f.calls, "install")
	f.spec = s
	if err := os.WriteFile(f.file, []byte(s.DataDir), 0o600); err != nil {
		return err
	}
	return f.Start()
}

func (f *fakeService) Start() error {
	f.calls = append(f.calls, "start")
	if startDetached(paths.New(f.DataDir()), config.Overrides{}, "", io.Discard, io.Discard) != exitOK {
		return fmt.Errorf("serve didn't start")
	}
	return nil
}

func (f *fakeService) Stop() error {
	f.calls = append(f.calls, "stop")
	_, _ = stopServer(paths.New(f.DataDir()))
	return nil
}

func (f *fakeService) Restart() error {
	f.calls = append(f.calls, "restart")
	_, _ = stopServer(paths.New(f.DataDir()))
	return f.Start()
}

func (f *fakeService) Running() bool { up, _ := running(paths.New(f.DataDir()).Lock); return up }
func (f *fakeService) Remove() error {
	f.calls = append(f.calls, "remove")
	return os.Remove(f.file)
}

func useFakeService(t *testing.T) *fakeService {
	t.Helper()
	f := &fakeService{file: filepath.Join(t.TempDir(), "meko.service")}
	old := newManager
	newManager = func() (service.Manager, error) { return f, nil }
	t.Cleanup(func() { newManager = old })
	return f
}

// fakeExtension writes an extension program that appends each action and its arguments to log.
func fakeExtension(t *testing.T, path string) string {
	t.Helper()
	log := path + ".log"
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\necho \"ext $1\"\n", log)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // G306: test script
		t.Fatal(err)
	}
	return log
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path) //nolint:gosec // G304: test file
	return string(b)
}

// configurePort makes the data folder's servers use port: a server the fake service starts gets no
// flags, and the test binary drops MEKO_PORT when it runs as serve.
func configurePort(t *testing.T, dir, port string) {
	t.Helper()
	if err := os.WriteFile(paths.New(dir).Config, []byte("[server]\nport = "+port+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// noRealExtensions keeps this machine's own extensions (an installed agent-k-local) out of reach.
func noRealExtensions(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", "/usr/bin:/bin")
}

func TestInstallRunsTheServiceAndExtensionsFollowMeko(t *testing.T) {
	dir, port := setup(t)
	noRealExtensions(t)
	configurePort(t, dir, port)
	f := useFakeService(t)
	code, out, errs := run("install", "--data-dir", dir)
	want(t, "install", code, 0, out+errs, "Meko is running at http://127.0.0.1:"+port)
	want(t, "install's next steps", code, 0, out, "meko install_extension agentk")
	if f.spec.DataDir != dir || f.spec.ServiceLog != paths.New(dir).ServiceLog || f.spec.Exe == "" {
		t.Errorf("spec %+v", f.spec)
	}
	code, out, _ = run("status", "--data-dir", dir)
	want(t, "status", code, 0, out, "Service:       fake, running")

	// An extension registered with this data folder starts and stops with Meko.
	ext := filepath.Join(t.TempDir(), "ext")
	log := fakeExtension(t, ext)
	reg, _ := extensions.Load(dir)
	reg.Entries["agentk"] = extensions.Entry{Command: ext, InstalledByMeko: true}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	code, out, _ = run("stop", "--data-dir", dir)
	want(t, "stop", code, 0, out, "ext stop")
	want(t, "stop", code, 0, out, "Meko stopped\n")
	if f.calls[len(f.calls)-1] != "stop" {
		t.Errorf("stop didn't go through the service: %v", f.calls)
	}
	code, out, _ = run("start", "--data-dir", dir)
	want(t, "start", code, 0, out, "run by fake")
	want(t, "start", code, 0, out, "ext start")
	code, out, _ = run("restart", "--data-dir", dir)
	want(t, "restart", code, 0, out, "Meko restarted")
	code, out, _ = run("stop", "--data-dir", dir, "--keep-extensions")
	want(t, "stop --keep-extensions", code, 0, out, "Meko stopped\n")
	if got := readLog(t, log); got != "stop\nstart\n" {
		t.Errorf("extension ran %q (restart and --keep-extensions must leave it alone)", got)
	}

	code, out, _ = run("uninstall", "--data-dir", dir)
	want(t, "uninstall", code, 0, out, "Removed the fake service")
	want(t, "uninstall", code, 0, out, "Extension agentk is still installed")
	want(t, "uninstall", code, 0, out, "Your data is still in "+dir+"; meko destroy")
	if _, err := os.Stat(paths.New(dir).DB); err != nil {
		t.Errorf("uninstall deleted the data: %v", err)
	}
	stdin = strings.NewReader("n\n")
	t.Cleanup(func() { stdin = os.Stdin })
	code, _, errs = run("uninstall", "--data-dir", dir, "--purge")
	want(t, "uninstall --purge, declined", code, 1, errs, "Nothing was removed")
	// --force keeps the extensions' data (the registry here) and says the extension is still installed.
	code, out, _ = run("uninstall", "--data-dir", dir, "--purge", "--force")
	want(t, "uninstall --purge --force", code, 0, out, "kept the extensions' data in it")
	want(t, "uninstall --purge --force", code, 0, out, "Extension agentk is still installed")
	if _, err := os.Stat(paths.New(dir).DB); !os.IsNotExist(err) {
		t.Errorf("meko.db is still there: %v", err)
	}
	code, out, _ = run("uninstall", "--data-dir", dir, "--purge", "--force", "--with-extensions")
	want(t, "uninstall --purge --force --with-extensions", code, 0, out, "Removed "+dir)
	if strings.Contains(out, "still installed") {
		t.Errorf("uninstall said a removed extension is still installed:\n%s", out)
	}
	if got := readLog(t, log); !strings.HasSuffix(got, "uninstall --purge --delete-worktrees --non-interactive\n") {
		t.Errorf("extension ran %q", got)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the data folder is still there: %v", err)
	}
}

// A registered extension whose command is gone (AgentK's home moved into the data folder) is found
// again in its release install there, and the registry keeps the new path.
func TestAGoneExtensionIsFoundAgain(t *testing.T) {
	dir, _ := setup(t)
	noRealExtensions(t)
	moved := filepath.Join(extensions.Dir(dir, "agentk"), "app", "current", "bin", "agentk")
	log := fakeExtension(t, moved)
	reg, _ := extensions.Load(dir)
	reg.Entries["agentk"] = extensions.Entry{Command: filepath.Join(t.TempDir(), "gone", "agentk"), InstalledByMeko: true}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	_, out, errs := run("stop", "--data-dir", dir)
	want(t, "stop", 0, 0, out+errs, "ext stop")
	if got := readLog(t, log); got != "stop\n" {
		t.Errorf("extension ran %q", got)
	}
	if reg, _ = extensions.Load(dir); reg.Entries["agentk"].Command != moved || !reg.Entries["agentk"].InstalledByMeko {
		t.Errorf("registry %+v", reg.Entries)
	}
}

func TestInstallWithoutAServiceLeavesStartToTheUser(t *testing.T) {
	dir, _ := setup(t)
	noRealExtensions(t)
	f := useFakeService(t)
	code, out, _ := run("install", "--data-dir", dir, "--no-service")
	want(t, "install --no-service", code, 0, out, "start Meko with meko start")
	if len(f.calls) != 0 {
		t.Errorf("the service was touched: %v", f.calls)
	}
	if _, err := os.Stat(paths.New(dir).ClientToken); err != nil {
		t.Errorf("no client token: %v", err)
	}
}

func TestInstallExtensionSetsUpAnInstalledAgentK(t *testing.T) {
	dir, port := setup(t)
	noRealExtensions(t)
	code, _, errs := run("install_extension", "agentk", "--data-dir", dir)
	want(t, "install_extension with no server", code, 1, errs, "meko start")
	var out string
	code, out, errs = run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	_, tok, _ := run("token", "--data-dir", dir)
	home, _ := os.UserHomeDir()
	// An install from before the rename to AgentK, found by its old command.
	ak := filepath.Join(home, ".local", "bin", "agent-k-local")
	log := fakeExtension(t, ak)
	code, out, errs = run("install_extension", "agentk", "--data-dir", dir)
	want(t, "install_extension", code, 0, out+errs, "AgentK uses this Meko")
	wantArgs := "install --meko-url http://127.0.0.1:" + port + "/mcp --meko-key " + strings.TrimSpace(tok) +
		" --non-interactive\n"
	if got := readLog(t, log); got != wantArgs {
		t.Errorf("agent-k-local got %q, want %q", got, wantArgs)
	}
	reg, _ := extensions.Load(dir)
	if e := reg.Entries["agentk"]; e.Command != ak || !e.InstalledByMeko {
		t.Errorf("registry %+v", reg.Entries)
	}
	code, out, _ = run("status", "--data-dir", dir)
	want(t, "status", code, 0, out, "Extension:     agentk, running (ext status)")
	code, out, _ = run("uninstall_extension", "agentk", "--data-dir", dir)
	want(t, "uninstall_extension", code, 0, out, "still installed")
	if reg, _ = extensions.Load(dir); len(reg.Entries) != 0 {
		t.Errorf("still registered: %v", reg.Entries)
	}
	code, _, errs = run("install_extension", "frob", "--data-dir", dir)
	want(t, "an unknown extension", code, 1, errs, "meko knows agentk")
}

func TestInstallExtensionRunsTheNewestAgentKInstaller(t *testing.T) {
	dir, port := setup(t)
	noRealExtensions(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	home, _ := os.UserHomeDir()
	argsFile := filepath.Join(home, "installer-args")
	// The installer puts agentk in ~/.local/bin, and agent-k-local, its old name, as AgentK's does.
	installer := fmt.Sprintf("#!/bin/sh\n# --meko-url\necho \"$* MEKO_HOME=$MEKO_HOME\" > %q\n"+
		"mkdir -p \"$HOME/.local/bin\"\n"+
		"printf '#!/bin/sh\\n' > \"$HOME/.local/bin/agentk\"\n"+
		"chmod 700 \"$HOME/.local/bin/agentk\"\n"+
		"ln -s agentk \"$HOME/.local/bin/agent-k-local\"\n", argsFile)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			_ = json.NewEncoder(w).Encode([]update.Release{
				{Tag: "meko-v0.20261031.0", Assets: []update.Asset{{Name: "install.sh", BrowserURL: srv.URL + "/wrong"}}},
				{Tag: "v0.20261029.0", Assets: []update.Asset{{Name: "install.sh", BrowserURL: srv.URL + "/wrong"}}},
				{Tag: "agentk-v0.20261030.0", Assets: []update.Asset{{Name: "install.sh", BrowserURL: srv.URL + "/install.sh"}}},
			})
		case "/install.sh":
			_, _ = io.WriteString(w, installer)
		default:
			http.Error(w, "wrong installer", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	old := newReleaseClient
	newReleaseClient = func(string) *update.Client {
		return &update.Client{Repo: "o/r", API: srv.URL, HTTP: srv.Client()}
	}
	t.Cleanup(func() { newReleaseClient = old })
	code, out, errs = run("install_extension", "agentk", "--data-dir", dir)
	want(t, "install_extension", code, 0, out+errs,
		"Installing AgentK 0.20261030.0 (agentk-v0.20261030.0)")
	if got := readLog(t, argsFile); !strings.HasPrefix(got, "--meko-url http://127.0.0.1:"+port+"/mcp --meko-key ") ||
		!strings.HasSuffix(got, " MEKO_HOME="+dir+"\n") {
		t.Errorf("installer got %q", got)
	}
	reg, _ := extensions.Load(dir)
	if reg.Entries["agentk"].Command != filepath.Join(home, ".local", "bin", "agentk") {
		t.Errorf("registry %+v", reg.Entries)
	}
}

func TestUpdateNeedsAReleaseInstall(t *testing.T) {
	dir, _ := setup(t)
	code, _, errs := run("update", "--data-dir", dir, "--check")
	want(t, "update", code, 1, errs, "wasn't installed from a release")
}

func TestVersionJSON(t *testing.T) {
	code, out, _ := run("version", "--json")
	var v struct {
		Version   string `json:"version"`
		SQLiteVec string `json:"sqlite_vec"`
		Schema    int    `json:"schema"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil || v.Version == "" || v.SQLiteVec == "" || v.Schema < 1 {
		t.Fatalf("exit %d, %q", code, out)
	}
}

func TestLogsMergesAndFormats(t *testing.T) {
	dir := shortDir(t)
	l := paths.New(dir)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	write := func(path, text string) {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(l.Log, `{"time":"2026-10-21T10:00:00Z","level":"INFO","msg":"Meko started","port":7778}
{"time":"2026-10-21T10:00:02Z","level":"WARN","msg":"slow","path":"/mcp"}
`)
	write(l.MemoryEngineLog, `{"time":"2026-10-21T10:00:01Z","msg":"engine listening","embedder":{"dims":384}}
a plain line
`)
	code, out, _ := run("logs", "--data-dir", dir, "-n", "3")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 3 || !strings.HasPrefix(lines[0], "memory | ") ||
		!strings.Contains(lines[0], `INFO  engine listening embedder={"dims":384}`) ||
		lines[1] != "memory | a plain line" || !strings.Contains(lines[2], "WARN  slow path=/mcp") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	code, out, _ = run("logs", "--data-dir", dir, "--server", "--json")
	if code != 0 || !strings.HasPrefix(out, `{"time":"2026-10-21T10:00:00Z"`) || strings.Contains(out, "memory |") {
		t.Fatalf("--server --json:\n%s", out)
	}
}

// syncBuffer is a bytes.Buffer safe to read while followLogs writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestLogsFollowsAcrossRotation(t *testing.T) {
	shorten(t, &followPoll, 10*time.Millisecond)
	dir := t.TempDir()
	path := filepath.Join(dir, "meko.log")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out syncBuffer
	f := newFollower([]logSource{{"meko", path}}, false, true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.run(ctx, &out)
	}()
	appendTo := func(text string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(text)
		_ = f.Close()
	}
	waitFor := func(text string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if strings.Contains(out.String(), text) {
				return
			}
		}
		t.Fatalf("never saw %q in %q", text, out.String())
	}
	appendTo("one\ntw")
	waitFor("one\n")
	appendTo("o\n")
	waitFor("two\n")
	if err := os.Rename(path, path+".1"); err != nil { // rotated
		t.Fatal(err)
	}
	appendTo("three\n")
	waitFor("three\n")
	cancel()
	<-done
	if got := out.String(); got != "one\ntwo\nthree\n" {
		t.Errorf("followed %q", got)
	}
}

func TestInstallExtensionRefusesAnAgentKTooOld(t *testing.T) {
	dir, port := setup(t)
	noRealExtensions(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	// An AgentK release from before --meko-url, in its release layout.
	home, _ := os.UserHomeDir()
	ver := filepath.Join(home, ".agent-k-local", "app", "0.20261001.0")
	log := fakeExtension(t, filepath.Join(ver, "bin", "agent-k-local"))
	js := filepath.Join(ver, "lib", "dist", "server", "install.js")
	if err := os.MkdirAll(filepath.Dir(js), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(js, []byte("parseArgs({ options: { 'meko-key': {} } })"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ver, "bin", "agent-k-local"), filepath.Join(bin, "agent-k-local")); err != nil {
		t.Fatal(err)
	}
	code, _, errs = run("install_extension", "agentk", "--data-dir", dir)
	want(t, "install_extension on an old AgentK", code, 1, errs, "predates --meko-url")
	if got := readLog(t, log); got != "" {
		t.Errorf("the old AgentK was run: %q", got)
	}
}

func TestStartRefusesFlagsTheServiceWouldIgnore(t *testing.T) {
	dir, port := setup(t)
	noRealExtensions(t)
	configurePort(t, dir, port)
	useFakeService(t)
	code, out, errs := run("install", "--data-dir", dir)
	want(t, "install", code, 0, out+errs, "Meko is running at")
	code, _, errs = run("restart", "--data-dir", dir, "--log-level", "debug")
	want(t, "restart --log-level", code, 1, errs, "reads config.toml")
	code, _, errs = run("stop", "--data-dir", dir)
	want(t, "stop", code, 0, errs, "")
	code, _, errs = run("start", "--data-dir", dir, "--port", port)
	want(t, "start --port", code, 1, errs, "reads config.toml")
}

// Under launchd, which restarts serve however it exits, a serve that finds the data folder taken
// waits for it instead of exiting 75 every few seconds.
func TestServeUnderLaunchdWaitsForTheFolder(t *testing.T) {
	dir, port := setup(t)
	shorten(t, &lockRetry, 50*time.Millisecond)
	t.Setenv("MEKO_SERVICE", "launchd")
	l := paths.New(dir)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	held, err := acquireLock(l.Lock)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := strconv.Atoi(port)
	cfg := config.Default()
	cfg.Server.Port = p
	cfg.Secrets.Store = "file"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ServeOptions{Layout: l, Config: cfg}) }()
	select {
	case err := <-done:
		t.Fatalf("serve gave up instead of waiting: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	releaseLock(held)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(l.Port); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve didn't start once the folder was free")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
