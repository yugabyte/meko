package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/update"
)

// fakeUpdates is a Web.Updates whose CheckNow and StartApply fail with err.
type fakeUpdates struct {
	err     error
	checks  int
	applies int
}

func (f *fakeUpdates) Status() update.Status {
	return update.Status{Running: "0.20261021.0", Release: true, Latest: "0.20261022.0", Available: true,
		RolledBack: []string{}}
}

func (f *fakeUpdates) CheckNow(context.Context) (update.Status, error) {
	f.checks++
	return f.Status(), f.err
}

func (f *fakeUpdates) StartApply() (update.Status, error) {
	f.applies++
	st := f.Status()
	st.Job = &update.Job{State: update.JobRunning, Step: update.StepChecking}
	return st, f.err
}

func TestUpdateRoutes(t *testing.T) {
	f := &fakeUpdates{}
	base, _ := startWeb(t, config.AccessLocal, "", noProxies, func(w *Web) { w.Updates = f })
	code, env := get(t, base+"/api/v1/update", nil, "")
	m, _ := env.Data.(map[string]any)
	if code != 200 || m["running"] != "0.20261021.0" || m["available"] != true || m["release"] != true {
		t.Errorf("status: %d %+v", code, env)
	}
	if code, env = post(t, base+"/api/v1/update/check", nil); code != 200 || f.checks != 1 {
		t.Errorf("check: %d %+v", code, env)
	}
	code, env = post(t, base+"/api/v1/update/apply", nil)
	m, _ = env.Data.(map[string]any)
	job, _ := m["job"].(map[string]any)
	if code != http.StatusAccepted || f.applies != 1 || job["state"] != update.JobRunning {
		t.Errorf("apply: %d %+v", code, env)
	}

	for err, want := range map[error]string{update.ErrBusy: "UPDATE_BUSY", update.ErrPending: "UPDATE_PENDING"} {
		f.err = err
		if code, env = post(t, base+"/api/v1/update/apply", nil); code != 409 || env.Error.Code != want {
			t.Errorf("apply with %v: %d %+v", err, code, env.Error)
		}
		if code, env = post(t, base+"/api/v1/update/check", nil); code != 409 || env.Error.Code != want {
			t.Errorf("check with %v: %d %+v", err, code, env.Error)
		}
	}
}

// While an Extensions-page action runs, an install is refused, naming it; a check isn't.
func TestUpdateWaitsForAnExtensionAction(t *testing.T) {
	f := &fakeUpdates{}
	run := &fakeRun{release: make(chan struct{})}
	base, _ := startWeb(t, config.AccessLocal, "", noProxies, installed(), func(w *Web) {
		w.Updates, w.Run = f, run.run
	})
	if code, _ := post(t, base+"/api/v1/extensions/kode/install", nil); code != http.StatusAccepted {
		t.Fatalf("install: %d", code)
	}
	code, env := post(t, base+"/api/v1/update/apply", nil)
	if code != 409 || env.Error.Code != "EXTENSION_BUSY" || !strings.Contains(env.Error.Message, "Kode's install") ||
		f.applies != 0 {
		t.Errorf("apply: %d %+v", code, env.Error)
	}
	if code, _ = post(t, base+"/api/v1/update/check", nil); code != 200 {
		t.Errorf("check: %d", code)
	}
	close(run.release)
	waitJob(t, base)
	if code, _ = post(t, base+"/api/v1/update/apply", nil); code != http.StatusAccepted {
		t.Errorf("apply after: %d", code)
	}
}

// A build from source has no updates: the status says so and the actions are refused.
func TestUpdateRoutesFromSource(t *testing.T) {
	base, _ := startWeb(t, config.AccessLocal, "", noProxies)
	code, env := get(t, base+"/api/v1/update", nil, "")
	m, _ := env.Data.(map[string]any)
	if code != 200 || m["release"] != false || m["running"] != "test" {
		t.Errorf("status: %d %+v", code, env)
	}
	for _, p := range []string{"/check", "/apply"} {
		if code, env = post(t, base+"/api/v1/update"+p, nil); code != 409 ||
			!strings.Contains(env.Error.Message, "built from source") {
			t.Errorf("%s: %d %+v", p, code, env.Error)
		}
	}
}

// A release install whose updater couldn't be set up says why, and refuses checks and installs.
func TestUpdateRoutesWhenOff(t *testing.T) {
	off := update.Off{Version: "0.20261021.0", Reason: "no schema"}
	base, _ := startWeb(t, config.AccessLocal, "", noProxies, func(w *Web) { w.Updates = off })
	code, env := get(t, base+"/api/v1/update", nil, "")
	m, _ := env.Data.(map[string]any)
	if code != 200 || m["release"] != true || m["off"] != "no schema" {
		t.Errorf("status: %d %+v", code, env)
	}
	for _, p := range []string{"/check", "/apply"} {
		if code, env = post(t, base+"/api/v1/update"+p, nil); code != 409 || env.Error.Code != "UPDATE_OFF" ||
			env.Error.Message != "Updates are off: no schema. See meko logs." {
			t.Errorf("%s: %d %+v", p, code, env.Error)
		}
	}
}

// Another site's page gets none of the routes, and the API listener has none of them.
func TestUpdateRoutesRefuseOtherSites(t *testing.T) {
	f := &fakeUpdates{}
	base, _ := startWeb(t, config.AccessRemote, "", noProxies, func(w *Web) { w.Updates = f })
	crossSite := map[string]string{"Sec-Fetch-Site": "cross-site"}
	if code, _ := get(t, base+"/api/v1/update", crossSite, ""); code != 403 {
		t.Errorf("status: %d", code)
	}
	if code, _ := post(t, base+"/api/v1/update/apply", crossSite); code != 403 || f.applies != 0 {
		t.Errorf("apply: %d", code)
	}
	_, api, _ := start(t)
	bearer := map[string]string{"Authorization": "Bearer " + token}
	if code, _ := get(t, api+"/api/v1/update", bearer, ""); code != 404 {
		t.Errorf("API listener: %d", code)
	}
	if code, _ := post(t, api+"/api/v1/update/apply", bearer); code != 404 {
		t.Errorf("API listener apply: %d", code)
	}
}
