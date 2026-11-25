package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/callhome"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/store"
)

// mainRun runs prog as cmd/meko does, through Main, which records the first commands.
func mainRun(prog string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(prog, args, &out, &errb)
	return code, out.String(), errb.String()
}

// trackFirstCommands starts the first commands' list in a temp data folder that every command
// uses (MEKO_HOME), as a first install does, and returns the folder.
func trackFirstCommands(t *testing.T) string {
	t.Helper()
	dir, _ := setup(t)
	noRealExtensions(t)
	t.Setenv(paths.HomeEnv, dir)
	t.Setenv(envParent, "")
	if err := paths.New(dir).Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := callhome.StartFirstCommands(paths.New(dir).FirstCommands, "dev", time.Now()); err != nil {
		t.Fatal(err)
	}
	return dir
}

func firstCommands(t *testing.T, dir string) []callhome.Command {
	t.Helper()
	list, err := callhome.ReadFirstCommands(paths.New(dir).FirstCommands, knownCommand)
	if err != nil {
		t.Fatal(err)
	}
	return list.Commands
}

func fakeTerminal(t *testing.T, on bool) {
	t.Helper()
	old := stdoutIsTerminal
	stdoutIsTerminal = func(any) bool { return on }
	t.Cleanup(func() { stdoutIsTerminal = old })
}

// Every run is recorded by its name and its flags' names, never what followed them, with whether
// its output went to a terminal, its exit code and its time.
func TestRecordsTheFirstCommands(t *testing.T) {
	dir := trackFirstCommands(t)
	fakeTerminal(t, true)
	mainRun(progMeko, "status", "--verbose")
	mainRun(progMeko)
	mainRun(progMeko, "nosuch", "sk-test-token")
	mainRun(progMeko, "extension", "delete", "--force")
	mainRun(progMeko, "ui", "start", "--help")
	mainRun(progMeko, "extension")
	mainRun(progMeko, "help", "agent")
	mainRun(progMeko, "kode", "sessions", "my-repo")
	fakeTerminal(t, false)
	mainRun(progMekoCLI, "search", "my secret query", "--datapack", "acme-notes")
	mainRun(progMeko, "status", "--bogus")

	got := firstCommands(t, dir)
	type row struct {
		cmd, flags string
		tty        bool
		exit       int
	}
	wants := []row{
		{"status", "verbose", true, exitNotRunning},
		{"help", "", true, exitOK},
		{callhome.UnknownCommand, "", true, exitUsage},
		{"extension delete", "force", true, exitUsage},
		{"ui start", "help", true, exitOK},
		{"extension", "", true, exitUsage},
		{"agent", "help", true, exitOK},
		{"kode", "", true, exitError},
		{"search", "datapack", false, exitNotRunning},
		{"status", "", false, exitUsage},
	}
	if len(got) != len(wants) {
		t.Fatalf("recorded %d: %+v", len(got), got)
	}
	for i, w := range wants {
		g := got[i]
		if g.N != i+1 || g.Cmd != w.cmd || strings.Join(g.Flags, ",") != w.flags || g.TTY != w.tty || g.Exit != w.exit ||
			g.Version != "dev" || g.Ms < 0 || g.MinsSinceInstall != 0 {
			t.Errorf("command %d: %+v, want %+v", i+1, g, w)
		}
	}
	b, err := os.ReadFile(paths.New(dir).FirstCommands)
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{"sk-test-token", "my secret query", "acme-notes", "my-repo", "sessions", "bogus", dir}
	for _, secret := range secrets {
		if strings.Contains(string(b), secret) {
			t.Errorf("the list holds %q:\n%s", secret, b)
		}
	}
}

// Nothing is recorded for an install that didn't start a list, for what no one runs by hand, or
// for a meko that meko ran; and the list stops at FirstCommandsMax.
func TestRecordsOnlyWhatCounts(t *testing.T) {
	dir := trackFirstCommands(t)
	tr := &tracked{}
	if runProg(progMeko, []string{"serve", "--bogus"}, io.Discard, io.Discard, tr); !tr.skip {
		t.Error("serve isn't skipped")
	}
	tr.finish(exitUsage, time.Now(), io.Discard)
	t.Setenv(envParent, "1")
	mainRun(progMeko, "version")
	if got := firstCommands(t, dir); len(got) != 0 {
		t.Fatalf("recorded: %+v", got)
	}
	t.Setenv(envParent, "")
	for range callhome.FirstCommandsMax + 3 {
		mainRun(progMeko, "version")
	}
	if got := firstCommands(t, dir); len(got) != callhome.FirstCommandsMax {
		t.Errorf("recorded %d", len(got))
	}

	// No list: nothing is made.
	other := shortDir(t)
	t.Setenv(paths.HomeEnv, other)
	mainRun(progMeko, "version")
	if _, err := os.Stat(paths.New(other).FirstCommands); !os.IsNotExist(err) {
		t.Errorf("a list was made: %v", err)
	}
}

