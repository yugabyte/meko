package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/envelope"
	"github.com/yugabyte/meko/backend/internal/extproxy"
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
		if r.Name == "agentk" {
			return r
		}
	}
	t.Fatalf("no agentk in %+v", env.Data)
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

// Without local access, an action needs the owner's session.
func TestExtensionActionsNeedASession(t *testing.T) {
	f := &fakeRun{release: make(chan struct{})}
	base, _ := startWeb(t, config.AccessRemote, "", installed("agentk"), func(w *Web) { w.Run = f.run })
	if code, _ := post(t, base+"/api/v1/extensions/agentk/start", nil); code != 401 {
		t.Errorf("no session: %d", code)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %v", f.calls)
	}
}

// A job keeps the end of its output, from a line's start, while it's written and read at once.
func TestJobOutputKeepsTheEnd(t *testing.T) {
	var x extJobs
	j, ok := x.start("agentk", "install")
	if !ok {
		t.Fatal("not started")
	}
	if _, again := x.start("agentk", "stop"); again {
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
	j.finish(nil)
	got := x.get("agentk")
	if len(got.Output) > JobOutputLimit || !strings.HasPrefix(got.Output, "line ") ||
		!strings.HasSuffix(got.Output, "line 1999 of the installer's output\n") || got.State != JobDone {
		t.Errorf("%d bytes, state %s, starting %q", len(got.Output), got.State, got.Output[:20])
	}
	if _, ok = x.start("agentk", "stop"); !ok {
		t.Error("a finished job blocks the next")
	}
}
