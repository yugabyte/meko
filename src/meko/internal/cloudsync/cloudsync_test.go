package cloudsync_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/cloudsync"
	"github.com/yugabyte/meko/src/meko/internal/memory"
	"github.com/yugabyte/meko/src/meko/internal/testenv"
)

// fakeCloud is Meko Cloud's import routes: a precheck that fits unless told otherwise, and imports
// that finish (or fail) when the test says.
type fakeCloud struct {
	mu        sync.Mutex
	fits      bool
	missing   bool          // no import routes at all
	result    string        // what an import ends as: done or failed
	release   chan struct{} // closed when an import may end
	started   map[string]bool
	canceled []string
	prechecks []map[string]any
}

func newFakeCloud() *fakeCloud {
	f := &fakeCloud{fits: true, result: "done", release: make(chan struct{}), started: map[string]bool{}}
	close(f.release)
	return f
}

func (f *fakeCloud) IsLinked() (bool, error) { return true, nil }

func reply(status int, data any) cloudlink.Reply {
	b, _ := json.Marshal(map[string]any{"data": data, "error": nil})
	return cloudlink.Reply{Status: status, Body: b}
}

func (f *fakeCloud) Pass(_ context.Context, method, path string, body io.Reader, _ string) (cloudlink.Reply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.missing {
		return cloudlink.Reply{Status: 404, Body: []byte(`{"message":"Not Found"}`)}, nil
	}
	switch {
	case method == "POST" && path == "/datapacks/import/precheck":
		var in map[string]any
		_ = json.NewDecoder(body).Decode(&in)
		f.prechecks = append(f.prechecks, in)
		check := map[string]any{"check": "datapacks", "used": 4, "adding": 1, "limit": 10, "ok": f.fits}
		if !f.fits {
			check["message"] = "Your Free plan allows 10 datapacks."
		}
		return reply(200, map[string]any{"ok": f.fits, "plan": "free", "checks": []any{check}}), nil
	case method == "POST" && path == "/datapacks/import":
		return reply(201, map[string]any{"import_id": "imp-1", "upload_url": "https://uploads.example/x",
			"fields": map[string]string{"key": "imports/imp-1.zip"}}), nil
	case method == "POST" && path == "/datapacks/import/imp-1/start":
		f.started["imp-1"] = true
		return reply(202, map[string]any{"status": "importing"}), nil
	case method == "GET" && path == "/datapacks/import/imp-1":
		select {
		case <-f.release:
			return reply(200, map[string]any{"status": f.result, "web_url": "https://cloud.example/datapacks/x"}), nil
		default:
			return reply(200, map[string]any{"status": "importing"}), nil
		}
	case method == "DELETE" && path == "/datapacks/import/imp-1":
		f.canceled = append(f.canceled, "imp-1")
		return reply(200, map[string]any{}), nil
	}
	return cloudlink.Reply{Status: 404, Body: []byte(`{"data":null,"error":{"code":"NOT_FOUND","message":"no"}}`)}, nil
}

type harness struct {
	e      *testenv.Env
	cloud  *fakeCloud
	s      *cloudsync.Syncer
	upload []byte
	fail   error // an upload that fails
}

func setup(t *testing.T) *harness {
	t.Helper()
	h := &harness{e: testenv.Start(t, testenv.Options{}), cloud: newFakeCloud()}
	h.s = &cloudsync.Syncer{Service: h.e.Service, Cloud: h.cloud, Version: "test", TempDir: t.TempDir(),
		Poll: 5 * time.Millisecond, Timeout: 5 * time.Second,
		Upload: func(_ context.Context, _ string, _ map[string]string, f *os.File) error {
			if h.fail != nil {
				return h.fail
			}
			b, err := io.ReadAll(f)
			h.upload = b
			return err
		}}
	return h
}

