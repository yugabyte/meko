package cli

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/auth"
	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/paths"
)

// install.sh's variables say when the run began, what it upgrades from, and whether it's the first
// install; meko install run by hand has none.
func TestInstallRun(t *testing.T) {
	env := map[string]string{envInstallStarted: "1700000000", envInstallFrom: "1.2.3", envInstallFirst: "1",
		envInstallAsk: "0"}
	run := installRun(func(k string) string { return env[k] })
	if !run.started.Equal(time.Unix(1700000000, 0)) || run.from != "1.2.3" || !run.first || run.ask {
		t.Errorf("installRun = %+v", run)
	}
	if run = installRun(func(string) string { return "" }); !run.started.IsZero() || run.from != "" || run.first ||
		!run.ask {
		t.Errorf("by hand: %+v", run)
	}
	soon := map[string]string{envInstallStarted: "soon"}
	if run = installRun(func(k string) string { return soon[k] }); !run.started.IsZero() {
		t.Errorf("a start that isn't a number: %+v", run)
	}
}

func TestHomeRel(t *testing.T) {
	for in, want := range map[string]string{"/home/alice/.meko": "~/.meko", "/home/alicex/.meko": "/home/alicex/.meko",
		"/srv/meko": "/srv/meko"} {
		if got := homeRel(in, "/home/alice"); got != want {
			t.Errorf("homeRel(%q) = %q, want %q", in, got, want)
		}
	}
}

// The profile is the one $SHELL reads; other shells get none, and are told the line instead.
func TestPathProfile(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	if got := pathProfile("/h"); got != "/h/.zshrc" {
		t.Errorf("zsh: %q", got)
	}
	t.Setenv("SHELL", "/usr/bin/fish")
	if got := pathProfile("/h"); got != "" {
		t.Errorf("fish: %q", got)
	}
	if got := pathLine("/h/.local/bin", "/h"); got != `export PATH="$HOME/.local/bin:$PATH"` {
		t.Errorf("pathLine = %s", got)
	}
}

// meko install adds its line once, after its mark, and meko uninstall takes both out again, leaving
// the profile as it was.
func TestPathLinesComeAndGo(t *testing.T) {
	home := t.TempDir()
	for name, before := range map[string]string{".bashrc": "alias ll='ls -l'\n", ".zshrc": "setopt autocd"} {
		p := filepath.Join(home, name)
		if err := os.WriteFile(p, []byte(before), 0o600); err != nil {
			t.Fatal(err)
		}
		line := pathLine(filepath.Join(home, ".local", "bin"), home)
		for range 2 {
			if err := addPathLine(p, line); err != nil {
				t.Fatal(err)
			}
		}
		got := readFile(t, p)
		if strings.Count(got, pathMark) != 1 || !strings.HasSuffix(got, "\n"+pathMark+"\n"+line+"\n") ||
			!strings.HasPrefix(got, strings.TrimSuffix(before, "\n")+"\n") {
			t.Errorf("%s after adding:\n%s", name, got)
		}
	}
	changed, err := removePathLines(home)
	if err != nil || len(changed) != 2 {
		t.Fatalf("removePathLines = %v, %v", changed, err)
	}
	if got := readFile(t, filepath.Join(home, ".bashrc")); got != "alias ll='ls -l'\n" {
		t.Errorf(".bashrc after removing: %q", got)
	}
	if got := readFile(t, filepath.Join(home, ".zshrc")); got != "setopt autocd\n" {
		t.Errorf(".zshrc after removing: %q", got)
	}
	if changed, err = removePathLines(home); err != nil || len(changed) != 0 {
		t.Errorf("again: %v, %v", changed, err)
	}
}

// Another program on a port the user chose (MEKO_PORT, as setup sets it) fails the install before
// the service is written, saying how to find it or move Meko, and that running the installer
// again is safe.
func TestInstallRefusesAPortInUse(t *testing.T) {
	dir, port := setup(t)
	noRealExtensions(t)
	configurePort(t, dir, port)
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	f := useFakeService(t)
	code, out, errs := run("install", "--data-dir", dir)
	want(t, "install", code, exitError, out, "  ✗ Server          port "+port+
		" is in use by another program (an AgentK from before Meko served it, or another)\n")
	want(t, "install's way to find it", code, exitError, out, "    See what holds it    ")
	// Nothing was written before the check: no rows of the steps before it, no web.toml, no token.
	if strings.Contains(out, "✓ Data") {
		t.Errorf("install wrote the data folder before the port check:\n%s", out)
	}
	if _, err := os.Stat(config.WebFile(paths.New(dir).Config)); !os.IsNotExist(err) {
		t.Errorf("web.toml was written: %v", err)
	}
	want(t, "install's way out", code, exitError, out, "Nothing was left half-done: running the installer again is safe\n")
	if errs != "" || strings.Contains(out, "Report a bug") {
		t.Errorf("a port the user chose: stderr %q, output %q", errs, out)
	}
	if f.spec.DataDir != "" || len(f.calls) != 0 {
		t.Errorf("the service was touched: %+v %v", f.spec, f.calls)
	}
}