// A first install begins the list and isn't on it; installing again keeps the list and is.
func TestFirstInstallStartsTheList(t *testing.T) {
	dir, port := setup(t)
	noRealExtensions(t)
	configurePort(t, dir, port)
	useFakeService(t)
	t.Setenv(envInstallFirst, "1")
	code, out, errs := mainRun(progMeko, "install", "--data-dir", dir)
	want(t, "install", code, 0, out+errs, "is running")
	list, err := callhome.ReadFirstCommands(paths.New(dir).FirstCommands, knownCommand)
	if err != nil || list.Started.IsZero() || len(list.Commands) != 0 {
		t.Fatalf("after a first install: %+v %v", list, err)
	}
	if code, out, errs = run("stop", "--data-dir", dir); code != 0 {
		t.Fatalf("stop: %d %s%s", code, out, errs)
	}
	t.Setenv(envInstallFirst, "")
	code, out, errs = mainRun(progMeko, "install", "--data-dir", dir)
	want(t, "install again", code, 0, out+errs, "is running")
	again, _ := callhome.ReadFirstCommands(paths.New(dir).FirstCommands, knownCommand)
	if !again.Started.Equal(list.Started) || len(again.Commands) != 1 || again.Commands[0].Cmd != "install" {
		t.Errorf("after installing again: %+v", again)
	}
}

// leavingReports collects uninstall's and destroy's events.
func leavingReports(t *testing.T) func() []callhome.UninstallEnvelope {
	t.Helper()
	var mu sync.Mutex
	var got []callhome.UninstallEnvelope
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var e callhome.UninstallEnvelope
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &e); err != nil {
			t.Errorf("%v: %s", err, b)
		}
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MEKO_CALL_HOME_URL", srv.URL)
	return func() []callhome.UninstallEnvelope {
		mu.Lock()
		defer mu.Unlock()
		return append([]callhome.UninstallEnvelope(nil), got...)
	}
}

// meko.db, with the install's id and first heartbeat's day, as a server leaves it.
func makeDatabase(t *testing.T, dir string, settings map[string]string) {
	t.Helper()
	st, err := store.Open(paths.New(dir).DB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	if err = st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for k, v := range settings {
		if err = st.SetSetting(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
}

// uninstall and destroy each send their event once they've worked, with the first commands so far
// and themselves last; declined, or with usage stats off, they send nothing.
func TestUninstallAndDestroySendTheirEvent(t *testing.T) {
	dir := trackFirstCommands(t)
	t.Setenv("XDG_DATA_HOME", "")
	svc := useFakeService(t)
	// The service runs another data folder: not this one's, so the event says none.
	if err := os.WriteFile(svc.file, []byte(shortDir(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	reports := leavingReports(t)
	makeDatabase(t, dir, map[string]string{store.KeyInstallID: "id-1", callhome.KeyFirstDay: "2026-11-09"})
	mainRun(progMeko, "status")

	code, out, errs := mainRun(progMeko, "uninstall")
	want(t, "uninstall", code, 0, out+errs, "Meko removed")
	got := reports()
	if len(got) != 1 {
		t.Fatalf("%d events", len(got))
	}
	e := got[0]
	p := e.Payload
	if e.Command != callhome.UninstallCommand || e.InstallID != "id-1" || p.Purge || p.FirstDay != "2026-11-09" ||
		p.Version != "dev" || p.Service != "none" || len(p.FirstCommands) != 2 ||
		p.FirstCommands[0].Cmd != "status" || p.FirstCommands[1].Cmd != "uninstall" || p.FirstCommands[1].N != 2 {
		t.Errorf("uninstall's event: %+v", e)
	}

	// Declined: nothing removed, nothing sent.
	withStdin(t, "n\n")
	code, _, errs = mainRun(progMeko, "destroy")
	want(t, "destroy, declined", code, exitError, errs, "Nothing was removed")
	if n := len(reports()); n != 1 {
		t.Errorf("a declined destroy sent: %d events", n)
	}
	// The web UI's switch turned usage stats off: nothing sent.
	makeDatabase(t, dir, map[string]string{callhome.KeyEnabled: "false"})
	code, _, _ = mainRun(progMeko, "uninstall")
	if n := len(reports()); code != exitOK || n != 1 {
		t.Errorf("sent with usage stats off: %d events", n)
	}
	makeDatabase(t, dir, map[string]string{callhome.KeyEnabled: "true"})

	// destroy sends after the folder is gone, from what it read before; the service runs this folder.
	if err := os.WriteFile(svc.file, []byte(dir), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs = mainRun(progMeko, "destroy", "--force", "--with-extensions")
	want(t, "destroy", code, 0, out+errs, "Removed "+dir)
	gone(t, dir)
	got = reports()
	if len(got) != 2 {
		t.Fatalf("%d events", len(got))
	}
	e, p = got[1], got[1].Payload
	last := p.FirstCommands[len(p.FirstCommands)-1]
	if e.Command != callhome.DestroyCommand || e.InstallID != "id-1" || !p.WithExtensions || p.Service != "fake" ||
		last.Cmd != "destroy" || strings.Join(last.Flags, ",") != "force,with-extensions" {
		t.Errorf("destroy's event: %+v", e)
	}

	// Nothing of Meko's left to destroy: nothing to say.
	mainRun(progMeko, "destroy", "--force")
	if n := len(reports()); n != 2 {
		t.Errorf("destroy with no data folder sent: %d events", n)
	}
}
