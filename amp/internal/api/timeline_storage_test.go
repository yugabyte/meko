package api_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/yb-light/amp/internal/api"
	"github.com/yugabyte/yb-light/amp/internal/core"
	"github.com/yugabyte/yb-light/amp/internal/storagecontroller"
)

// fakeSCServer is an httptest stand-in for the managed storage controller's
// pending-init endpoints. It records the calls the timeline handlers make
// through a real storagecontroller.Client, so the create-timeline recipe path
// is exercised end-to-end without a real controller. failRecord, when set,
// makes the record (POST) return 500 so the handler's error path can be tested.
type fakeSCServer struct {
	mu               sync.Mutex
	recordHit        bool
	recordedTenant   string
	recordedTimeline string
	recordedReq      storagecontroller.PendingTimelineInit
	deleteHit        bool
	failRecord       bool
}

// start stands up the httptest server and returns a storagecontroller.Client
// pointed at it. The path is /control/v1/tenant/{tenant}/timeline/{timeline}/pending_init.
func (f *fakeSCServer) start(t *testing.T) *storagecontroller.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/control/v1/tenant/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/pending_init") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// /control/v1/tenant/{tenant}/timeline/{timeline}/pending_init
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/control/v1/tenant/"), "/")
		tenant, timeline := parts[0], parts[2]
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			if f.failRecord {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			f.recordHit = true
			f.recordedTenant = tenant
			f.recordedTimeline = timeline
			_ = json.NewDecoder(r.Body).Decode(&f.recordedReq)
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			f.deleteHit = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return storagecontroller.NewClient(ts.Listener.Addr().String())
}

func newTimelineControllers(t *testing.T, sc *storagecontroller.Client) (*api.Controllers, *core.MemBackend) {
	t.Helper()
	backend := core.NewMemBackend()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return api.NewControllers(backend, nil, api.Provisioning{SC: sc}, logger), backend
}

func seedUserPool(t *testing.T, backend *core.MemBackend) uuid.UUID {
	t.Helper()
	u, err := backend.CreateUser(context.Background(), false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	p, err := backend.CreateStoragePool(context.Background(), u.ID, "pool")
	if err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	return p.ID
}

// TestCreateTimelineRecordsPendingInit: with a storage controller wired,
// create-timeline records the pending-init recipe (root timeline: no ancestor,
// start LSN 0/0, pg_version 17) keyed by the 32-hex ids, and does NOT delete it
// (deletion is deferred to the create-endpoint path).
func TestCreateTimelineRecordsPendingInit(t *testing.T) {
	sc := &fakeSCServer{}
	c, backend := newTimelineControllers(t, sc.start(t))
	poolID := seedUserPool(t, backend)

	resp, err := c.CreateTimeline(context.Background(), api.CreateTimelineRequestObject{
		PoolId: poolID,
		Body:   &api.CreateTimelineJSONRequestBody{Name: "main"},
	})
	if err != nil {
		t.Fatalf("CreateTimeline: %v", err)
	}
	created, ok := resp.(api.CreateTimeline201JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want CreateTimeline201JSONResponse", resp)
	}

	if !sc.recordHit {
		t.Fatal("RecordPendingTimelineInit was not called")
	}
	if want := hex.EncodeToString(created.TenantId[:]); sc.recordedTenant != want {
		t.Errorf("recorded tenant = %q, want %q", sc.recordedTenant, want)
	}
	if want := hex.EncodeToString(created.TimelineId[:]); sc.recordedTimeline != want {
		t.Errorf("recorded timeline = %q, want %q", sc.recordedTimeline, want)
	}
	if sc.recordedReq.BranchName != "main" {
		t.Errorf("branch_name = %q, want main", sc.recordedReq.BranchName)
	}
	if sc.recordedReq.PgVersion != 17 {
		t.Errorf("pg_version = %d, want 17", sc.recordedReq.PgVersion)
	}
	if sc.recordedReq.AncestorTimelineID != nil {
		t.Errorf("ancestor_timeline_id = %v, want nil (root timeline)", *sc.recordedReq.AncestorTimelineID)
	}
	if sc.recordedReq.StartLSN != "0/0" {
		t.Errorf("start_lsn = %q, want 0/0", sc.recordedReq.StartLSN)
	}
	if sc.deleteHit {
		t.Error("DeletePendingTimelineInit was called during create-timeline; it belongs to create-endpoint")
	}
}

// TestCreateTimelineNoStorageController: without a storage controller, the
// handler records the control-plane row only (no panic, still 201).
func TestCreateTimelineNoStorageController(t *testing.T) {
	c, backend := newTimelineControllers(t, nil)
	poolID := seedUserPool(t, backend)

	resp, err := c.CreateTimeline(context.Background(), api.CreateTimelineRequestObject{
		PoolId: poolID,
		Body:   &api.CreateTimelineJSONRequestBody{Name: "main"},
	})
	if err != nil {
		t.Fatalf("CreateTimeline: %v", err)
	}
	if _, ok := resp.(api.CreateTimeline201JSONResponse); !ok {
		t.Fatalf("response = %T, want CreateTimeline201JSONResponse", resp)
	}
}

// TestCreateTimelineStorageError: a storage-controller failure recording the
// recipe fails the request.
func TestCreateTimelineStorageError(t *testing.T) {
	sc := &fakeSCServer{failRecord: true}
	c, backend := newTimelineControllers(t, sc.start(t))
	poolID := seedUserPool(t, backend)

	_, err := c.CreateTimeline(context.Background(), api.CreateTimelineRequestObject{
		PoolId: poolID,
		Body:   &api.CreateTimelineJSONRequestBody{Name: "main"},
	})
	if err == nil {
		t.Fatal("CreateTimeline: expected error when storage controller fails, got nil")
	}
}
