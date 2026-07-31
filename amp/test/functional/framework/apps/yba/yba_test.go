package yba

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeYBA is a scripted httptest YBA. Handlers register by "METHOD path"; the
// default 404s. It records the ordered requests and the auth header seen.
type fakeYBA struct {
	mu       sync.Mutex
	requests []string
	auth     []string
	handlers map[string]http.HandlerFunc
	srv      *httptest.Server
}

func newFake() *fakeYBA {
	f := &fakeYBA{handlers: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		f.mu.Lock()
		f.requests = append(f.requests, key)
		f.auth = append(f.auth, r.Header.Get(authHeader))
		h := f.handlers[key]
		f.mu.Unlock()
		if h == nil {
			http.Error(w, `{"error":"no handler"}`, http.StatusNotFound)
			return
		}
		h(w, r)
	}))
	return f
}

func (f *fakeYBA) on(key string, h http.HandlerFunc) { f.handlers[key] = h }

func (f *fakeYBA) sawAuth(token string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.auth {
		if a != token {
			return false
		}
	}
	return len(f.auth) > 0
}

func (f *fakeYBA) close() { f.srv.Close() }

func (f *fakeYBA) client() *Client {
	return New(f.srv.URL, "cust1", "tok-abc",
		WithHTTPClient(f.srv.Client()),
		WithPoll(time.Millisecond, 2*time.Millisecond))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// realisticUniverse is a multi-tserver universe-details payload with unknown
// fields sprinkled in, to prove field tolerance.
func realisticUniverse() map[string]any {
	node := func(name, ip string, tserver, master bool, state string, port int) map[string]any {
		return map[string]any{
			"nodeName":          name,
			"cloudInfo":         map[string]any{"private_ip": ip, "public_ip": "203.0.113." + name[len(name)-1:], "region": "us-west-2", "az": "us-west-2a"},
			"state":             state,
			"isTserver":         tserver,
			"isMaster":          master,
			"ysqlServerRpcPort": port,
			"unknownNodeField":  "ignored",
		}
	}
	return map[string]any{
		"universeUUID":  "u-123",
		"name":          "my-universe",
		"surpriseField": 42,
		"universeDetails": map[string]any{
			"clusters": []any{
				map[string]any{"userIntent": map[string]any{"ybSoftwareVersion": "2025.1.0.0-b123", "universeName": "my-universe"}},
			},
			"nodeDetailsSet": []any{
				node("n-1", "10.0.0.1", true, true, "Live", 5433),
				node("n-2", "10.0.0.2", true, false, "Live", 5433),
				node("n-3", "10.0.0.3", true, false, "Live", 0),       // 0 → default 5433
				node("n-4", "10.0.0.4", false, true, "Live", 0),       // master-only, skipped
				node("n-5", "10.0.0.5", true, false, "Removed", 5433), // not live, skipped
			},
		},
	}
}

func TestListAndGetUniverse(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/universes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{realisticUniverse()})
	})

	byName, err := f.client().GetUniverse(context.Background(), "my-universe")
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	if byName.UniverseUUID != "u-123" {
		t.Errorf("uuid = %q, want u-123", byName.UniverseUUID)
	}
	byID, err := f.client().GetUniverse(context.Background(), "u-123")
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if byID.Name != "my-universe" {
		t.Errorf("name = %q, want my-universe", byID.Name)
	}
	if v := byID.Version(); v != "2025.1.0.0-b123" {
		t.Errorf("version = %q, want 2025.1.0.0-b123", v)
	}
	if !f.sawAuth("tok-abc") {
		t.Errorf("auth header not asserted on every request")
	}
}

func TestGetUniverseNotFound(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/universes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{})
	})
	if _, err := f.client().GetUniverse(context.Background(), "nope"); err == nil {
		t.Fatalf("expected not-found error")
	}
}

func TestYSQLConnInfoExtraction(t *testing.T) {
	var u Universe
	b, _ := json.Marshal(realisticUniverse())
	if err := json.Unmarshal(b, &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ci, addrs, err := YSQLConnInfo(u)
	if err != nil {
		t.Fatalf("conn info: %v", err)
	}
	if ci.Host != "10.0.0.1" || ci.Port != 5433 {
		t.Errorf("primary = %s:%d, want 10.0.0.1:5433", ci.Host, ci.Port)
	}
	if ci.User != DefaultYSQLUser || ci.Database != DefaultYSQLDatabase {
		t.Errorf("user/db = %s/%s, want %s/%s", ci.User, ci.Database, DefaultYSQLUser, DefaultYSQLDatabase)
	}
	// Three live tservers (n-1, n-2, n-3); master-only n-4 and removed n-5 excluded.
	want := []string{"10.0.0.1:5433", "10.0.0.2:5433", "10.0.0.3:5433"}
	if len(addrs) != len(want) {
		t.Fatalf("addrs = %v, want %v", addrs, want)
	}
	for i, a := range want {
		if addrs[i] != a {
			t.Errorf("addrs[%d] = %q, want %q", i, addrs[i], a)
		}
	}
}

func TestYSQLConnInfoNoLiveTserver(t *testing.T) {
	u := Universe{Name: "empty", UniverseDetails: UniverseDetails{
		NodeDetailsSet: []NodeDetails{{NodeName: "m", IsMaster: true, CloudInfo: CloudInfo{PrivateIP: "10.0.0.9"}}},
	}}
	if _, _, err := YSQLConnInfo(u); err == nil {
		t.Fatalf("expected error for no live tserver")
	}
}

func TestCreateUniverseAndWaitSuccess(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/customers/cust1/universes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"taskUUID": "task-1", "resourceUUID": "u-new"})
	})
	var polls int
	f.on("GET /api/v1/customers/cust1/tasks/task-1", func(w http.ResponseWriter, _ *http.Request) {
		polls++
		status := TaskRunning
		if polls >= 2 {
			status = TaskSuccess
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": status, "percent": 100, "title": "Create Universe", "extra": 1})
	})
	ref, err := f.client().CreateUniverseAndWait(context.Background(), map[string]any{"name": "u-new"})
	if err != nil {
		t.Fatalf("create and wait: %v", err)
	}
	if ref.ResourceUUID != "u-new" || ref.TaskUUID != "task-1" {
		t.Errorf("ref = %+v", ref)
	}
	if polls < 2 {
		t.Errorf("expected at least 2 polls, got %d", polls)
	}
}