// Run by install.sh, a first install's headline says how long it took, and with no terminal to ask
// on it says how to add an extension in place of the picker. Run again as an upgrade, it says so.
func TestInstallFromTheInstaller(t *testing.T) {
	dir, port := setup(t)
	noRealExtensions(t)
	configurePort(t, dir, port)
	useFakeService(t)
	t.Setenv(envInstallStarted, strconv.FormatInt(time.Now().Add(-5*time.Second).Unix(), 10))
	t.Setenv(envInstallFirst, "1")
	code, out, errs := run("install", "--data-dir", dir)
	want(t, "install", code, 0, out+errs, "\n  ● Meko dev is running · installed in ")
	want(t, "install's picker, off a terminal", code, 0, out, "\n  "+addLater+"\n\nNext:\n")
	want(t, "install's data", code, 0, out, " · secrets in")

	// The fake service doesn't restart a running server as a real one's install does: stop it first.
	if code, out, errs = run("stop", "--data-dir", dir); code != 0 {
		t.Fatalf("stop: %d %s%s", code, out, errs)
	}
	t.Setenv(envInstallFirst, "")
	t.Setenv(envInstallFrom, "0.1.0")
	code, out, errs = run("install", "--data-dir", dir)
	want(t, "upgrade", code, 0, out+errs, "  ✓ Server          restarted in ")
	want(t, "upgrade's headline", code, 0, out, " · upgraded in ")
	want(t, "upgrade's data", code, 0, out, " · kept\n")
	if strings.Contains(out, addLater) {
		t.Errorf("an upgrade offers extensions:\n%s", out)
	}
}

// The context engine's row waits while the engine loads: it says how long it took once it's ready,
// and that it's still loading after engineWait.
func TestWaitEngine(t *testing.T) {
	var calls atomic.Int32
	readyAfter := int32(1 << 30)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		state := "loading"
		if calls.Add(1) > atomic.LoadInt32(&readyAfter) {
			state = "ready"
		}
		_, _ = w.Write([]byte(`{"data":{"version":"1.0","port":1,"context_engine":{"state":"` + state + `"}}}`))
	}))
	defer srv.Close()
	c := &client{base: srv.URL, http: srv.Client()}
	old := engineWait
	engineWait = 300 * time.Millisecond
	defer func() { engineWait = old }()

	var b strings.Builder
	if err := waitEngine(c, &b); err != nil {
		t.Fatal(err)
	}
	if want := "  ! Context engine  still loading · meko status says when it's ready\n"; b.String() != want {
		t.Errorf("never ready: %q, want %q", b.String(), want)
	}

	b.Reset()
	calls.Store(0)
	atomic.StoreInt32(&readyAfter, 1)
	engineWait = time.Minute
	if err := waitEngine(c, &b); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.String(), "  ✓ Context engine  ready in 0.") {
		t.Errorf("ready on the second ask: %q", b.String())
	}
}

// A port held by a Meko server install replaces isn't another program's: this data folder's, or
// the one the installed service runs, which installing the service moves to the new folder.
func TestMekoHoldsItsOwnPort(t *testing.T) {
	dirA, port := setup(t)
	noRealExtensions(t)
	configurePort(t, dirA, port)
	f := useFakeService(t)
	if code, out, errs := run("install", "--data-dir", dirA); code != 0 {
		t.Fatalf("install: %d %s%s", code, out, errs)
	}
	p, _ := strconv.Atoi(port)
	dirB, _ := setup(t)
	if !mekoHolds(paths.New(dirB), f, p) {
		t.Error("the installed service's server on the port isn't counted as Meko's")
	}
	if !mekoHolds(paths.New(dirA), f, p) {
		t.Error("this data folder's server on the port isn't counted as Meko's")
	}
	if mekoHolds(paths.New(dirB), f, p+1) {
		t.Error("another port counted as Meko's")
	}
	other := &fakeService{file: filepath.Join(t.TempDir(), "none")}
	if mekoHolds(paths.New(dirB), other, p) {
		t.Error("a port counted as Meko's with no service and no server for this folder")
	}
}

// A Meko from before one port wrote its agents' port (7778) to its port file, while its web UI held
// 7777: installing over it counts 7777 as its own, from its status, and restarts it rather than
// refusing the port.
func TestMekoHoldsItsOldWebPort(t *testing.T) {
	dir, _ := setup(t)
	l := paths.New(dir)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireServeLock(l.Lock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { releaseLock(lock) })
	const webPortNow = 47777
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/status" || r.Header.Get("Authorization") != "Bearer admin-test-token" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":{"version":"old","web":{"url":"http://127.0.0.1:%d","listening":true}}}`, webPortNow)
	}))
	t.Cleanup(old.Close)
	u, _ := url.Parse(old.URL)
	if err = paths.WriteFile(l.Port, []byte(u.Port()+"\n")); err != nil {
		t.Fatal(err)
	}
	if err = auth.WriteToken(l.Token, "admin-test-token"); err != nil {
		t.Fatal(err)
	}
	none := &fakeService{file: filepath.Join(t.TempDir(), "none")}
	if !mekoHolds(l, none, webPortNow) {
		t.Error("the old server's web port isn't counted as Meko's")
	}
	if p, _ := strconv.Atoi(u.Port()); !mekoHolds(l, none, p) {
		t.Error("the old server's port file isn't counted as Meko's")
	}
	if mekoHolds(l, none, webPortNow+1) {
		t.Error("another port counted as Meko's")
	}
}
