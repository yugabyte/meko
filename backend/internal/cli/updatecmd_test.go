package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/service"
)

const updateOld, updateNext = "0.20261021.0", "0.20261022.0"

// With Meko stopped, meko update installs the release, says so in its Release row, and leaves it
// for the next start.
func TestUpdateInstallsMekoForTheNextStart(t *testing.T) {
	dir, _ := setup(t)
	app := releaseInstall(t, updateOld, updateNext)
	var out, errs bytes.Buffer
	exe, code := updateMeko(paths.New(dir), false, &out, &errs)
	want(t, "update", code, 0, out.String()+errs.String(),
		"Installing meko "+updateNext+"\n  ✓ Release         meko "+updateNext+" installed\n"+
			"Installed meko "+updateNext+"; it runs at the next meko start\n")
	if exe != app.CurrentExe() || app.Current() != updateNext {
		t.Errorf("exe %q, current %q", exe, app.Current())
	}
}

// A release that doesn't unpack ends the Release row with ✗, and the installed version stays.
func TestUpdateSaysWhenMekosReleaseFails(t *testing.T) {
	dir, _ := setup(t)
	app := releaseInstall(t, updateOld, updateNext)
	serveMekoRelease(t, updateNext, []byte("not a tarball"))
	var out, errs bytes.Buffer
	_, code := updateMeko(paths.New(dir), false, &out, &errs)
	want(t, "update", code, exitError, out.String(), "  ✗ Release         installing meko "+updateNext+"\n")
	if app.Current() != updateOld {
		t.Errorf("current %q", app.Current())
	}
}

// Under the service, the restart has its Server row: ✓ with where the new server runs, or ✗ when
// the service can't restart it or it doesn't answer.
func TestUpdateRestartsMekoThroughTheService(t *testing.T) {
	dir, port := setup(t)
	noRealExtensions(t)
	configurePort(t, dir, port)
	f := useFakeService(t)
	code, out, errs := run("install", "--data-dir", dir)
	want(t, "install", code, 0, out+errs, "  ✓ Server          running at http://127.0.0.1:"+port)
	releaseInstall(t, updateOld, updateNext)

	var o, e bytes.Buffer
	_, code = updateMeko(paths.New(dir), false, &o, &e)
	want(t, "update", code, 0, o.String()+e.String(),
		"Restarting into meko "+updateNext+"\n  ✓ Server          running at http://127.0.0.1:"+port+" (pid ")
	want(t, "update", code, 0, o.String(), "Meko is running "+updateNext+"\n")
	if f.calls[len(f.calls)-1] != "start" || f.calls[len(f.calls)-2] != "restart" {
		t.Errorf("the update didn't restart the service: %v", f.calls)
	}

	old := serviceStartTimeout
	serviceStartTimeout = time.Second
	t.Cleanup(func() { serviceStartTimeout = old })
	for _, tc := range []struct {
		what string
		err  error
	}{{"a failed restart", errors.New("no restart")}, {"a server that doesn't answer", nil}} {
		releaseInstall(t, updateNext, "0.20261023.0")
		withRestart(t, f, tc.err)
		o.Reset()
		e.Reset()
		_, code = updateMeko(paths.New(dir), false, &o, &e)
		want(t, tc.what, code, exitError, o.String(), "  ✗ Server          restarting into meko 0.20261023.0\n")
		if strings.Contains(o.String(), "Meko is running") {
			t.Errorf("%s: %q", tc.what, o.String())
		}
		if err := f.Start(); err != nil { // for the next case, and setup's stop
			t.Fatal(err)
		}
	}
}

// brokenRestart is the fake service with a Restart that fails with err, or with err nil stops the
// server and starts nothing.
type brokenRestart struct {
	*fakeService
	err error
}

func (b brokenRestart) Restart() error {
	if b.err != nil {
		return b.err
	}
	_, _ = stopServer(paths.New(b.DataDir()))
	return nil
}

// withRestart makes serviceOf return f with brokenRestart's Restart.
func withRestart(t *testing.T, f *fakeService, err error) {
	t.Helper()
	old := newManager
	newManager = func() (service.Manager, error) { return brokenRestart{f, err}, nil }
	t.Cleanup(func() { newManager = old })
}
