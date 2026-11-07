package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/envelope"
	"github.com/yugabyte/meko/src/meko/internal/extproxy"
	"github.com/yugabyte/meko/src/meko/internal/update"
)

func post(t *testing.T, url string, header map[string]string) (int, envelope.Response) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var env envelope.Response
	_ = json.Unmarshal(b, &env)
	return resp.StatusCode, env
}

// agentkRow is GET /api/v1/extensions' row for agentk.
func agentkRow(t *testing.T, base string) ExtensionInfo {
	t.Helper()
	return extensionRow(t, base, "agentk")
}

// extensionRow is GET /api/v1/extensions' row for name.
func extensionRow(t *testing.T, base, name string) ExtensionInfo {
	t.Helper()
	resp, err := http.Get(base + "/api/v1/extensions") //nolint:noctx // a test against a local server
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var env struct {
		Data []ExtensionInfo `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil || resp.StatusCode != 200 {
		t.Fatalf("extensions: %d %v", resp.StatusCode, err)
	}
	for _, r := range env.Data {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no %s in %+v", name, env.Data)
	return ExtensionInfo{}
}

func noProxies(w *Web) { w.Extensions = func() []*extproxy.Proxy { return nil } }

func installed(names ...string) func(*Web) {
	return func(w *Web) { w.Installed = func() []string { return names } }
}

// The list has every known extension: one that isn't installed has no path, and one that is, with
// its socket up, is served at /ext/<name>/.
func TestExtensionListShowsTheCatalog(t *testing.T) {
	base, _ := startWeb(t, config.AccessLocal, "", noProxies, installed())
	r := agentkRow(t, base)
	if r.Title != "AgentK" || r.Description == "" || r.Installed || !r.WebUI || r.Path != "" || r.Available ||
		r.Job != nil {
		t.Errorf("not installed: %+v", r)
	}

	// A nil Installed reports the served ones as installed.
	base, _ = startWeb(t, config.AccessLocal, fakeExtension(t))
	if r = agentkRow(t, base); !r.Installed || r.Path != "/ext/agentk/" || !r.Available {
		t.Errorf("installed: %+v", r)
	}
}

// fakeRun is a Web.Run that writes a line and waits for release; stop fails.
type fakeRun struct {
	release chan struct{}
	mu      sync.Mutex
	calls   []string
}

func (f *fakeRun) run(_ context.Context, name, action string, out io.Writer) error {
	f.mu.Lock()
	f.calls = append(f.calls, name+" "+action)
	f.mu.Unlock()
	_, _ = fmt.Fprintf(out, "%s %s\n", action, name)
	<-f.release
	if action == "stop" {
		return errors.New("stop failed")
	}
	return nil
}

func waitJob(t *testing.T, base string) ExtensionInfo {
	t.Helper()
	for range 200 {
		if r := agentkRow(t, base); r.Job != nil && r.Job.State != JobRunning {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the job didn't finish")
	return ExtensionInfo{}
}

// An action runs as the extension's job: 202 at once, one at a time, then done or failed with its
// output and error.
func TestExtensionActions(t *testing.T) {
	f := &fakeRun{release: make(chan struct{})}
	base, _ := startWeb(t, config.AccessLocal, filepath.Join(shortTemp(t), "gone.sock"), installed("agentk"),
		func(w *Web) { w.Run = f.run })
	code, env := post(t, base+"/api/v1/extensions/agentk/start", nil)
	row, _ := env.Data.(map[string]any)
	job, _ := row["job"].(map[string]any)
	if code != http.StatusAccepted || job["action"] != "start" || job["state"] != JobRunning {
		t.Fatalf("start: %d %+v", code, env)
	}
	if code, env = post(t, base+"/api/v1/extensions/agentk/stop", nil); code != 409 || env.Error.Code != "CONFLICT" ||
		!strings.Contains(env.Error.Message, "its start hasn't finished") {
		t.Errorf("a second action: %d %+v", code, env.Error)
	}
	if r := agentkRow(t, base); r.Job == nil || r.Job.Output != "start agentk\n" {
		t.Errorf("output while it runs: %+v", r.Job)
	}
	f.release <- struct{}{}
	r := waitJob(t, base)
	if r.Job.State != JobDone || r.Job.Error != "" || r.Job.FinishedAt == nil || r.Job.Output != "start agentk\n" {
		t.Errorf("done: %+v", r.Job)
	}

	if code, _ = post(t, base+"/api/v1/extensions/agentk/stop", nil); code != http.StatusAccepted {
		t.Fatalf("stop: %d", code)
	}
	f.release <- struct{}{}
	if r = waitJob(t, base); r.Job.State != JobFailed || r.Job.Error != "stop failed" || r.Job.Action != "stop" {
		t.Errorf("failed: %+v", r.Job)
	}

	for _, path := range []string{"/frob/start", "/agentk/uninstall"} {
		if code, _ = post(t, base+"/api/v1/extensions"+path, nil); code != 404 {
			t.Errorf("%s: %d", path, code)
		}
	}
	if code, _ = post(t, base+"/api/v1/extensions/agentk/start",
		map[string]string{"Origin": "http://example.com"}); code != 403 {
		t.Errorf("another origin: %d", code)
	}
	if len(f.calls) != 2 {
		t.Errorf("ran %v", f.calls)
	}
}

// A failed Install or Start leaves the card once the extension is installed and running anyway, as
// after meko extension install from a terminal. One that's registered but doesn't answer keeps it:
// an install fails after registering when the extension never answers on its socket.
func TestFailedJobOvertaken(t *testing.T) {
	var mu sync.Mutex
	names := []string{}
	register := func(n ...string) { mu.Lock(); defer mu.Unlock(); names = n }
	fail := func(_ context.Context, _, action string, _ io.Writer) error {
		return errors.New("me_kode " + action + " failed: exit status 1")
	}
	sock := filepath.Join(shortTemp(t), "agentk.sock")
	base, _ := startWeb(t, config.AccessLocal, sock,
		func(w *Web) {
			w.Run = fail
			w.Installed = func() []string { mu.Lock(); defer mu.Unlock(); return names }
		})
	if code, _ := post(t, base+"/api/v1/extensions/agentk/install", nil); code != http.StatusAccepted {
		t.Fatalf("install: %d", code)
	}
	if r := waitJob(t, base); r.Job.State != JobFailed || r.Installed {
		t.Fatalf("failed install: %+v", r)
	}

	// Registered, but not answering: the install's own failure, so it stays.
	register("agentk")
	if r := agentkRow(t, base); !r.Installed || r.Available || r.Job == nil || r.Job.State != JobFailed {
		t.Errorf("registered, not answering: %+v", r)
	}

	// Installed from a terminal and running.
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if r := agentkRow(t, base); !r.Installed || !r.Available || r.Job != nil {
		t.Errorf("installed from a terminal: %+v", r)
	}

	// A failed start of one that isn't running stays.
	_ = ln.Close()
	if code, _ := post(t, base+"/api/v1/extensions/agentk/start", nil); code != http.StatusAccepted {
		t.Fatalf("start: %d", code)
	}
	if r := waitJob(t, base); r.Available || r.Job.State != JobFailed || r.Job.Action != "start" {
		t.Errorf("failed start: %+v", r)
	}
}

// An installed extension whose release Meko installed shows its version and can be updated; one Meko
// didn't install (a build from source) can't.
func TestExtensionListShowsReleasesMekoUpdates(t *testing.T) {
	f := &fakeRun{release: make(chan struct{})}
	close(f.release)
	release := func(w *Web) {
		w.ExtensionUpdates = func(name string) ExtensionUpdate {
			if name == "present" {
				return ExtensionUpdate{Version: "0.20261024.0", Updatable: true, RolledBack: []string{}}
			}
			return ExtensionUpdate{RolledBack: []string{}}
		}
	}
	base, _ := startWeb(t, config.AccessLocal, "", noProxies, installed("agentk", "present"), release,
		func(w *Web) { w.Run = f.run })
	if r := extensionRow(t, base, "present"); r.Version != "0.20261024.0" || !r.Updatable {
		t.Errorf("present: %+v", r)
	}
	if r := agentkRow(t, base); r.Version != "" || r.Updatable {
		t.Errorf("agentk: %+v", r)
	}
	if code, _ := post(t, base+"/api/v1/extensions/present/update", nil); code != http.StatusAccepted {
		t.Fatalf("update: %d", code)
	}
	for range 200 {
		if r := extensionRow(t, base, "present"); r.Job != nil && r.Job.State != JobRunning {
			if r.Job.Action != "update" || r.Job.State != JobDone || r.Job.Output != "update present\n" {
				t.Errorf("update: %+v", r.Job)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the update didn't finish")
}

// Only install runs on an extension that isn't installed, and a server with no runner has no actions.
func TestExtensionActionsNeedAnInstall(t *testing.T) {
	f := &fakeRun{release: make(chan struct{})}
	close(f.release)
	base, _ := startWeb(t, config.AccessLocal, "", noProxies, installed(), func(w *Web) { w.Run = f.run })
	if code, env := post(t, base+"/api/v1/extensions/agentk/start", nil); code != 409 ||
		env.Error.Message != "AgentK isn't installed." {
		t.Errorf("start: %d %+v", code, env.Error)
	}
	if code, _ := post(t, base+"/api/v1/extensions/agentk/install", nil); code != http.StatusAccepted {
		t.Errorf("install: %d", code)
	}
	if r := waitJob(t, base); r.Job.State != JobDone || r.Job.Output != "install agentk\n" {
		t.Errorf("install: %+v", r.Job)
	}

	base, _ = startWeb(t, config.AccessLocal, "", noProxies, installed())
	if code, _ := post(t, base+"/api/v1/extensions/agentk/install", nil); code != 404 {
		t.Errorf("no runner: %d", code)
	}
}

// With remote access, another site's page can't run an action.
func TestExtensionActionsRefuseOtherSites(t *testing.T) {
	f := &fakeRun{release: make(chan struct{})}
	base, _ := startWeb(t, config.AccessRemote, "", installed("agentk"), func(w *Web) { w.Run = f.run })
	crossSite := map[string]string{"Sec-Fetch-Site": "cross-site"}
	if code, _ := post(t, base+"/api/v1/extensions/agentk/start", crossSite); code != 403 {
		t.Errorf("another site's page: %d", code)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %v", f.calls)
	}
}

// The jobs are the listener owner's: a server built after a restart of the listener sees the job
// still running and refuses a second action on the extension.
func TestJobsOutliveTheServer(t *testing.T) {
	f := &fakeRun{release: make(chan struct{})}
	jobs := NewExtensionJobs()
	shared := func(w *Web) { w.Run, w.Jobs = f.run, jobs }
	first, _ := startWeb(t, config.AccessLocal, "", noProxies, installed("agentk"), shared)
	if code, _ := post(t, first+"/api/v1/extensions/agentk/install", nil); code != http.StatusAccepted {
		t.Fatalf("install: %d", code)
	}
	second, _ := startWeb(t, config.AccessLocal, "", noProxies, installed("agentk"), shared)
	if r := agentkRow(t, second); r.Job == nil || r.Job.State != JobRunning || r.Job.Action != "install" {
		t.Errorf("the new server's job: %+v", r.Job)
	}
	if code, _ := post(t, second+"/api/v1/extensions/agentk/start", nil); code != 409 {
		t.Errorf("a second action on the new server: %d", code)
	}
	if !jobs.Busy() {
		t.Error("not busy while the install runs")
	}
	f.release <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	jobs.WaitIdle(ctx)
	if jobs.Busy() || waitJob(t, second).Job.State != JobDone {
		t.Error("the install didn't end")
	}
}

// Cancel ends the context the running jobs were given, and waits for them.
func TestCancelStopsTheJobs(t *testing.T) {
	jobs := NewExtensionJobs()
	run := func(ctx context.Context, _, _ string, _ io.Writer) error {
		<-ctx.Done()
		return errors.New("it was stopped")
	}
	base, _ := startWeb(t, config.AccessLocal, "", noProxies, installed(),
		func(w *Web) { w.Run, w.Jobs = run, jobs })
	if code, _ := post(t, base+"/api/v1/extensions/agentk/install", nil); code != http.StatusAccepted {
		t.Fatalf("install: %d", code)
	}
	jobs.Cancel(5 * time.Second)
	if r := agentkRow(t, base); jobs.Busy() || r.Job.State != JobFailed || r.Job.Error != "it was stopped" {
		t.Errorf("after Cancel: busy %v, %+v", jobs.Busy(), r.Job)
	}
}

// A job keeps the end of its output, from a line's start, while it's written and read at once.
func TestJobOutputKeepsTheEnd(t *testing.T) {
	x := NewExtensionJobs()
	j, ok := x.start("agentk", "install", false)
	if !ok {
		t.Fatal("not started")
	}
	if _, again := x.start("agentk", "stop", false); again {
		t.Error("a second job started while one runs")
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 2000 {
			_, _ = fmt.Fprintf(j, "line %04d of the installer's output\n", i)
		}
	})
	for range 100 {
		_ = x.get("agentk")
	}
	wg.Wait()
	x.done(j, nil)
	got := x.get("agentk")
	if len(got.Output) > JobOutputLimit || !strings.HasPrefix(got.Output, "line ") ||
		!strings.HasSuffix(got.Output, "line 1999 of the installer's output\n") || got.State != JobDone {
		t.Errorf("%d bytes, state %s, starting %q", len(got.Output), got.State, got.Output[:20])
	}
	if _, ok = x.start("agentk", "stop", false); !ok {
		t.Error("a finished job blocks the next")
	}
}

// Check now checks in the request and answers with the row, or 409 UPDATE_BUSY while an update
// holds the lock; PATCH saves Update automatically, and refuses a body without it.
func TestExtensionCheckAndAutoUpdate(t *testing.T) {
	auto := map[string]bool{"present": true}
	busy := false
	checks := 0
	with := func(w *Web) {
		w.Run = (&fakeRun{}).run
		w.ExtensionUpdates = func(name string) ExtensionUpdate {
			return ExtensionUpdate{Version: "0.20261024.0", Updatable: true, Latest: "0.20261025.0",
				UpdateAvailable: checks > 0, AutoUpdate: auto[name], RolledBack: []string{}}
		}
		w.CheckExtensions = func(context.Context) error {
			if busy {
				return update.ErrBusy
			}
			checks++
			return nil
		}
		w.SetAutoUpdate = func(name string, on bool) error { auto[name] = on; return nil }
	}
	base, _ := startWeb(t, config.AccessLocal, "", noProxies, installed("present"), with)
	code, env := post(t, base+"/api/v1/extensions/present/check", nil)
	if row, _ := env.Data.(map[string]any); code != http.StatusOK || row["update_available"] != true || checks != 1 {
		t.Errorf("check: %d %+v", code, env)
	}
	busy = true
	if code, env = post(t, base+"/api/v1/extensions/present/check", nil); code != http.StatusConflict ||
		env.Error == nil || env.Error.Code != "UPDATE_BUSY" || env.Data == nil {
		t.Errorf("busy check: %d %+v", code, env)
	}
	patch := func(name, body string) (int, envelope.Response) {
		req, _ := http.NewRequest(http.MethodPatch, base+"/api/v1/extensions/"+name, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var reply envelope.Response
		_ = json.NewDecoder(resp.Body).Decode(&reply)
		return resp.StatusCode, reply
	}
	if code, env = patch("present", `{"auto_update": false}`); code != http.StatusOK || auto["present"] {
		t.Errorf("patch: %d %+v", code, env)
	}
	if row := extensionRow(t, base, "present"); row.AutoUpdate {
		t.Errorf("row after patch: %+v", row)
	}
	if code, _ = patch("present", `{}`); code != http.StatusBadRequest {
		t.Errorf("patch without auto_update: %d", code)
	}
	if code, _ = patch("agentk", `{"auto_update": true}`); code != http.StatusConflict {
		t.Errorf("patch of one not installed: %d", code)
	}
}