func (h *harness) wait(t *testing.T, id string) cloudsync.Run {
	t.Helper()
	for range 400 {
		if r, ok := h.s.Status(id); ok && r.State != cloudsync.StateSyncing {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the sync of %s didn't end", id)
	return cloudsync.Run{}
}

func (h *harness) exists(t *testing.T, id string) bool {
	t.Helper()
	_, err := h.e.Service.Datapack(context.Background(), id)
	if err != nil && !errors.Is(err, memory.ErrNoDatapack) {
		t.Fatal(err)
	}
	return err == nil
}

// A datapack that fits is exported with where it's filed, imported, and then deleted here; while it
// syncs, it takes no writes.
func TestSyncMovesTheDatapack(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	d, err := h.e.Service.CreateDatapack(ctx, "invoices", "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := h.e.Service.CreateProject(ctx, memory.NewProject{Name: "finance", Folders: []string{"invoices"}})
	if err != nil {
		t.Fatal(err)
	}
	folders, _, err := h.e.Service.ProjectTree(ctx, p.ID)
	if err != nil || len(folders) != 1 {
		t.Fatal(folders, err)
	}
	if err = h.e.Service.PlaceDatapack(ctx, d.ID, folders[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.e.Service.PutArtifact(ctx, d.ID, "a", "notes.txt", "text/plain", []byte("hello")); err != nil {
		t.Fatal(err)
	}

	h.cloud.release = make(chan struct{}) // the import waits until the writes are checked
	run, err := h.s.Start(ctx, d.ID)
	if err != nil || run.State != cloudsync.StateSyncing {
		t.Fatalf("start: %+v %v", run, err)
	}
	for range 200 {
		if r, _ := h.s.Status(d.ID); r.Step == "importing" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err = h.e.Service.PutArtifact(ctx, d.ID, "a", "late.txt", "text/plain", []byte("x")); !errors.Is(err,
		memory.ErrDatapackSyncing) {
		t.Errorf("a write while syncing: %v", err)
	}
	close(h.cloud.release)

	run = h.wait(t, d.ID)
	if run.State != cloudsync.StateDone || run.WebURL == "" {
		t.Fatalf("done: %+v", run)
	}
	if h.exists(t, d.ID) {
		t.Error("the synced datapack is still here")
	}
	if jobs, _ := h.e.Service.SyncJobs(ctx); len(jobs) != 0 {
		t.Errorf("jobs left: %v", jobs)
	}
	zr, err := zip.NewReader(bytes.NewReader(h.upload), int64(len(h.upload)))
	if err != nil {
		t.Fatal(err)
	}
	var head map[string]any
	for _, f := range zr.File {
		if f.Name == "datapack.json" {
			rc, _ := f.Open()
			_ = json.NewDecoder(rc).Decode(&head)
		}
	}
	pl, _ := head["placement"].(map[string]any)
	if pl["project_name"] != "finance" || pl["folder_name"] != "invoices" {
		t.Errorf("the export's placement: %v", head["placement"])
	}
	if pc := h.cloud.prechecks[0]; pc["datapack_id"] != d.ID || pc["memories"] == nil {
		t.Errorf("the precheck sent %v", pc)
	}
}

// A datapack that doesn't fit the plan isn't synced: nothing is imported, and it stays, taking writes.
func TestSyncThatDoesntFit(t *testing.T) {
	h := setup(t)
	h.cloud.fits = false
	run, err := h.s.Start(context.Background(), h.e.DatapackID)
	if err != nil || run.State != cloudsync.StateFailed || run.Precheck == nil || run.Precheck.OK {
		t.Fatalf("start: %+v %v", run, err)
	}
	if !h.exists(t, h.e.DatapackID) || h.cloud.started["imp-1"] {
		t.Error("a datapack that doesn't fit was synced")
	}
	if _, err = h.e.Service.PutArtifact(context.Background(), h.e.DatapackID, "a", "n.txt", "text/plain",
		[]byte("x")); err != nil {
		t.Errorf("writes after a refused precheck: %v", err)
	}
}

// A failed upload cancels the import and leaves the datapack as it was; so does a failed import.
func TestSyncThatFails(t *testing.T) {
	h := setup(t)
	h.fail = errors.New("network down")
	if _, err := h.s.Start(context.Background(), h.e.DatapackID); err != nil {
		t.Fatal(err)
	}
	if run := h.wait(t, h.e.DatapackID); run.State != cloudsync.StateFailed ||
		!strings.Contains(run.Error, "network down") {
		t.Errorf("a failed upload: %+v", run)
	}
	if !h.exists(t, h.e.DatapackID) || len(h.cloud.canceled) != 1 {
		t.Errorf("after a failed upload: here %v, canceled %v", h.exists(t, h.e.DatapackID), h.cloud.canceled)
	}

	h.fail, h.cloud.result = nil, "failed"
	if _, err := h.s.Start(context.Background(), h.e.DatapackID); err != nil {
		t.Fatal(err)
	}
	if run := h.wait(t, h.e.DatapackID); run.State != cloudsync.StateFailed || !h.exists(t, h.e.DatapackID) {
		t.Errorf("a failed import: %+v", run)
	}
	if _, err := h.e.Service.PutArtifact(context.Background(), h.e.DatapackID, "a", "n.txt", "text/plain",
		[]byte("x")); err != nil {
		t.Errorf("writes after a failed sync: %v", err)
	}
}

// A Meko Cloud without the import routes says so before anything starts.
func TestSyncUnsupported(t *testing.T) {
	h := setup(t)
	h.cloud.missing = true
	if _, err := h.s.Start(context.Background(), h.e.DatapackID); !errors.Is(err, cloudsync.ErrUnsupported) {
		t.Errorf("start: %v", err)
	}
}

// A sync a stop interrupted after Meko Cloud took the import is finished at the next start.
func TestResume(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	if err := h.e.Service.SetSyncJob(ctx, h.e.DatapackID, "imp-1"); err != nil {
		t.Fatal(err)
	}
	h.s.Resume(ctx)
	if run := h.wait(t, h.e.DatapackID); run.State != cloudsync.StateDone || h.exists(t, h.e.DatapackID) {
		t.Errorf("resumed: %+v", run)
	}
}
