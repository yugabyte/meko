package cli

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/callhome"
	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/service"
	"github.com/yugabyte/meko/src/meko/internal/update"
)

// callHomeReports points call home at a test server and returns the failure reports it gets.
func callHomeReports(t *testing.T) func() []callhome.FailureEnvelope {
	t.Helper()
	var mu sync.Mutex
	var got []callhome.FailureEnvelope
	var raw []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e callhome.FailureEnvelope
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &e)
		mu.Lock()
		got = append(got, e)
		raw = append(raw, string(b))
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MEKO_CALL_HOME_URL", srv.URL)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, body := range raw {
			// Only enums and versions: never an error's text or a path.
			if strings.Contains(body, "boom") || strings.Contains(body, string(filepath.Separator)+"tmp") {
				t.Errorf("a failure report carries text or a path: %s", body)
			}
		}
	})
	return func() []callhome.FailureEnvelope {
		mu.Lock()
		defer mu.Unlock()
		return append([]callhome.FailureEnvelope(nil), got...)
	}
}

// testRelease makes a release install whose versions are folders with a bin/meko, runs this test
// as its version running, and points app/current at current ("" for none).
func testRelease(t *testing.T, running, current string, versions ...string) update.App {
	t.Helper()
	app := update.App{Root: filepath.Join(t.TempDir(), "app")}
	for _, v := range versions {
		if err := os.MkdirAll(filepath.Join(app.Dir(v), "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(app.Exe(v), []byte(fakeReleaseBinary(v)), 0o755); err != nil { //nolint:gosec // G306: test
			t.Fatal(err)
		}
	}
	if current != "" {
		if err := app.Repoint(current); err != nil {
			t.Fatal(err)
		}
	}
	old := runningRelease
	runningRelease = func() (update.App, string, bool) { return app, running, true }
	t.Cleanup(func() { runningRelease = old })
	return app
}

// When another program holds Meko's port and nobody chose it, install moves Meko to the next free
// port, says so, and goes on; the server listens there.
func TestInstallPicksAFreePort(t *testing.T) {
	dir, _ := setup(t)
	noRealExtensions(t)
	t.Setenv("MEKO_PORT", "") // the default port, in effect: nobody chose it
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	taken := tcpPort(t, ln)
	l := paths.New(dir)
	if err = l.Ensure(); err != nil {
		t.Fatal(err)
	}
	// A port an earlier install picked counts as nobody's choice too, so it moves again.
	if err = config.SetPickedPort(l.Config, taken); err != nil {
		t.Fatal(err)
	}
	next := taken + 1
	for portFree(next) != nil {
		next++
	}
	useFakeService(t)
	code, out, errs := run("install", "--data-dir", dir)
	want(t, "install", code, 0, out+errs, "  ! Port            "+strconv.Itoa(taken)+
		" is in use by another program · Meko uses "+strconv.Itoa(next)+", set in ")
	want(t, "install's server", code, 0, out, "  ✓ Server          started in ")
	cfg, err := config.Load(l.Config, func(string) string { return "" }, config.Overrides{})
	if err != nil || cfg.Server.Port != next {
		t.Errorf("config.toml's port: %d, %v", cfg.Server.Port, err)
	}
	if p, _ := readInt(l.Port); p != next {
		t.Errorf("the server listens on %d, want %d", p, next)
	}
}

// brokenService is a service whose server fails at every start, as serve does: its error in
// service.log, once per start.
type brokenService struct{ *fakeService }

func (b brokenService) Install(s service.Spec) error {
	b.calls = append(b.calls, "install")
	b.spec = s
	if err := os.WriteFile(b.file, []byte(s.DataDir), 0o600); err != nil {
		return err
	}
	f, err := os.OpenFile(s.ServiceLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString("meko: boom\nmeko: boom\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (b brokenService) Stop() error {
	b.calls = append(b.calls, "stop")
	return nil
}

// A first install whose server keeps failing stops waiting once the service log says so twice,
// shows the error and the log, stops the service so it doesn't restart forever, and leaves the
// setup for the installer to finish. Call home gets the step and the class, not the error.
func TestInstallSaysWhyTheServerDidntStart(t *testing.T) {
	dir, _ := setup(t)
	noRealExtensions(t)
	reports := callHomeReports(t)
	app := testRelease(t, "0.2.0", "", "0.2.0")
	f := &fakeService{file: filepath.Join(t.TempDir(), "meko.service")}
	old := newManager
	newManager = func() (service.Manager, error) { return brokenService{f}, nil }
	t.Cleanup(func() { newManager = old })
	t.Setenv(envInstallStarted, strconv.FormatInt(time.Now().Unix(), 10))
	t.Setenv(envInstallFirst, "1")

	began := time.Now()
	code, out, errs := run("install", "--data-dir", dir)
	if time.Since(began) > 20*time.Second {
		t.Errorf("install waited %s for a server that said it can't start", time.Since(began))
	}
	want(t, "install", code, exitError, out, "  ✗ Server          didn't start: boom\n\n    meko: boom\n    meko: boom\n")
	want(t, "install's way to more", code, exitError, out, "    More                 meko logs --service\n")
	want(t, "install's service", code, exitError, out,
		"    The service          fake status · stopped so it doesn't keep restarting\n")
	want(t, "install's way out", code, exitError, out, "  Fix it, then run the installer again to finish the setup\n")
	want(t, "install's bug form", code, exitError, out, "  Report a bug: "+bugForm+"\n")
	if errs != "" {
		t.Errorf("the failure is said twice, on stderr too: %q", errs)
	}
	if f.calls[len(f.calls)-1] != "stop" {
		t.Errorf("the service wasn't stopped: %v", f.calls)
	}
	if v := app.SetupPending(); v != "0.2.0" {
		t.Errorf("setup-pending = %q, want the version whose setup didn't finish", v)
	}
	got := reports()
	if len(got) != 1 {
		t.Fatalf("call home got %d reports", len(got))
	}
	p := got[0].Payload
	if got[0].Command != callhome.FailureCommand || p.Step != callhome.StepServer || p.Kind != callhome.KindStartFailed ||
		p.Version != "0.2.0" || !p.First || p.Service != "fake" || got[0].InstallID == "" {
		t.Errorf("report %+v", got[0])
	}
}

// An upgrade with a service goes on probation; when the new version's server fails its starts and
// serve puts the old one back, install says so: the old version is running, with its data.
func TestInstallSaysAnUpgradeRolledBack(t *testing.T) {
	dir, port := setup(t)
	noRealExtensions(t)
	configurePort(t, dir, port)
	reports := callHomeReports(t)
	// This test's server says it's "dev": as if serve had rolled back to it.
	app := testRelease(t, "0.2.0", "dev", "dev", "0.2.0")
	useFakeService(t)
	t.Setenv(envInstallFrom, "dev")

	code, out, errs := run("install", "--data-dir", dir)
	want(t, "install", code, exitError, out+errs,
		"  ✗ Server          meko 0.2.0 didn't start: it kept failing to start\n")
	want(t, "install's rollback", code, exitError, out,
		"  ! Rolled back     meko dev is running again · with its data as it was\n")
	want(t, "install's closing", code, exitError, out, "  Meko dev is installed and running\n")
	p, err := app.ReadPending()
	if err != nil || p == nil || p.From != "dev" || p.To != "0.2.0" {
		t.Errorf("the upgrade isn't on probation: %+v %v", p, err)
	}
	if v := app.SetupPending(); v != "" {
		t.Errorf("setup-pending after a rollback: %q", v)
	}
	if got := reports(); len(got) != 1 || got[0].Payload.Kind != callhome.KindRolledBack || got[0].Payload.From != "dev" {
		t.Errorf("call home: %+v", got)
	}
}

// A release install that starts clears its setup marker, and installing the same version again
// isn't an upgrade: nothing goes on probation.
func TestInstallFinishesItsSetup(t *testing.T) {
	dir, port := setup(t)
	noRealExtensions(t)
	configurePort(t, dir, port)
	app := testRelease(t, "0.2.0", "0.2.0", "0.2.0")
	if err := app.MarkSetup("0.2.0", false); err != nil {
		t.Fatal(err)
	}
	useFakeService(t)
	code, out, errs := run("install", "--data-dir", dir)
	want(t, "install", code, 0, out+errs, "  ✓ Server          started in ")
	if v := app.SetupPending(); v != "" {
		t.Errorf("setup-pending after a finished setup: %q", v)
	}
	if p, _ := app.ReadPending(); p != nil {
		t.Errorf("a reinstall went on probation: %+v", p)
	}
}

// The service manager's own check is a ✗ Service row, before anything is written.
func TestInstallSaysTheServiceManagerIsMissing(t *testing.T) {
	dir, _ := setup(t)
	noRealExtensions(t)
	f := &fakeService{file: filepath.Join(t.TempDir(), "meko.service")}
	old := newManager
	newManager = func() (service.Manager, error) { return noSession{f}, nil }
	t.Cleanup(func() { newManager = old })
	code, out, _ := run("install", "--data-dir", dir)
	want(t, "install", code, exitError, out, "  ✗ Service         no login session: install with --no-service\n"+
		"  Nothing was left half-done: running the installer again is safe\n")
	if strings.Contains(out, "Report a bug") {
		t.Errorf("a known failure asks for a bug report:\n%s", out)
	}
}

type noSession struct{ *fakeService }

func (noSession) Preflight() error { return errNoSession }

var errNoSession = &preflightError{"no login session: install with --no-service"}

type preflightError struct{ s string }

func (e *preflightError) Error() string { return e.s }