func TestCreateUniverseTaskFailureSurfaced(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/customers/cust1/universes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"taskUUID": "task-x"})
	})
	f.on("GET /api/v1/customers/cust1/tasks/task-x", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": TaskFailure, "title": "Create Universe"})
	})
	f.on("GET /api/v1/customers/cust1/tasks/task-x/failed", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"failedSubTasks": []any{
			map[string]any{"subTaskGroupType": "Provisioning", "errorString": "no capacity for m7g.xlarge in us-west-2a"},
		}})
	})
	_, err := f.client().CreateUniverseAndWait(context.Background(), map[string]any{"name": "boom"})
	if err == nil {
		t.Fatalf("expected task failure error")
	}
	// The failed subtask's errorString must be surfaced, not just the task title.
	if !strings.Contains(err.Error(), "no capacity for m7g.xlarge") || !strings.Contains(err.Error(), "Provisioning") {
		t.Errorf("error missing subtask detail: %v", err)
	}
}

func TestDeleteUniverseAndWait(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("DELETE /api/v1/customers/cust1/universes/u-123", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"taskUUID": "task-del"})
	})
	f.on("GET /api/v1/customers/cust1/tasks/task-del", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": TaskSuccess})
	})
	if _, err := f.client().DeleteUniverseAndWait(context.Background(), "u-123"); err != nil {
		t.Fatalf("delete and wait: %v", err)
	}
}

func TestForceDeleteUniverseSetsForceQuery(t *testing.T) {
	f := newFake()
	defer f.close()
	var rawQuery string
	f.on("DELETE /api/v1/customers/cust1/universes/u-123", func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		writeJSON(w, http.StatusOK, map[string]any{"taskUUID": "task-del"})
	})
	f.on("GET /api/v1/customers/cust1/tasks/task-del", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": TaskSuccess})
	})
	if _, err := f.client().ForceDeleteUniverseAndWait(context.Background(), "u-123"); err != nil {
		t.Fatalf("force delete and wait: %v", err)
	}
	// isForceDelete=true terminates nodes of a FAILED/locked universe (a plain
	// delete no-ops on those, stranding instances).
	if !strings.Contains(rawQuery, "isForceDelete=true") {
		t.Errorf("delete query = %q, want isForceDelete=true", rawQuery)
	}
}

func TestSetYbcEnabledForProvider(t *testing.T) {
	f := newFake()
	defer f.close()
	var body, contentType string
	f.on("PUT /api/v1/customers/cust1/runtime_config/prov-1/key/ybc.provider.enabled", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		contentType = r.Header.Get("Content-Type")
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
	})
	if err := f.client().SetYbcEnabledForProvider(context.Background(), "prov-1", false); err != nil {
		t.Fatalf("set ybc: %v", err)
	}
	// setKey requires the raw value as text/plain (not JSON-quoted).
	if body != "false" {
		t.Errorf("body = %q, want false", body)
	}
	if contentType != "text/plain" {
		t.Errorf("content-type = %q, want text/plain", contentType)
	}
}

func TestAPIErrorSurfaced(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/universes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "bad token"})
	})
	_, err := f.client().ListUniverses(context.Background())
	if err == nil {
		t.Fatalf("expected API error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusForbidden || apiErr.Message != "bad token" {
		t.Errorf("apiErr = %+v", apiErr)
	}
}

func TestFindProvider(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/providers", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{
			map[string]any{"uuid": "p-1", "code": "aws", "name": "aws-west"},
			map[string]any{"uuid": "p-2", "code": "gcp", "name": "gcp-central"},
		})
	})
	p, err := f.client().FindProvider(context.Background(), "gcp-central")
	if err != nil {
		t.Fatalf("find provider: %v", err)
	}
	if p.UUID != "p-2" || p.Code != "gcp" {
		t.Errorf("provider = %+v", p)
	}
	if _, err := f.client().FindProvider(context.Background(), "azure-x"); err == nil {
		t.Fatalf("expected not-found for missing provider")
	}
}

func TestUniverseHealth(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/universes/u-123/health_check", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{
			map[string]any{"node_name": "n-1", "node_data": map[string]any{"has_error": false}},
		})
	})
	hc, err := f.client().UniverseHealth(context.Background(), "u-123")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if len(hc) != 1 || hc[0].NodeName != "n-1" {
		t.Errorf("health = %+v", hc)
	}
}
