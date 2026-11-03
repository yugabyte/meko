package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\necho \"ext $1${MEKO_SUMMARY:+ as a row}\"\n", log)
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

// noRealExtensions keeps this machine's own extensions (an installed agentk) out of reach while the
// test runs; t.Setenv restores PATH before setup's cleanup, which is why that one keeps the extensions.
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
	want(t, "install", code, 0, out+errs, "  ✓ Server          running at http://127.0.0.1:"+port)
	want(t, "install's next steps", code, 0, out, "meko extension install agentk")
	if f.spec.DataDir != dir || f.spec.ServiceLog != paths.New(dir).ServiceLog || f.spec.Exe == "" {
		t.Errorf("spec %+v", f.spec)
	}
	code, out, _ = run("status", "--data-dir", dir, "--verbose")
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
	want(t, "stop", code, 0, out, "  ✓ Server          stopped\n")
	if f.calls[len(f.calls)-1] != "stop" {
		t.Errorf("stop didn't go through the service: %v", f.calls)
	}
	code, out, _ = run("start", "--data-dir", dir)
	want(t, "start", code, 0, out, "through fake")
	want(t, "start", code, 0, out, "ext start as a row")
	code, out, _ = run("restart", "--data-dir", dir)
	want(t, "restart", code, 0, out, "Meko restarted")
	code, out, _ = run("stop", "--data-dir", dir, "--keep-extensions")
	want(t, "stop --keep-extensions", code, 0, out, "  ✓ Server          stopped\n")
	// The server asks it where it listens (_handoff) at each start; that's not starting or stopping it.
	if got := strings.ReplaceAll(readLog(t, log), "_handoff --json\n", ""); got != "stop\nstart\n" {
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
	// A service for this data folder goes too, though the purge removed the folder before it's looked at.
	if err := os.WriteFile(f.file, []byte(dir), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ = run("uninstall", "--data-dir", dir, "--purge", "--force", "--with-extensions")
	want(t, "uninstall --purge --force --with-extensions", code, 0, out, "Removed "+dir)
	want(t, "uninstall --purge --force --with-extensions", code, 0, out, "Removed the fake service")
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

// A registered extension whose command is missing (AgentK's home moved into the data folder) is found
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

// An extension that fails gets a ✗ row after its own output, and one that's gone a ✗ row saying so;
// stop goes on to Meko either way.
func TestAFailingExtensionGetsAFailRow(t *testing.T) {
	dir, _ := setup(t)
	noRealExtensions(t)
	ext := filepath.Join(t.TempDir(), "agentk")
	script := []byte("#!/bin/sh\necho 'it broke' >&2\nexit 1\n")
	if err := os.WriteFile(ext, script, 0o700); err != nil { //nolint:gosec // G306: test script
		t.Fatal(err)
	}
	reg, _ := extensions.Load(dir)
	reg.Entries["agentk"] = extensions.Entry{Command: ext, InstalledByMeko: true}
	reg.Entries["presenter"] = extensions.Entry{Command: filepath.Join(t.TempDir(), "gone", "meko_presenter")}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run("stop", "--data-dir", dir)
	want(t, "the failing extension's own output", code, 0, errs, "it broke\n")
	want(t, "the failing extension's row", code, 0, out, "  ✗ AgentK          didn't stop: ")
	want(t, "the damaged extension's row", code, 0, out, "  ✗ Presenter       is damaged (")
	want(t, "Meko's row", code, 0, out, "  ✓ Server          isn't running\n")
}

// install --no-ui writes [web] enabled = false into web.toml beside the access it picks, says so,
// and offers meko ui start; the server then never takes the web port. config.toml's own enabled
// stops it.
func TestInstallWithoutTheWebUI(t *testing.T) {
	dir, port := setup(t)
	noRealExtensions(t)
	configurePort(t, dir, port)
	useFakeService(t)
	code, out, errs := run("install", "--data-dir", dir, "--no-ui", "--access", "local")
	want(t, "install --no-ui", code, 0, out+errs, "  ✓ Web UI          off (meko ui start turns it on), in ")
	want(t, "install --no-ui's next steps", code, 0, out, "meko ui start")
	cfg, err := config.Load(paths.New(dir).Config, func(string) string { return "" }, config.Overrides{})
	if err != nil || cfg.Web.Enabled || cfg.Web.Access != config.AccessLocal {
		t.Fatalf("settings after install --no-ui: %+v %v", cfg.Web, err)
	}
	code, out, _ = run("status", "--data-dir", dir)
	want(t, "status", code, 0, out, "Web UI          off (meko ui start turns it on)")
	//nolint:gosec // G704: the test's own server
	if c, derr := net.DialTimeout("tcp", "127.0.0.1:"+os.Getenv("MEKO_WEB_PORT"), time.Second); derr == nil {
		_ = c.Close()
		t.Error("the web port answers after install --no-ui")
	}

	dir2, _ := setup(t)
	if err = os.WriteFile(paths.New(dir2).Config, []byte("[web]\nenabled = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs = run("install", "--data-dir", dir2, "--no-ui", "--no-service")
	want(t, "install --no-ui with config.toml's enabled", code, exitError, errs,
		"config.toml sets [web] enabled, which --no-ui would change: change it there instead")
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
	code, _, errs := run("extension", "install", "agentk", "--data-dir", dir)
	want(t, "extension install with no server", code, 1, errs, "meko start")
	var out string
	code, out, errs = run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	_, tok, _ := run("token", "--data-dir", dir)
	home, _ := os.UserHomeDir()
	// An install from before the rename to AgentK, found by its old command.
	ak := filepath.Join(home, ".local", "bin", "agent-k-local")
	log := fakeExtension(t, ak)
	code, out, errs = run("extension", "install", "agentk", "--data-dir", dir)
	want(t, "extension install", code, 0, out+errs, "AgentK uses this Meko")
	wantArgs := "install --meko-url http://127.0.0.1:" + port + "/mcp --meko-key " + strings.TrimSpace(tok) +
		" --non-interactive\n"
	if got := readLog(t, log); got != wantArgs {
		t.Errorf("agent-k-local got %q, want %q", got, wantArgs)
	}
	reg, _ := extensions.Load(dir)
	if e := reg.Entries["agentk"]; e.Command != ak || !e.InstalledByMeko {
		t.Errorf("registry %+v", reg.Entries)
	}
	code, out, _ = run("status", "--data-dir", dir, "--verbose")
	want(t, "status", code, 0, out, "Extension:     agentk, running (ext status)")
	// It isn't Meko's release, so delete runs its uninstall and forgets it, and Meko leaves the
	// command (the fake's uninstall leaves it too; AgentK's own removes its ~/.local/bin links).
	code, out, _ = run("extension", "delete", "agentk", "--force", "--data-dir", dir)
	want(t, "extension delete", code, 0, out, "Its command ("+ak+") isn't a release Meko installed, so Meko leaves it")
	want(t, "extension delete", code, 0, out, "Deleted AgentK")
	if got := readLog(t, log); !strings.HasSuffix(got, "uninstall --non-interactive\n") {
		t.Errorf("agent-k-local got %q", got)
	}
	if reg, _ = extensions.Load(dir); len(reg.Entries) != 0 {
		t.Errorf("still registered: %v", reg.Entries)
	}
	if _, err := os.Stat(ak); err != nil {
		t.Errorf("a command Meko didn't install was removed: %v", err)
	}
	code, _, errs = run("extension", "install", "frob", "--data-dir", dir)
	want(t, "an unknown extension", code, 1, errs, "meko knows agentk, presenter")
}

// meko update on a Meko built from source updates no Meko, and goes on to the extensions.
func TestUpdateFromSourceUpdatesTheExtensions(t *testing.T) {
	dir, _ := setup(t)
	code, out, errs := run("update", "--data-dir", dir, "--check")
	want(t, "update", code, 0, out+errs, "doesn't update itself")
	want(t, "update", code, 0, out, "No extensions are installed")
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
	code, _, errs = run("extension", "install", "agentk", "--data-dir", dir)
	want(t, "extension install on an old AgentK", code, 1, errs, "predates --meko-url")
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
	want(t, "install", code, 0, out+errs, "is installed and running")
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

// The service for a data folder is found while the folder is gone (meko destroy leaves the service
// to start Meko on an empty one), and not for another folder.
func TestServiceOfAMissingDataFolder(t *testing.T) {
	f := useFakeService(t)
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.WriteFile(f.file, []byte(gone), 0o600); err != nil {
		t.Fatal(err)
	}
	if serviceOf(paths.New(gone)) == nil {
		t.Error("the service for a missing data folder wasn't found")
	}
	if serviceOf(paths.New(t.TempDir())) != nil {
		t.Error("the service for another data folder was found")
	}
}

// meko uninstall removes a release install's commands, with mekosh (meko-cli's old name), and leaves
// a mekosh that isn't a link into the app folder.
func TestUninstallRemovesTheCommands(t *testing.T) {
	dir, _ := setup(t)
	noRealExtensions(t)
	useFakeService(t)
	app := releaseInstall(t, "0.20261021.0", "0.20261022.0")
	home := os.Getenv("HOME")
	bin := filepath.Join(home, ".local", "bin")
	for _, link := range []string{progMeko, progMekoCLI, progMekosh} {
		if _, err := app.LinkCommandAs(home, link); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errs := run("uninstall", "--data-dir", dir)
	want(t, "uninstall", code, 0, out+errs, "and the meko and meko-cli commands")
	for _, link := range []string{progMeko, progMekoCLI, progMekosh} {
		if _, err := os.Lstat(filepath.Join(bin, link)); !os.IsNotExist(err) { //nolint:gosec // G703: the test's home
			t.Errorf("uninstall left %s: %v", link, err)
		}
	}
	mekosh := filepath.Join(bin, progMekosh)
	if err := os.WriteFile(mekosh, []byte("mine"), 0o600); err != nil { //nolint:gosec // G703: the test's home
		t.Fatal(err)
	}
	code, out, errs = run("uninstall", "--data-dir", dir)
	if code != 0 {
		t.Fatalf("uninstall again: exit %d\n%s%s", code, out, errs)
	}
	if b, _ := os.ReadFile(mekosh); string(b) != "mine" { //nolint:gosec // G304: test file
		t.Errorf("uninstall removed a mekosh that isn't a link into the app folder")
	}
}

// serve under launchd moves a plist that ran app/current/bin/meko onto the launcher, and leaves
// one already on it alone (#430).
func TestServeMovesTheLaunchAgentOntoTheLauncher(t *testing.T) {
	data := t.TempDir()
	app := update.App{Root: filepath.Join(t.TempDir(), "app")}
	oldRunning := runningRelease
	runningRelease = func() (update.App, string, bool) { return app, "0.1.0", true }
	t.Cleanup(func() { runningRelease = oldRunning })
	never := func(name string, args ...string) (string, error) { // never loaded
		if args[0] == "print" {
			return "", fmt.Errorf("not loaded")
		}
		return "", nil
	}
	d := &service.Launchd{Home: t.TempDir(), UID: 501, Run: never}
	old := newManager
	newManager = func() (service.Manager, error) { return d, nil }
	t.Cleanup(func() { newManager = old })
	l := paths.New(data)
	spec := service.Spec{Exe: app.CurrentExe(), DataDir: data, ServiceLog: l.ServiceLog}
	if err := d.Install(spec); err != nil {
		t.Fatal(err)
	}
	launcher := "<string>" + service.Launcher(app.CurrentExe()) + "</string>"
	b, err := os.ReadFile(d.File())
	if err != nil {
		t.Fatal(err)
	}
	oldPlist := strings.Replace(string(b), launcher, "<string>"+app.CurrentExe()+"</string>", 1)
	if err = os.WriteFile(d.File(), []byte(oldPlist), 0o600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	refreshLaunchAgent(l, log) // not run by launchd
	if b, _ = os.ReadFile(d.File()); string(b) != oldPlist {
		t.Fatal("serve outside launchd changed the plist")
	}
	t.Setenv("MEKO_SERVICE", "launchd")
	refreshLaunchAgent(l, log)
	if b, _ = os.ReadFile(d.File()); !strings.Contains(string(b), launcher) {
		t.Fatalf("the plist doesn't run the launcher:\n%s", b)
	}
	before, _ := os.Stat(d.File())
	refreshLaunchAgent(l, log)
	if after, _ := os.Stat(d.File()); !os.SameFile(before, after) {
		t.Error("an up-to-date plist was written again")
	}
	if b, _ = os.ReadFile(service.Launcher(app.CurrentExe())); !strings.Contains(string(b), "exec '"+app.CurrentExe()+"'") {
		t.Errorf("launcher:\n%s", b)
	}
}
