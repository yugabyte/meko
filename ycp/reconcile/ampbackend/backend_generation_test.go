package ampbackend

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"meko/ycp/internal/ampclient"
	"meko/ycp/internal/api"
	"meko/ycp/reconcile"
)

// These tests cover the half of meko#985 the controller cannot: how the agent
// reads the echoed generation, how it distinguishes an update still in flight
// from one whose task failed, and that it never treats a 202 as proof of work.

const (
	genPoolID     = "22222222-2222-2222-2222-222222222222"
	genClusterID  = "33333333-3333-3333-3333-333333333333"
	genBranchID   = "44444444-4444-4444-4444-444444444444"
	genEndpointID = "55555555-5555-5555-5555-555555555555"
	genTaskID     = "77777777-7777-7777-7777-777777777777"

	genBranchPath   = "pool-a/recproj/main"
	genEndpointPath = "pool-a/recproj/main/rw"
)

// genServer is a stub controller for the update path: it lists one
// pool/cluster/branch/endpoint tree with a scriptable echoed generation, hands
// back a Task from every mutating call, and answers GET /tasks/{id} with a
// scriptable status.
type genServer struct {
	// echoed ycp_generation on the branch and endpoint rows; nil renders `null`.
	branchGen   *int64
	endpointGen *int64
	// taskStatus / taskError script GET /tasks/{id}. An empty taskStatus makes the
	// server 404 the task, i.e. "the controller no longer knows it".
	taskStatus    string
	taskError     string
	taskErrorCode string
	// taskStatusOf overrides taskStatus for one task id, for a test that needs a
	// second task in a different state.
	taskStatusOf map[string]string
	// noEndpoints lists the branch with no endpoints, as after a delete.
	noEndpoints bool
	// ctlImageTag is the tag the controller's pod template carries; empty takes it
	// from whatever helm last deployed. controllerVersion is what GET /version
	// reports — helm starts at an empty release, so a declared version always
	// diverges unless a test says otherwise.
	ctlImageTag       string
	controllerVersion string
	helm              *fakeHelm
	// mutateStatus overrides the status code the two mutating calls return;
	// mutateError is the error message sent with a non-2xx.
	mutateStatus int
	mutateError  string
	// endpointConnectivity is spliced into the endpoint row as the raw JSON value
	// of its `connectivity` property. Empty renders no property at all, i.e. an
	// endpoint from before connectivity was configurable.
	endpointConnectivity string
	// endpointSettings / branchSettings are spliced into their row as raw JSON
	// properties, e.g. `,"pg_settings":{"work_mem":"64MB"}`. Empty renders none,
	// which is how the controller reports a surface that is empty or never set —
	// the two are indistinguishable on the wire, deliberately.
	endpointSettings string
	branchSettings   string
	// endpointDocumentdb is the endpoint row's documentdb_address; empty renders
	// none. caStatus / caBody script GET …/endpoints/{id}/ca, defaulting to 200
	// and a bundle that needs no CA; caGets counts those reads.
	endpointDocumentdb string
	caStatus           int
	caBody             string
	caGets             int
	// settingsStatus overrides the status code the two settings PATCHes return;
	// settingsError is the body sent with a non-2xx.
	settingsStatus int
	settingsError  string

	// fleetVersion / fleetPin are GET /fleet-version's version and target_version;
	// fleetRoll, when set, is the status of the roll it reports in flight, and
	// fleetRollDone / fleetRollTotal that roll's children.
	fleetVersion   string
	fleetPin       string
	fleetRoll      string
	fleetRollDone  int
	fleetRollTotal int
	// fleetSubtasksFail makes GET /tasks/{id}/subtasks answer 500.
	fleetSubtasksFail bool
	// fleetMissing makes GET /fleet-version 404, as a controller predating it does.
	fleetMissing bool

	// registered is the catalog's registered (non-builtin) half, served beside
	// builtinCatalog; extPuts records every PUT /extensions/{name} body.
	registered []ampclient.Extension
	extPuts    []ampclient.RegisterExtensionRequest

	fleetRolls   int // POST /endpoint-upgrades calls
	resizes      []ampclient.ResizeEndpointRequest
	connUpdates  []ampclient.UpdateEndpointConnectivityRequest
	schedUpdates []ampclient.UpdateEndpointSchedulingRequest
	// mutations names each scheduling and resize call, in arrival order.
	mutations []string
	extAdds   []ampclient.AddBranchExtensionsRequest
	epPatches []ampclient.UpdateEndpointSettingsRequest
	brPatches []ampclient.UpdateBranchSettingsRequest
	taskGets  int
	epCreates int // POST …/endpoints calls
	clCreates int // POST …/clusters calls
}

func (g *genServer) start(t *testing.T) *Backend {
	t.Helper()
	if g.mutateStatus == 0 {
		g.mutateStatus = http.StatusAccepted
	}
	if g.settingsStatus == 0 {
		g.settingsStatus = http.StatusOK
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveCatalog(w, r, g.registered...) {
			return
		}
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case p == "/users":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":"`+ycpAdminUserID.String()+`","is_admin":true,"created_at":"2026-01-01T00:00:00Z"}`)
		case strings.HasPrefix(p, "/extensions/") && r.Method == http.MethodPut:
			var req ampclient.RegisterExtensionRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode register-extension body: %v", err)
			}
			if got := r.URL.Query().Get("user_id"); got != ycpAdminUserID.String() {
				t.Errorf("register-extension user_id = %q, want the agent's admin %s", got, ycpAdminUserID)
			}
			g.extPuts = append(g.extPuts, req)
			io.WriteString(w, `{"name":"`+strings.TrimPrefix(p, "/extensions/")+`","image":"`+req.Image+`","builtin":false,"preload":false}`)
		case p == "/version":
			io.WriteString(w, `{"version":"`+g.controllerVersion+`","git_commit":"abc1234","build_date":"2026-01-01T00:00:00Z"}`)
		case p == "/fleet-version":
			if g.fleetMissing {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"error":{"code":"not_found","message":"no such route"}}`)
				return
			}
			body := map[string]any{"version": g.fleetVersion, "target_version": g.fleetPin}
			if g.fleetRoll != "" {
				body["roll"] = map[string]any{"id": genTaskID, "kind": "upgrade_endpoints", "status": g.fleetRoll,
					"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"}
			}
			_ = json.NewEncoder(w).Encode(body)
		case p == "/endpoint-upgrades":
			g.fleetRolls++
			g.writeMutateResponse(w)
		case strings.HasSuffix(p, "/subtasks"):
			if g.fleetSubtasksFail {
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, `{"error":{"code":"internal","message":"store unavailable"}}`)
				return
			}
			subs := []map[string]any{}
			for i := 0; i < g.fleetRollTotal; i++ {
				status := "pending"
				if i < g.fleetRollDone {
					status = "succeeded"
				}
				subs = append(subs, map[string]any{"id": genTaskID, "task_id": genTaskID, "kind": "upgrade_endpoint",
					"status": status, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"count": len(subs), "subtasks": subs})
		case strings.HasPrefix(p, "/tasks/"):
			g.taskGets++
			if g.taskStatus == "" {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"error":{"code":"not_found","message":"unknown task"}}`)
				return
			}
			status := g.taskStatus
			if s, ok := g.taskStatusOf[strings.TrimPrefix(p, "/tasks/")]; ok {
				status = s
			}
			body := map[string]any{"id": genTaskID, "kind": "resize_endpoint", "status": status,
				"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"}
			if g.taskError != "" {
				body["error"] = g.taskError
			}
			if g.taskErrorCode != "" {
				body["error_code"] = g.taskErrorCode
			}
			_ = json.NewEncoder(w).Encode(body)
		case strings.HasSuffix(p, "/ca"):
			g.caGets++
			status, body := g.caStatus, g.caBody
			if status == 0 {
				status = http.StatusOK
			}
			if body == "" {
				body = `{"required":false,"verify_name":"ep.host"}`
			}
			w.WriteHeader(status)
			io.WriteString(w, body)
		case strings.HasSuffix(p, "/connectivity"):
			var req ampclient.UpdateEndpointConnectivityRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode connectivity body: %v", err)
			}
			g.connUpdates = append(g.connUpdates, req)
			g.writeMutateResponse(w)
		case strings.HasSuffix(p, "/scheduling"):
			var req ampclient.UpdateEndpointSchedulingRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode scheduling body: %v", err)
			}
			g.schedUpdates = append(g.schedUpdates, req)
			g.mutations = append(g.mutations, "scheduling")
			g.writeMutateResponse(w)
		case strings.HasSuffix(p, "/resize"):
			var req ampclient.ResizeEndpointRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode resize body: %v", err)
			}
			g.resizes = append(g.resizes, req)
			g.mutations = append(g.mutations, "resize")
			g.writeMutateResponse(w)
		case strings.HasSuffix(p, "/settings") && strings.Contains(p, "/endpoints/"):
			var req ampclient.UpdateEndpointSettingsRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode endpoint settings body: %v", err)
			}
			g.epPatches = append(g.epPatches, req)
			g.writeSettingsResponse(w)
		case strings.HasSuffix(p, "/settings"):
			var req ampclient.UpdateBranchSettingsRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode branch settings body: %v", err)
			}
			g.brPatches = append(g.brPatches, req)
			g.writeSettingsResponse(w)
		case strings.HasSuffix(p, "/extensions"):
			var req ampclient.AddBranchExtensionsRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode add-extensions body: %v", err)
			}
			g.extAdds = append(g.extAdds, req)
			g.writeMutateResponse(w)
		case strings.HasSuffix(p, "/endpoints") && r.Method == http.MethodPost:
			g.epCreates++
			g.writeMutateResponse(w)
		case strings.HasSuffix(p, "/endpoints") && g.noEndpoints:
			io.WriteString(w, `{"endpoints":[]}`)
		case strings.HasSuffix(p, "/clusters") && r.Method == http.MethodPost:
			g.clCreates++
			g.writeMutateResponse(w)
		case strings.HasSuffix(p, "/endpoints"):
			conn := ""
			if g.endpointConnectivity != "" {
				conn = `,"connectivity":` + g.endpointConnectivity
			}
			if g.endpointDocumentdb != "" {
				conn += `,"documentdb_address":"` + g.endpointDocumentdb + `"`
			}
			io.WriteString(w, `{"endpoints":[{"id":"`+genEndpointID+`","name":"rw","branch_id":"`+genBranchID+
				`","cluster_id":"`+genClusterID+`","role":"read_write","address":"ep.host:5433",`+
				`"resources":{"cpu":"2","memory":"4Gi"},"ycp_generation":`+jsonGen(g.endpointGen)+conn+
				g.endpointSettings+`,"created_at":"2026-01-01T00:00:00Z"}]}`)
		case strings.HasSuffix(p, "/branches"):
			io.WriteString(w, `{"branches":[{"branch_id":"`+genBranchID+`","cluster_id":"`+genClusterID+
				`","name":"main","is_default":true,"pg_version":18,"extensions":["pgvector"],`+
				`"ycp_generation":`+jsonGen(g.branchGen)+g.branchSettings+
				`,"created_at":"2026-01-01T00:00:00Z"}]}`)
		case strings.HasSuffix(p, "/clusters"):
			io.WriteString(w, `{"clusters":[{"id":"`+genClusterID+`","name":"recproj","storage_pool_id":"`+genPoolID+
				`","created_at":"2026-01-01T00:00:00Z","default_branch":{"branch_id":"`+genBranchID+
				`","cluster_id":"`+genClusterID+`","name":"main","is_default":true,"pg_version":18,`+
				`"created_at":"2026-01-01T00:00:00Z"}}]}`)
		case strings.HasSuffix(p, "/storage-pools"):
			io.WriteString(w, `{"storage_pools":[{"id":"`+genPoolID+
				`","name":"pool-a","owner_user_id":"11111111-1111-1111-1111-111111111111",`+
				`"created_at":"2026-01-01T00:00:00Z"}]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	if g.helm == nil {
		g.helm = &fakeHelm{}
	}
	b, err := New(srv.URL, "", nil, ControllerConfig{
		Helm: g.helm,
		Rollout: func(context.Context, string) (bool, string, error) {
			tag := g.ctlImageTag
			if tag == "" {
				tag = g.helm.deployedTag()
			}
			return true, tag, nil
		},
		Release:    ctlRelease,
		Namespace:  ctlNamespace,
		Chart:      ctlChart,
		Deployment: "amp-controller",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}
	return b
}

// writeMutateResponse answers a mutating call with the scripted status, carrying a
// Task on a 2xx so the agent has something to follow.
func (g *genServer) writeMutateResponse(w http.ResponseWriter) {
	w.WriteHeader(g.mutateStatus)
	if g.mutateStatus < 300 {
		io.WriteString(w, `{"id":"`+genTaskID+`","kind":"resize_endpoint","status":"pending",`+
			`"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`)
		return
	}
	msg, code := g.mutateError, "conflict"
	if g.mutateStatus == http.StatusBadRequest {
		code = "invalid_argument"
	}
	if msg == "" {
		msg = "a task is already in progress"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": msg},
	})
}

// writeSettingsResponse answers a settings PATCH. Unlike the mutating calls it is
// synchronous: a 200 carrying SettingsUpdateResult, never a Task.
func (g *genServer) writeSettingsResponse(w http.ResponseWriter) {
	w.WriteHeader(g.settingsStatus)
	if g.settingsStatus < 300 {
		io.WriteString(w, `{"applied":true}`)
		return
	}
	if g.settingsError == "" {
		g.settingsError = "the controller refused these settings"
	}
	io.WriteString(w, `{"error":{"code":"invalid_argument","message":"`+g.settingsError+`"}}`)
}

func jsonGen(gen *int64) string {
	if gen == nil {
		return "null"
	}
	b, _ := json.Marshal(*gen)
	return string(b)
}

func gen64(v int64) *int64 { return &v }

// mustIssue asserts an Update issued its call. A successful issue reports Pending,
// not nil: a 202 means the controller planned a task, so the update is in flight,
// and calling that "applied" is the claim meko#985 is about.
func mustIssue(t *testing.T, err error) {
	t.Helper()
	var pending *reconcile.PendingError
	if !errors.As(err, &pending) {
		t.Fatalf("Update = %v (%T), want a PendingError reporting the task it planned", err, err)
	}
}

func resizeRes(gen int64) api.DesiredResource {
	return api.DesiredResource{Kind: KindEndpoint, Name: genEndpointPath, Generation: gen,
		Spec: json.RawMessage(`{"resources":{"cpu":"4","memory":"8Gi"}}`)}
}

func extRes(gen int64) api.DesiredResource {
	return api.DesiredResource{Kind: KindBranch, Name: genBranchPath, Generation: gen,
		Spec: json.RawMessage(`{"extensions":["pgvector","pg_cron"]}`)}
}

// The echo, read off the resource: a branch and an endpoint report the generation
// the controller has fully applied, and pools and clusters report nothing — they
// have no update path, so they keep the engine's cache fallback.
func TestListEchoesAppliedGeneration(t *testing.T) {
	g := &genServer{branchGen: gen64(3), endpointGen: gen64(2)}
	b := g.start(t)

	actuals, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byKind := map[string]reconcile.Actual{}
	for _, a := range actuals {
		byKind[a.Key.Kind] = a
	}

	for kind, want := range map[string]int64{KindBranch: 3, KindEndpoint: 2} {
		if got := byKind[kind].ObservedGen; got != want {
			t.Errorf("%s ObservedGen = %d, want %d", kind, got, want)
		}
	}
	// Also visible in the inventory an operator reads.
	if got := byKind[KindBranch].Attrs["applied_generation"]; got != "3" {
		t.Errorf("branch applied_generation attr = %q, want 3", got)
	}
	if got := byKind[KindEndpoint].Attrs["applied_generation"]; got != "2" {
		t.Errorf("endpoint applied_generation attr = %q, want 2", got)
	}

	// Pools and clusters have no update path, so they echo nothing and rely on the
	// engine's cache alone.
	for _, kind := range []string{KindPool, KindCluster} {
		if got := byKind[kind].ObservedGen; got != 0 {
			t.Errorf("%s ObservedGen = %d, want 0 (no update path, so no echo)", kind, got)
		}
	}
}

// A NULL column is generation 0 — no YCP update has been applied — and reports no
// applied_generation attr, so the inventory does not claim a number for a resource
// nothing has updated.
func TestListEchoesZeroForNeverUpdatedRows(t *testing.T) {
	g := &genServer{} // both ycp_generation columns null
	b := g.start(t)

	actuals, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, a := range actuals {
		if a.Key.Kind != KindBranch && a.Key.Kind != KindEndpoint {
			continue
		}
		if a.ObservedGen != 0 {
			t.Errorf("%s ObservedGen = %d, want 0", a.Key, a.ObservedGen)
		}
		if _, ok := a.Attrs["applied_generation"]; ok {
			t.Errorf("%s reports applied_generation for a null column: %v", a.Key, a.Attrs)
		}
	}
}

// Both update paths must tell the controller which generation they are applying —
// it is the value the task stamps on the row, so an update that omits it can
// never converge.
func TestUpdateSendsAppliedGeneration(t *testing.T) {
	t.Run("endpoint resize", func(t *testing.T) {
		g := &genServer{endpointGen: gen64(1)}
		b := g.start(t)
		mustIssue(t, b.Update(t.Context(), resizeRes(4)))
		if len(g.resizes) != 1 {
			t.Fatalf("resize calls = %d, want 1", len(g.resizes))
		}
		if got := g.resizes[0].YcpGeneration; got == nil || *got != 4 {
			t.Errorf("resize ycp_generation = %v, want 4", got)
		}
	})

	t.Run("branch extensions", func(t *testing.T) {
		g := &genServer{branchGen: gen64(1)}
		b := g.start(t)
		mustIssue(t, b.Update(t.Context(), extRes(4)))
		if len(g.extAdds) != 1 {
			t.Fatalf("add-extensions calls = %d, want 1", len(g.extAdds))
		}
		if got := g.extAdds[0].YcpGeneration; got == nil || *got != 4 {
			t.Errorf("add-extensions ycp_generation = %v, want 4", got)
		}
	})
}

// While the task the last update planned is still running, the next tick must
// report pending: issuing again would plan duplicate work, and reporting failure
// would degrade an environment that is converging fine. This is the case a timer
// cannot tell from a failure.
func TestUpdatePendingWhileTaskRunning(t *testing.T) {
	for _, status := range []string{"pending", "running"} {
		t.Run(status, func(t *testing.T) {
			g := &genServer{endpointGen: gen64(1), taskStatus: status}
			b := g.start(t)

			mustIssue(t, b.Update(t.Context(), resizeRes(4)))
			err := b.Update(t.Context(), resizeRes(4))
			var pending *reconcile.PendingError
			if !errors.As(err, &pending) {
				t.Fatalf("second Update err = %v (%T), want a PendingError", err, err)
			}
			if len(g.resizes) != 1 {
				t.Errorf("resize calls = %d, want 1 — the second tick must not re-issue", len(g.resizes))
			}
			if g.taskGets != 1 {
				t.Errorf("task reads = %d, want 1", g.taskGets)
			}
		})
	}
}

// The failure a 202 could not report. Once the task is terminal-failed the agent
// must surface the controller's own error as an ordinary retryable failure — that
// is what engages the engine's backoff and degrades the environment with a reason,
// where before the tick reported success and latched the generation.
func TestUpdateFailedTaskSurfacesItsError(t *testing.T) {
	g := &genServer{endpointGen: gen64(1), taskStatus: "failed", taskError: "node has insufficient cpu"}
	b := g.start(t)

	mustIssue(t, b.Update(t.Context(), resizeRes(4)))
	err := b.Update(t.Context(), resizeRes(4))
	if err == nil {
		t.Fatal("Update after a failed task = nil, want the task's error")
	}
	if !strings.Contains(err.Error(), "node has insufficient cpu") {
		t.Errorf("err = %v, want it to carry the task's own error", err)
	}
	// Retryable, not permanent: the next attempt may well succeed.
	var perm *reconcile.PermanentError
	var pending *reconcile.PendingError
	if errors.As(err, &perm) || errors.As(err, &pending) {
		t.Errorf("err = %v, want a plain retryable error", err)
	}
	// And the failed task is forgotten, so the tick after the backoff re-issues.
	mustIssue(t, b.Update(t.Context(), resizeRes(4)))
	if len(g.resizes) != 2 {
		t.Errorf("resize calls = %d, want 2 (the failure must not wedge the resource)", len(g.resizes))
	}
}

// A task the controller no longer knows (its log was pruned) leaves nothing to
// wait for. The echoed generation is the authority on whether the work landed, so
// the agent re-issues rather than reporting pending forever.
func TestUpdateForgottenTaskReissues(t *testing.T) {
	g := &genServer{endpointGen: gen64(1)} // taskStatus "" => 404
	b := g.start(t)

	mustIssue(t, b.Update(t.Context(), resizeRes(4)))
	mustIssue(t, b.Update(t.Context(), resizeRes(4)))
	if len(g.resizes) != 2 {
		t.Errorf("resize calls = %d, want 2", len(g.resizes))
	}
}

// On the UPDATE path a 409 is the kind's first-writer-wins lock saying a task is
// already doing this work — pending, not done. Counting it as success is the latch
// this change removes, and it is what an agent restart mid-task hits.
func TestUpdateConflictIsPending(t *testing.T) {
	g := &genServer{endpointGen: gen64(1), mutateStatus: http.StatusConflict}
	b := g.start(t)

	err := b.Update(t.Context(), resizeRes(4))
	var pending *reconcile.PendingError
	if !errors.As(err, &pending) {
		t.Fatalf("Update on 409 = %v (%T), want a PendingError", err, err)
	}
}

// Create keeps the opposite reading of 409 — there it means the resource already
// exists, and presence-based diffing covers it, so treating it as success is
// right. The two paths must not be collapsed.
func TestCreateConflictStaysSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveCatalog(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"error":{"code":"conflict","message":"already exists"}}`)
	}))
	t.Cleanup(srv.Close)

	b, err := New(srv.URL, "", nil, noCtl())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Create(t.Context(), api.DesiredResource{Kind: KindPool, Name: "pool-a", Spec: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("Create on 409 = %v, want success", err)
	}
}

// A create plans a task the 202 cannot report the outcome of, so the agent
// follows it the way it follows an update's: pending while it runs, without a
// second POST, and the task's own error once it fails. A conflict — a second
// read_write endpoint on a branch, which the controller refuses only inside the
// task — is Rejected, so the engine stops re-planning the same doomed create every
// tick (CLOUDGA-37405); any other failure stays retryable.
func TestCreateFollowsItsTask(t *testing.T) {
	rw2 := api.DesiredResource{Kind: KindEndpoint, Name: genBranchPath + "/rw2", Generation: 1,
		Spec: json.RawMessage(`{"role":"read_write"}`)}

	for _, tc := range []struct {
		name, code string
		rejected   bool
	}{
		{"conflict is rejected", "conflict", true},
		{"other failures retry", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &genServer{taskStatus: "running"}
			b := g.start(t)

			mustIssue(t, b.Create(t.Context(), rw2))
			mustIssue(t, b.Create(t.Context(), rw2))
			if g.epCreates != 1 {
				t.Fatalf("create calls = %d, want 1 — a running task must not be re-issued", g.epCreates)
			}

			g.taskStatus, g.taskError, g.taskErrorCode = "failed", "ensure endpoint record: conflict", tc.code
			err := b.Create(t.Context(), rw2)
			if err == nil || !strings.Contains(err.Error(), "ensure endpoint record: conflict") {
				t.Fatalf("Create after a failed task = %v, want the task's own error", err)
			}
			var perm *reconcile.PermanentError
			var pending *reconcile.PendingError
			switch {
			case errors.As(err, &pending):
				t.Fatalf("err = %v, want a failure, not pending", err)
			case tc.rejected && reconcile.PermanentReasonOf(err) != reconcile.ReasonRejected:
				t.Errorf("err = %v, want Rejected", err)
			case !tc.rejected && errors.As(err, &perm):
				t.Errorf("err = %v, want a plain retryable error", err)
			}
			if g.epCreates != 1 {
				t.Errorf("create calls = %d, want 1 — reading the failure must not re-issue", g.epCreates)
			}

			// The failed task is forgotten, so the next attempt — after the backoff,
			// or a re-declaration — issues afresh.
			mustIssue(t, b.Create(t.Context(), rw2))
			if g.epCreates != 2 {
				t.Errorf("create calls = %d, want 2", g.epCreates)
			}
		})
	}
}

func conflictFailed(g *genServer) {
	g.taskStatus, g.taskError, g.taskErrorCode = "failed", "ensure endpoint record: conflict", "conflict"
}

// The swap: one declaration withdraws a branch's writer and adds another, and the
// new one's create fails while the old one's delete is still running. That clears
// by itself, so it must be retried rather than held until the spec changes.
func TestCreateConflictRetriesWhileASiblingIsBeingDeleted(t *testing.T) {
	const deleteTask = "88888888-8888-8888-8888-888888888888"
	g := &genServer{taskStatus: "running", taskStatusOf: map[string]string{deleteTask: "running"}}
	b := g.start(t)
	rw2 := api.DesiredResource{Kind: KindEndpoint, Name: genBranchPath + "/rw2", Spec: json.RawMessage(`{"role":"read_write"}`)}

	mustIssue(t, b.Create(t.Context(), rw2))
	b.deleting[reconcile.ResourceKey{Kind: KindEndpoint, Name: genEndpointPath}] = openapi_types.UUID(uuid.MustParse(deleteTask))
	conflictFailed(g)

	err := b.Create(t.Context(), rw2)
	if err == nil || reconcile.PermanentReasonOf(err) == reconcile.ReasonRejected {
		t.Fatalf("Create = %v, want a retryable failure while a sibling's delete runs", err)
	}
	var perm *reconcile.PermanentError
	if errors.As(err, &perm) {
		t.Fatalf("Create = %v, want retryable, not permanent", err)
	}
}

// A conflict with nothing being deleted is held — but only until something under
// the parent goes. Withdrawing the conflicting writer in a later declaration does
// not touch rw2's generation, so without Reconsider rw2 would stay rejected.
func TestRejectedCreateReconsideredWhenASiblingGoes(t *testing.T) {
	g := &genServer{taskStatus: "running"}
	b := g.start(t)
	rw2 := api.DesiredResource{Kind: KindEndpoint, Name: genBranchPath + "/rw2", Spec: json.RawMessage(`{"role":"read_write"}`)}

	mustIssue(t, b.Create(t.Context(), rw2))
	conflictFailed(g)
	if err := b.Create(t.Context(), rw2); reconcile.PermanentReasonOf(err) != reconcile.ReasonRejected {
		t.Fatalf("Create = %v, want Rejected", err)
	}
	if got := b.Reconsider(); len(got) != 0 {
		t.Fatalf("Reconsider = %v before anything was removed, want none", got)
	}

	g.noEndpoints = true // rw is gone
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}
	want := reconcile.ResourceKey{Kind: KindEndpoint, Name: rw2.Name}
	if got := b.Reconsider(); len(got) != 1 || got[0] != want {
		t.Fatalf("Reconsider = %v, want [%v]", got, want)
	}
	if got := b.Reconsider(); len(got) != 0 {
		t.Errorf("Reconsider = %v on the next tick, want it to report a key once", got)
	}
}

// A cluster has no update path and echoes no generation, so once its create lands
// the engine plans an Update whenever the create carried a generation above 0.
// Update must read the create's task — Pending while it runs, applied once it
// succeeded at this generation — or the cluster is drift forever.
func TestUpdateAfterCreateReadsTheCreatesTask(t *testing.T) {
	g := &genServer{taskStatus: "running"}
	b := g.start(t)
	cl := api.DesiredResource{Kind: KindCluster, Name: "pool-a/recproj2", Generation: 2, Spec: json.RawMessage(`{}`)}

	mustIssue(t, b.Create(t.Context(), cl))
	mustIssue(t, b.Update(t.Context(), cl))

	g.taskStatus = "succeeded"
	if err := b.Update(t.Context(), cl); err != nil {
		t.Fatalf("Update after the create succeeded at this generation = %v, want applied", err)
	}
	if g.clCreates != 1 {
		t.Errorf("create calls = %d, want 1", g.clCreates)
	}
	// Only the once: a later edit is a real update, which a cluster refuses.
	cl.Generation = 3
	var perm *reconcile.PermanentError
	if err := b.Update(t.Context(), cl); !errors.As(err, &perm) || perm.Reason != reconcile.ReasonUnsupported {
		t.Errorf("Update at a later generation = %v, want unsupported", err)
	}
}

// A create's task can succeed between a tick's List and its Create. Issuing it
// again then is no no-op for a cluster or branch — a duplicate name fails inside
// its task with conflict, and that would replace the success Update reads — so
// the agent waits one tick for the listing. Only one: a resource deleted since it
// was created must still be re-created.
func TestCreateWaitsOneTickForTheListingOnceItsTaskSucceeded(t *testing.T) {
	g := &genServer{taskStatus: "running"}
	b := g.start(t)
	cl := api.DesiredResource{Kind: KindCluster, Name: "pool-a/recproj2", Generation: 1, Spec: json.RawMessage(`{}`)}

	mustIssue(t, b.Create(t.Context(), cl))
	g.taskStatus = "succeeded"
	mustIssue(t, b.Create(t.Context(), cl))
	if g.clCreates != 1 {
		t.Fatalf("create calls = %d, want 1 — a succeeded create must not be re-issued at once", g.clCreates)
	}
	mustIssue(t, b.Create(t.Context(), cl))
	if g.clCreates != 2 {
		t.Errorf("create calls = %d, want 2 — still unlisted a tick later, it is re-created", g.clCreates)
	}
}

// A delete whose task has finished frees the conflict at most once. If its
// resource is still listed afterwards (a task the controller forgot reads as
// succeeded), it is not what clears the conflict, and must not keep the create
// retrying on backoff forever.
func TestFinishedDeleteExcusesAConflictOnce(t *testing.T) {
	const deleteTask = "88888888-8888-8888-8888-888888888888"
	g := &genServer{taskStatus: "running", taskStatusOf: map[string]string{deleteTask: "succeeded"}}
	b := g.start(t)
	rw2 := api.DesiredResource{Kind: KindEndpoint, Name: genBranchPath + "/rw2", Spec: json.RawMessage(`{"role":"read_write"}`)}
	b.deleting[reconcile.ResourceKey{Kind: KindEndpoint, Name: genEndpointPath}] = openapi_types.UUID(uuid.MustParse(deleteTask))

	mustIssue(t, b.Create(t.Context(), rw2))
	conflictFailed(g)
	var perm *reconcile.PermanentError
	if err := b.Create(t.Context(), rw2); err == nil || errors.As(err, &perm) {
		t.Fatalf("first conflict = %v, want retryable — the delete may have just landed", err)
	}

	g.taskStatus, g.taskError, g.taskErrorCode = "running", "", ""
	mustIssue(t, b.Create(t.Context(), rw2))
	conflictFailed(g)
	if err := b.Create(t.Context(), rw2); reconcile.PermanentReasonOf(err) != reconcile.ReasonRejected || !errors.As(err, &perm) {
		t.Fatalf("second conflict = %v, want Rejected — the finished delete freed nothing", err)
	}
}

// The enumeration guard. Every kind is either one Update refuses outright, or one
// that both sends a generation and has that generation echoed back by List —
// there is no third option, because a kind that accepts updates without the echo
// is exactly the silent latch of meko#985.
//
// The enumeration guard's own registries. Test-only on purpose: nothing in the
// backend reads either of these, so declaring them beside the code would make
// them look like runtime behaviour.
//
// allKinds is every kind this backend maps, and observableKinds is the subset
// that proves convergence by OBSERVATION rather than by an echoed generation —
// every field of the spec is directly readable on the remote, so an echo would
// be a second record of something already visible. Those kinds report
// GenerationNever rather than 0: a kind that always exists is never Created, and
// at 0 it would sit at effGen 0 against a first declaration's generation 0 and
// never be updated at all.
//
// A kind absent from allKinds, or in none of the three sets, fails the guard
// below rather than going unasserted in either direction.
var (
	allKinds        = []string{KindPool, KindCluster, KindBranch, KindEndpoint, KindController, KindFleetVersion, KindExtension}
	observableKinds = map[string]bool{KindController: true, KindFleetVersion: true, KindExtension: true}
	// synchronousKinds finish their work before Update returns, so a successful
	// issue is nil rather than a PendingError.
	synchronousKinds = map[string]bool{KindExtension: true}
)

// A kind added to updateFuncs without the plumbing fails here rather than in
// production.
func TestUpdatableKindsEchoTheirGeneration(t *testing.T) {
	specs := map[string]api.DesiredResource{
		KindPool:         {Kind: KindPool, Name: "pool-a", Generation: 4, Spec: json.RawMessage(`{}`)},
		KindCluster:      {Kind: KindCluster, Name: "pool-a/recproj", Generation: 4, Spec: json.RawMessage(`{}`)},
		KindBranch:       extRes(4),
		KindEndpoint:     resizeRes(4),
		KindController:   controllerRes(4),
		KindFleetVersion: fleetRes(4),
		KindExtension:    postgisRes(postgisSpec),
	}
	// Every kind this backend maps has to be here, or the guard below silently
	// covers less than it claims — in either direction, since a kind absent from
	// specs is asserted neither updatable nor permanent.
	for _, kind := range allKinds {
		if _, ok := specs[kind]; !ok {
			t.Fatalf("backend maps kind %q but this test has no spec for it — add one", kind)
		}
	}
	for kind := range updateFuncs {
		if _, ok := specs[kind]; !ok {
			t.Fatalf("updateFuncs has %q but this test has no spec for it — add one", kind)
		}
	}

	for kind, res := range specs {
		t.Run(kind, func(t *testing.T) {
			// The fleet is behind a controller that already deploys the target, so the
			// only thing left for the fleet-version kind to do is roll.
			g := &genServer{branchGen: gen64(1), endpointGen: gen64(1),
				fleetVersion: "0.260901.0-abc1234", fleetPin: controllerTargetVersion}
			b := g.start(t)

			err := b.Update(t.Context(), res)
			var perm *reconcile.PermanentError
			if _, updatable := updateFuncs[kind]; !updatable {
				if !errors.As(err, &perm) {
					t.Fatalf("Update on non-updatable %s = %v, want a PermanentError", kind, err)
				}
				return
			}
			if synchronousKinds[kind] {
				if err != nil {
					t.Fatalf("Update on %s = %v, want nil", kind, err)
				}
			} else {
				mustIssue(t, err)
			}

			if observableKinds[kind] {
				// No echo to assert: every field of the spec is directly observable on
				// the remote, so convergence is read back out of List. What has to hold
				// instead is that a diverging declaration acts and a converged one does
				// not — the same guarantee the echo gives, proved the other way.
				assertObservable(t, kind, res, g)
				return
			}

			// It sent a generation...
			var sent *int64
			switch kind {
			case KindEndpoint:
				if len(g.resizes) != 1 {
					t.Fatalf("%s: mutating calls = %d, want 1", kind, len(g.resizes))
				}
				sent = g.resizes[0].YcpGeneration
			case KindBranch:
				if len(g.extAdds) != 1 {
					t.Fatalf("%s: mutating calls = %d, want 1", kind, len(g.extAdds))
				}
				sent = g.extAdds[0].YcpGeneration
			default:
				t.Fatalf("%s is updatable but this test does not know how to read its request body", kind)
			}
			if sent == nil || *sent != res.Generation {
				t.Errorf("%s sent ycp_generation = %v, want %d", kind, sent, res.Generation)
			}

			// ...and List echoes one back for it, so the engine has something durable
			// to compare against instead of falling back to its in-process cache.
			actuals, err := b.List(t.Context())
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			var echoed bool
			for _, a := range actuals {
				if a.Key.Kind == kind {
					echoed = a.ObservedGen == 1 // what the fixture reports
					break
				}
			}
			if !echoed {
				t.Errorf("%s is updatable but List echoes no ObservedGen for it", kind)
			}
		})
	}
}

// The branch latch the echo exists to remove, reached through the one path that
// used to skip the controller entirely.
//
// add_branch_extensions unions the branch row BEFORE it redeploys the branch's
// endpoints, so a tick that lands after the union sees nothing missing. If that
// meant "converged", the engine would cache the generation and Noop the branch
// forever — while the task that unioned the row may still fail redeploying an
// endpoint, leaving ycp_generation unstamped and the endpoints on the old set.
// An agent restart makes it certain: the in-flight task id is gone, so the 409
// cannot save it either.
//
// The answer is a settings PATCH carrying nothing but the generation. It used to
// be a re-send of the whole extension set — the only branch call that could carry
// one — which planned a task on every generation bump of a branch declaring
// extensions. Re-driving the redeploy an unstamped row implies is the task's job
// now: it redeploys exactly the endpoints whose recorded set is behind.
func TestUpdateBranchStampsAnUnrecordedGenerationWithoutWork(t *testing.T) {
	// The row already carries what the spec declares — the union landed — but the
	// generation has not been echoed back.
	g := &genServer{branchGen: nil}
	b := g.start(t)

	if err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindBranch, Name: genBranchPath, Generation: 4,
		Spec: json.RawMessage(`{"extensions":["pgvector"]}`),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if len(g.extAdds) != 0 {
		t.Errorf("add-extensions calls = %d, want 0 — nothing is missing, so nothing is added", len(g.extAdds))
	}
	if len(g.brPatches) != 1 {
		t.Fatalf("branch settings calls = %d, want 1 — nothing missing must not read as converged", len(g.brPatches))
	}
	// Nothing but the generation: every surface omitted means "do not touch", so the
	// controller writes the stamp and tells no endpoint to reload.
	p := g.brPatches[0]
	if p.PgSettings != nil || p.WriteTokenSettings != nil {
		t.Errorf("patched surfaces = %+v, want none — this call exists only to stamp", p)
	}
	if p.YcpGeneration == nil || *p.YcpGeneration != 4 {
		t.Errorf("sent ycp_generation = %v, want 4", p.YcpGeneration)
	}
}

// Once the controller has recorded the generation there is nothing left to do, so
// no call goes out — the re-issue above is bounded by the echo, not unconditional.
func TestUpdateBranchIssuesNothingOnceTheGenerationIsRecorded(t *testing.T) {
	g := &genServer{branchGen: gen64(4)}
	b := g.start(t)

	if err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindBranch, Name: genBranchPath, Generation: 4,
		Spec: json.RawMessage(`{"extensions":["pgvector"]}`),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(g.extAdds) != 0 {
		t.Errorf("add-extensions calls = %d, want 0 once the row confirms the generation", len(g.extAdds))
	}
	if len(g.brPatches) != 0 {
		t.Errorf("branch settings calls = %d, want 0 — nothing to change, nothing to stamp", len(g.brPatches))
	}
}

// A generation that declares something no verb here can apply never fully lands,
// so this update must not claim it: stamping would echo back at the desired
// generation and Noop the resource forever, losing the drift. The add still goes
// out — one unfixable field does not block the rest — just without a generation.
func TestUpdateBranchWithDriftClaimsNoGeneration(t *testing.T) {
	g := &genServer{branchGen: gen64(1)}
	b := g.start(t)

	// parent_branch_id is fixed at the fork and no verb moves it, so declaring one
	// the branch does not carry is drift; the extensions add is real work alongside
	// it. (pg_settings used to stand in here, before the settings verb was wired.)
	err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindBranch, Name: genBranchPath, Generation: 4,
		Spec: json.RawMessage(`{"extensions":["pgvector","pg_cron"],` +
			`"parent_branch_id":"99999999-9999-9999-9999-999999999999"}`),
	})

	var perm *reconcile.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("Update = %v (%T), want the drift reported as permanent", err, err)
	}
	if !strings.Contains(err.Error(), "parent_branch_id") {
		t.Errorf("err = %v, want it to name the field it cannot apply", err)
	}
	if len(g.extAdds) != 1 {
		t.Fatalf("add-extensions calls = %d, want the add still issued", len(g.extAdds))
	}
	if got := g.extAdds[0].YcpGeneration; got != nil {
		t.Errorf("sent ycp_generation = %d, want none — this generation is not fully applied", *got)
	}
}

// connectivity is create-time only in the controller: no update verb carries it,
// so resize drops it and a generation bump that changed it can never converge.
// Claiming the generation anyway would stamp the row, echo back at the desired
// generation and Noop the endpoint forever — converged on a spec that never
// landed, which is the meko#985 failure. Until connectivity became expressible
// an endpoint spec could only say `resources`, which resize applies in full, so
// this is the first field an endpoint can declare and not get.
func TestUpdateEndpointWithConnectivityDriftClaimsNoGeneration(t *testing.T) {
	g := &genServer{endpointGen: gen64(1)} // no connectivity on the row: cluster_ip
	b := g.start(t)

	err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
		Spec: json.RawMessage(`{"resources":{"cpu":"4","memory":"8Gi"},` +
			`"connectivity":{"mode":"nlb_internal"}}`),
	})

	var perm *reconcile.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("Update = %v (%T), want the drift reported as permanent", err, err)
	}
	if !strings.Contains(err.Error(), "connectivity.mode") {
		t.Errorf("err = %v, want it to name the field it cannot apply", err)
	}
	// The resize still goes out — one unfixable field does not block the resources
	// change — but without a generation.
	if len(g.resizes) != 1 {
		t.Fatalf("resize calls = %d, want the resize still issued", len(g.resizes))
	}
	if got := g.resizes[0].YcpGeneration; got != nil {
		t.Errorf("sent ycp_generation = %d, want none — this generation is not fully applied", *got)
	}
}

// A changed allowed_cidrs list goes out as its own task (CLOUDGA-37410). With a
// resize to follow it claims no generation and the resize waits a tick, because
// this backend follows one task per resource and only the last call may stamp.
func TestUpdateEndpointCidrChangeGoesBeforeTheResize(t *testing.T) {
	g := &genServer{
		endpointGen:          gen64(1),
		endpointConnectivity: `{"mode":"nlb_internal","allowed_cidrs":["10.0.0.0/16"]}`,
	}
	b := g.start(t)

	mustIssue(t, b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
		Spec: json.RawMessage(`{"resources":{"cpu":"4","memory":"8Gi"},` +
			`"connectivity":{"mode":"nlb_internal","allowed_cidrs":["10.1.0.0/16","10.0.0.0/16"]}}`),
	}))

	if len(g.connUpdates) != 1 {
		t.Fatalf("connectivity calls = %d, want 1", len(g.connUpdates))
	}
	if got, want := g.connUpdates[0].AllowedCidrs, []string{"10.0.0.0/16", "10.1.0.0/16"}; !equalStrings(got, want) {
		t.Errorf("sent allowed_cidrs = %v, want %v", got, want)
	}
	if got := g.connUpdates[0].YcpGeneration; got != nil {
		t.Errorf("connectivity ycp_generation = %d, want none — the resize is still to come", *got)
	}
	if len(g.resizes) != 0 {
		t.Errorf("resize calls = %d, want none until the connectivity task is done", len(g.resizes))
	}

	// Next tick: the task succeeded and the row reports the new ranges, so the
	// resize goes out and is the call that carries the generation.
	g.taskStatus = "succeeded"
	g.endpointConnectivity = `{"mode":"nlb_internal","allowed_cidrs":["10.0.0.0/16","10.1.0.0/16"]}`
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}
	mustIssue(t, b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
		Spec: json.RawMessage(`{"resources":{"cpu":"4","memory":"8Gi"},` +
			`"connectivity":{"mode":"nlb_internal","allowed_cidrs":["10.1.0.0/16","10.0.0.0/16"]}}`),
	}))
	if len(g.connUpdates) != 1 {
		t.Errorf("connectivity calls = %d, want no second one once the row matches", len(g.connUpdates))
	}
	if len(g.resizes) != 1 || g.resizes[0].YcpGeneration == nil || *g.resizes[0].YcpGeneration != 4 {
		t.Errorf("resizes = %+v, want one carrying generation 4", g.resizes)
	}
}

// With nothing after it, the connectivity task is the last call and carries the
// generation, so the worker stamping it is what converges the endpoint.
func TestUpdateEndpointCidrChangeAloneClaimsTheGeneration(t *testing.T) {
	g := &genServer{
		endpointGen:          gen64(1),
		endpointConnectivity: `{"mode":"nlb_external","allowed_cidrs":["103.200.215.98/32"]}`,
	}
	b := g.start(t)

	mustIssue(t, b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 2,
		Spec: json.RawMessage(`{"connectivity":{"mode":"nlb_external","allowed_cidrs":[]}}`),
	}))

	if len(g.connUpdates) != 1 {
		t.Fatalf("connectivity calls = %d, want 1", len(g.connUpdates))
	}
	req := g.connUpdates[0]
	if req.AllowedCidrs == nil || len(req.AllowedCidrs) != 0 {
		t.Errorf("sent allowed_cidrs = %#v, want [] to remove the restriction", req.AllowedCidrs)
	}
	if req.YcpGeneration == nil || *req.YcpGeneration != 2 {
		t.Errorf("connectivity ycp_generation = %v, want 2", req.YcpGeneration)
	}
	if len(g.epPatches) != 0 {
		t.Errorf("settings PATCHes = %d, want none — the task stamps the generation", len(g.epPatches))
	}
}

// A drifted update is reported once and never re-issued, so it cannot defer the
// resize to a later tick: both go out now, and neither claims a generation.
func TestUpdateEndpointDriftWithCidrsAndResizeIssuesBoth(t *testing.T) {
	g := &genServer{
		endpointGen:          gen64(1),
		endpointConnectivity: `{"mode":"nlb_internal","allowed_cidrs":["10.0.0.0/16"]}`,
	}
	b := g.start(t)

	err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
		Spec: json.RawMessage(`{"resources":{"cpu":"4","memory":"8Gi"},"role":"read_only",` +
			`"connectivity":{"mode":"nlb_internal","allowed_cidrs":["10.1.0.0/16"]}}`),
	})

	var perm *reconcile.PermanentError
	if !errors.As(err, &perm) || !strings.Contains(err.Error(), "role") {
		t.Fatalf("Update = %v, want permanent drift naming role", err)
	}
	if len(g.resizes) != 1 || g.resizes[0].YcpGeneration != nil {
		t.Errorf("resizes = %+v, want one, carrying no generation", g.resizes)
	}
	if len(g.connUpdates) != 1 || g.connUpdates[0].YcpGeneration != nil {
		t.Errorf("connectivity calls = %+v, want one, carrying no generation", g.connUpdates)
	}
}

// The controller trims entries and drops empty ones before storing, so a declared
// list must be compared the same way or it never matches and is re-sent forever.
func TestUpdateEndpointCidrsAreNormalisedLikeTheController(t *testing.T) {
	g := &genServer{
		endpointGen:          gen64(1),
		endpointConnectivity: `{"mode":"nlb_internal","allowed_cidrs":["10.0.0.0/16"]}`,
	}
	b := g.start(t)

	mustIssue(t, b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
		Spec: json.RawMessage(`{"resources":{"cpu":"4","memory":"8Gi"},` +
			`"connectivity":{"mode":"nlb_internal","allowed_cidrs":[" 10.0.0.0/16 ",""]}}`),
	}))
	if len(g.connUpdates) != 0 {
		t.Errorf("connectivity calls = %d, want none — the list matches once normalised", len(g.connUpdates))
	}
	if len(g.resizes) != 1 {
		t.Errorf("resize calls = %d, want the resize issued", len(g.resizes))
	}
}

// The controller judges a list against the mode in force, so with the mode wrong
// the list cannot be sent and both are drift.
func TestUpdateEndpointCidrsWithAModeChangeAreDrift(t *testing.T) {
	g := &genServer{
		endpointGen:          gen64(1),
		endpointConnectivity: `{"mode":"nlb_internal","allowed_cidrs":["10.0.0.0/16"]}`,
	}
	b := g.start(t)

	err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
		Spec: json.RawMessage(`{"resources":{"cpu":"4","memory":"8Gi"},` +
			`"connectivity":{"mode":"nlb_external","allowed_cidrs":["10.1.0.0/16"]}}`),
	})

	var perm *reconcile.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("Update = %v (%T), want the drift reported as permanent", err, err)
	}
	for _, field := range []string{"connectivity.mode", "connectivity.allowed_cidrs"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("err = %v, want it to name %s", err, field)
		}
	}
	if len(g.connUpdates) != 0 {
		t.Errorf("connectivity calls = %d, want none while the mode differs", len(g.connUpdates))
	}
}

// A declared connectivity the endpoint already has is not drift, or every
// endpoint spec that pins its connectivity would latch permanently on the first
// resize. Absent on the row means cluster_ip, so declaring the default matches.
func TestUpdateEndpointWithMatchingConnectivityClaimsTheGeneration(t *testing.T) {
	for name, spec := range map[string]string{
		"default against an absent row": `{"resources":{"cpu":"4","memory":"8Gi"},"connectivity":{"mode":"cluster_ip"}}`,
		"connectivity not declared":     `{"resources":{"cpu":"4","memory":"8Gi"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			g := &genServer{endpointGen: gen64(1)}
			b := g.start(t)

			mustIssue(t, b.Update(t.Context(), api.DesiredResource{
				Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
				Spec: json.RawMessage(spec),
			}))
			if got := g.resizes[0].YcpGeneration; got == nil || *got != 4 {
				t.Errorf("resize ycp_generation = %v, want 4 — nothing here is unappliable", got)
			}
		})
	}
}

// An order-only difference in the CIDR list is the same set, so it must not be
// reported as a permanent latch.
func TestUpdateEndpointIgnoresAllowedCidrOrder(t *testing.T) {
	g := &genServer{
		endpointGen:          gen64(1),
		endpointConnectivity: `{"mode":"nlb_internal","allowed_cidrs":["10.0.0.0/16","10.2.0.0/16"]}`,
	}
	b := g.start(t)

	mustIssue(t, b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
		Spec: json.RawMessage(`{"resources":{"cpu":"4","memory":"8Gi"},` +
			`"connectivity":{"mode":"nlb_internal","allowed_cidrs":["10.2.0.0/16","10.0.0.0/16"]}}`),
	}))
	if got := g.resizes[0].YcpGeneration; got == nil || *got != 4 {
		t.Errorf("resize ycp_generation = %v, want 4 — the set is unchanged", got)
	}
}

// What an endpoint spec can declare and still not get. `role`, `connectivity` and
// `tags` are create-time in the controller — no update verb carries them, so recreating
// the endpoint is the only path. `extended_config` is the one surface
// updateEndpointSettings does NOT take, so it stays here for a reason of its own
// even though its three neighbours converge now.
func TestUpdateEndpointReportsEveryFieldItCannotApply(t *testing.T) {
	for name, tc := range map[string]struct{ declare, want string }{
		"role":            {`"role":"read_only"`, "role"},
		"extended_config": {`"extended_config":{"shared_preload_libraries":"pg_stat_statements"}`, "extended_config"},
		"tags":            {`"tags":{"team":"web"}`, "tags"},
	} {
		t.Run(name, func(t *testing.T) {
			g := &genServer{endpointGen: gen64(1)}
			b := g.start(t)

			err := b.Update(t.Context(), api.DesiredResource{
				Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
				Spec: json.RawMessage(`{"resources":{"cpu":"4","memory":"8Gi"},` + tc.declare + `}`),
			})

			var perm *reconcile.PermanentError
			if !errors.As(err, &perm) {
				t.Fatalf("Update = %v (%T), want the drift reported as permanent", err, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name %s", err, tc.want)
			}
			if got := g.resizes[0].YcpGeneration; got != nil {
				t.Errorf("sent ycp_generation = %d, want none — this generation is not fully applied", *got)
			}
		})
	}
}

// A declared value the endpoint already carries is not drift: the row reports
// role read_write, so pinning it in the spec must still claim the generation.
// Otherwise every spec that names its role would latch on the first resize.
func TestUpdateEndpointMatchingRoleClaimsTheGeneration(t *testing.T) {
	g := &genServer{endpointGen: gen64(1)}
	b := g.start(t)

	mustIssue(t, b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
		Spec: json.RawMessage(`{"resources":{"cpu":"4","memory":"8Gi"},"role":"read_write"}`),
	}))
	if got := g.resizes[0].YcpGeneration; got == nil || *got != 4 {
		t.Errorf("resize ycp_generation = %v, want 4 — the declared role is the one it has", got)
	}
}

// --- endpoint settings -------------------------------------------------------

// The three surfaces converge through updateEndpointSettings, and only the ones
// that differ are named: an omitted field means "do not touch", so re-sending an
// identical surface would push a reload to the compute for a change that is not
// one. The endpoint row here carries none of them.
func TestUpdateEndpointAppliesDeclaredSettings(t *testing.T) {
	g := &genServer{endpointGen: gen64(1)}
	b := g.start(t)

	mustIssue(t, b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
		Spec: json.RawMessage(`{"resources":{"cpu":"4","memory":"8Gi"},` +
			`"pg_settings":{"work_mem":"64MB"},` +
			`"pageserver_settings":{"tenant_config.enable_gc":true}}`),
	}))

	if len(g.epPatches) != 1 {
		t.Fatalf("endpoint settings calls = %d, want 1", len(g.epPatches))
	}
	p := g.epPatches[0]
	if p.PgSettings == nil || (*p.PgSettings)["work_mem"] != "64MB" {
		t.Errorf("patched pg_settings = %v, want work_mem=64MB", p.PgSettings)
	}
	if p.PageserverSettings == nil {
		t.Error("patched pageserver_settings = nil, want the declared map")
	}
	if p.SafekeeperSettings != nil {
		t.Errorf("patched safekeeper_settings = %v, want it absent — undeclared surfaces are untouched", p.SafekeeperSettings)
	}
}

// A surface the row already matches is left out of the body, and with nothing
// else to change the PATCH is not issued at all.
func TestUpdateEndpointSkipsSettingsAlreadyStored(t *testing.T) {
	g := &genServer{endpointGen: gen64(1), endpointSettings: `,"pg_settings":{"work_mem":"64MB"}`}
	b := g.start(t)

	mustIssue(t, b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
		Spec: json.RawMessage(`{"resources":{"cpu":"4","memory":"8Gi"},"pg_settings":{"work_mem":"64MB"}}`),
	}))

	if len(g.epPatches) != 0 {
		t.Errorf("endpoint settings calls = %d, want 0 — the row already carries it", len(g.epPatches))
	}
	if got := g.resizes[0].YcpGeneration; got == nil || *got != 4 {
		t.Errorf("resize ycp_generation = %v, want 4 — the settings are converged", got)
	}
}

// Replace, not merge: the body is the declared map verbatim, so a key the row
// carries and the spec does not is dropped. Desired state says what the surface
// IS, which is also what the PATCH itself means.
func TestUpdateEndpointSettingsReplaceTheWholeSurface(t *testing.T) {
	g := &genServer{endpointGen: gen64(1),
		endpointSettings: `,"pg_settings":{"work_mem":"64MB","shared_buffers":"1GB"}`}
	b := g.start(t)

	mustIssue(t, b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
		Spec: json.RawMessage(`{"resources":{"cpu":"4","memory":"8Gi"},"pg_settings":{"work_mem":"64MB"}}`),
	}))

	if len(g.epPatches) != 1 {
		t.Fatalf("endpoint settings calls = %d, want 1", len(g.epPatches))
	}
	sent := *g.epPatches[0].PgSettings
	if len(sent) != 1 || sent["work_mem"] != "64MB" {
		t.Errorf("patched pg_settings = %v, want exactly the declared map — shared_buffers is pruned", sent)
	}
}

// `{}` clears a surface, and is distinguishable from omitting it. Against a row
// that already reports nothing it is a no-op, because the controller omits an
// empty surface from the GET — cleared and never-set are the same on the wire.
func TestUpdateEndpointSettingsClearAndNoOp(t *testing.T) {
	t.Run("clears a populated surface", func(t *testing.T) {
		g := &genServer{endpointGen: gen64(1), endpointSettings: `,"pg_settings":{"work_mem":"64MB"}`}
		b := g.start(t)

		mustIssue(t, b.Update(t.Context(), api.DesiredResource{
			Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
			Spec: json.RawMessage(`{"resources":{"cpu":"4","memory":"8Gi"},"pg_settings":{}}`),
		}))
		if len(g.epPatches) != 1 {
			t.Fatalf("endpoint settings calls = %d, want 1", len(g.epPatches))
		}
		if sent := g.epPatches[0].PgSettings; sent == nil || len(*sent) != 0 {
			t.Errorf("patched pg_settings = %v, want an empty map", sent)
		}
	})
	t.Run("is a no-op against an empty row", func(t *testing.T) {
		g := &genServer{endpointGen: gen64(1)}
		b := g.start(t)

		mustIssue(t, b.Update(t.Context(), api.DesiredResource{
			Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
			Spec: json.RawMessage(`{"resources":{"cpu":"4","memory":"8Gi"},"pg_settings":{}}`),
		}))
		if len(g.epPatches) != 0 {
			t.Errorf("endpoint settings calls = %d, want 0 — nothing to clear", len(g.epPatches))
		}
	})
}

// A setting the controller refuses is permanent drift, never a retry: the 400
// stored nothing, so every retry sends the same rejected body. The resize still
// goes out — one bad setting does not block a resources change — but without a
// generation, because this spec did not fully land.
//
// The controller has two independent 400 sources here (Postgres refusing a
// pg_settings name, and the daemon key-set check refusing a pageserver key) and
// they reach the agent identically, as the controller's own message.
func TestUpdateEndpointRefusedSettingIsPermanent(t *testing.T) {
	g := &genServer{
		endpointGen:    gen64(1),
		settingsStatus: http.StatusBadRequest,
		settingsError:  "Postgres will not accept: work_memm (unrecognized parameter)",
	}
	b := g.start(t)

	err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
		Spec: json.RawMessage(`{"resources":{"cpu":"4","memory":"8Gi"},"pg_settings":{"work_memm":"64MB"}}`),
	})

	var perm *reconcile.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("Update = %v (%T), want the refusal reported as permanent", err, err)
	}
	if !strings.Contains(err.Error(), "work_memm") {
		t.Errorf("err = %v, want it to carry the controller's own message", err)
	}
	if len(g.resizes) != 1 {
		t.Fatalf("resize calls = %d, want the resize still issued", len(g.resizes))
	}
	if got := g.resizes[0].YcpGeneration; got != nil {
		t.Errorf("resize ycp_generation = %d, want none — the settings never landed", *got)
	}
}

// A transient settings failure is retryable, and must stop the tick before the
// resize: letting the resize go out would plan a task that stamps a generation
// for settings that are not stored.
func TestUpdateEndpointTransientSettingsFailureIssuesNoResize(t *testing.T) {
	g := &genServer{endpointGen: gen64(1), settingsStatus: http.StatusServiceUnavailable}
	b := g.start(t)

	err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
		Spec: json.RawMessage(`{"resources":{"cpu":"4","memory":"8Gi"},"pg_settings":{"work_mem":"64MB"}}`),
	})

	if err == nil {
		t.Fatal("Update = nil, want a retryable error")
	}
	var perm *reconcile.PermanentError
	var pending *reconcile.PendingError
	if errors.As(err, &perm) || errors.As(err, &pending) {
		t.Fatalf("Update = %v (%T), want a plain retryable error", err, err)
	}
	if len(g.resizes) != 0 {
		t.Errorf("resize calls = %d, want 0 — nothing may claim a generation for settings that did not land", len(g.resizes))
	}
}

// A settings-only endpoint spec plans no task at all: the PATCH carries the
// generation itself. The resize used to go out unconditionally just to get one
// stamped, which resized the endpoint to the size it already was.
func TestUpdateEndpointWithOnlySettingsIssuesNoResize(t *testing.T) {
	g := &genServer{endpointGen: gen64(1)}
	b := g.start(t)

	if err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
		Spec: json.RawMessage(`{"pg_settings":{"work_mem":"64MB"}}`),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(g.resizes) != 0 {
		t.Errorf("resize calls = %d, want 0 — nothing declared a reservation", len(g.resizes))
	}
	if len(g.epPatches) != 1 {
		t.Fatalf("endpoint settings calls = %d, want 1", len(g.epPatches))
	}
	if got := g.epPatches[0].YcpGeneration; got == nil || *got != 4 {
		t.Errorf("patched ycp_generation = %v, want 4 — the PATCH is the only call this tick", got)
	}
}

// The endpoint twin of the branch pure stamp: nothing to change, nothing else to
// issue, and the generation unrecorded.
func TestUpdateEndpointStampsAnUnrecordedGenerationWithoutWork(t *testing.T) {
	g := &genServer{endpointGen: nil, endpointSettings: `,"pg_settings":{"work_mem":"64MB"}`}
	b := g.start(t)

	if err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
		Spec: json.RawMessage(`{"pg_settings":{"work_mem":"64MB"}}`),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(g.resizes) != 0 {
		t.Errorf("resize calls = %d, want 0", len(g.resizes))
	}
	if len(g.epPatches) != 1 {
		t.Fatalf("endpoint settings calls = %d, want 1", len(g.epPatches))
	}
	p := g.epPatches[0]
	if p.PgSettings != nil || p.PageserverSettings != nil || p.SafekeeperSettings != nil {
		t.Errorf("patched surfaces = %+v, want none — this call exists only to stamp", p)
	}
	if p.YcpGeneration == nil || *p.YcpGeneration != 4 {
		t.Errorf("sent ycp_generation = %v, want 4", p.YcpGeneration)
	}
}

// An endpoint's pg_settings and its branch's are separate rows, and each PATCH
// carries its own. The agent never merges them: precedence — the endpoint's
// winning on a shared key — is the compute's business, resolved by the
// supervisor's own query (amp/compute/endpoint_supervisor/src/registry.rs) and
// mirrored in core.EffectivePgSettings. An agent that compared against the merged
// view would see a branch-supplied value as the endpoint's own and never write it.
func TestBranchAndEndpointSettingsGoToTheirOwnRows(t *testing.T) {
	g := &genServer{
		branchGen:      gen64(1),
		endpointGen:    gen64(1),
		branchSettings: `,"pg_settings":{"work_mem":"16MB"}`,
	}
	b := g.start(t)

	// The branch says 16MB; the endpoint declares its own 64MB override and its
	// row carries nothing, so both sides have work to do.
	if err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindBranch, Name: genBranchPath, Generation: 4,
		Spec: json.RawMessage(`{"pg_settings":{"work_mem":"32MB"}}`),
	}); err != nil {
		t.Fatalf("Update branch: %v", err)
	}
	if err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
		Spec: json.RawMessage(`{"pg_settings":{"work_mem":"64MB"}}`),
	}); err != nil {
		t.Fatalf("Update endpoint: %v", err)
	}

	if len(g.brPatches) != 1 || (*g.brPatches[0].PgSettings)["work_mem"] != "32MB" {
		t.Errorf("branch patch = %+v, want work_mem=32MB", g.brPatches)
	}
	if len(g.epPatches) != 1 || (*g.epPatches[0].PgSettings)["work_mem"] != "64MB" {
		t.Errorf("endpoint patch = %+v, want work_mem=64MB", g.epPatches)
	}
}

// --- a stamp must never outrun the task that is still doing the work ----------

// The branch shape that made retiring the whole-set re-send a regression.
//
// add_branch_extensions unions the branch row as its FIRST effect, before it
// redeploys a single endpoint, so one tick after the 202 the agent sees nothing
// left to add, nothing to change, and a generation the worker has not stamped —
// because the worker stamps only once every step has succeeded. That is exactly
// the shape the pure stamp fires on, and stamping there claims the desired
// generation for a roll that is still running: the echo then matches, Diff Noops
// the branch forever, and if the roll fails on some endpoint nothing re-drives it
// and nothing reports it. The meko#985 latch, reached through the one door
// applyUpdate does not cover.
//
// The re-send used to cover this by accident, because it went through applyUpdate.
func TestUpdateBranchDoesNotStampOverARunningTask(t *testing.T) {
	g := &genServer{branchGen: nil, taskStatus: "running"}
	b := g.start(t)
	// What the previous tick left behind: a task this agent planned for the branch.
	b.setInflight(reconcile.ResourceKey{Kind: KindBranch, Name: genBranchPath}, uuid.MustParse(genTaskID))

	// The row already reports pgvector — the task unioned it on the way in — so
	// there is nothing to add and nothing to change.
	err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindBranch, Name: genBranchPath, Generation: 4,
		Spec: json.RawMessage(`{"extensions":["pgvector"]}`),
	})

	var pending *reconcile.PendingError
	if !errors.As(err, &pending) {
		t.Fatalf("Update = %v (%T), want Pending while the extensions task is still running", err, err)
	}
	if len(g.brPatches) != 0 {
		t.Errorf("branch settings calls = %d, want 0 — nothing may stamp a generation the roll has not earned", len(g.brPatches))
	}
}

// And the failed case: the task's own error has to surface, not a stamp. Without
// the guard the generation would be claimed for a roll that never finished, and
// the failure would never be reported at all — no task is re-planned, because
// there is nothing left to add.
func TestUpdateBranchSurfacesAFailedTaskInsteadOfStamping(t *testing.T) {
	g := &genServer{branchGen: nil, taskStatus: "failed", taskError: "redeploy endpoint 3 of 5 failed"}
	b := g.start(t)
	b.setInflight(reconcile.ResourceKey{Kind: KindBranch, Name: genBranchPath}, uuid.MustParse(genTaskID))

	err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindBranch, Name: genBranchPath, Generation: 4,
		Spec: json.RawMessage(`{"extensions":["pgvector"]}`),
	})

	if err == nil {
		t.Fatal("Update = nil, want the failed task's error")
	}
	var pending *reconcile.PendingError
	var perm *reconcile.PermanentError
	if errors.As(err, &pending) || errors.As(err, &perm) {
		t.Fatalf("Update = %v (%T), want a plain retryable error carrying the task's failure", err, err)
	}
	if !strings.Contains(err.Error(), "redeploy endpoint 3 of 5 failed") {
		t.Errorf("err = %v, want the controller's own task error", err)
	}
	if len(g.brPatches) != 0 {
		t.Errorf("branch settings calls = %d, want 0", len(g.brPatches))
	}
}

// The endpoint twin. `resizing` is derived from the spec, so a running task cannot
// falsify it — but the operator can, by dropping `resources` from the spec while
// the resize it asked for is still in flight. Then nothing routes through
// applyUpdate and the pure stamp would claim the generation over it.
func TestUpdateEndpointDoesNotStampOverARunningTask(t *testing.T) {
	g := &genServer{endpointGen: nil, taskStatus: "running",
		endpointSettings: `,"pg_settings":{"work_mem":"64MB"}`}
	b := g.start(t)
	b.setInflight(reconcile.ResourceKey{Kind: KindEndpoint, Name: genEndpointPath}, uuid.MustParse(genTaskID))

	// No resources, and the settings already match: nothing to issue.
	err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 4,
		Spec: json.RawMessage(`{"pg_settings":{"work_mem":"64MB"}}`),
	})

	var pending *reconcile.PendingError
	if !errors.As(err, &pending) {
		t.Fatalf("Update = %v (%T), want Pending while the resize task is still running", err, err)
	}
	if len(g.epPatches) != 0 {
		t.Errorf("endpoint settings calls = %d, want 0", len(g.epPatches))
	}
}

// The controller refusing a declaration is not the same thing as a resource that
// cannot be changed, and reporting both as "drift" is CLOUDGA-36992: a console
// resize to 1 vCPU came back as "this resource type cannot be changed in place",
// which is true of a storage pool and false of an endpoint — and it replaced the
// controller's message saying exactly what memory to declare instead.
//
// The refusal here is the real one: resize fills an omitted dimension from the
// endpoint's stored row rather than from the new cpu, so naming cpu alone lands
// under the memory-per-vCPU floor.
func TestControllerRejectionIsNotReportedAsImmutability(t *testing.T) {
	const refusal = "resources.memory 4Gi is below the 8 GiB per vCPU this control plane " +
		"requires: 1 cpu needs at least 8Gi"
	g := &genServer{mutateStatus: http.StatusBadRequest, mutateError: refusal}
	b := g.start(t)

	err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 1,
		Spec: json.RawMessage(`{"role":"read_write","resources":{"cpu":"1"}}`),
	})
	if got := reconcile.PermanentReasonOf(err); got != reconcile.ReasonRejected {
		t.Fatalf("Update = %v (reason %v), want ReasonRejected", err, got)
	}
	if got := reconcile.PermanentReasonOf(err).State(); got != "rejected" {
		t.Errorf("reported state = %q, want %q", got, "rejected")
	}
	// The whole point: the operator is told what to declare instead.
	if !strings.Contains(err.Error(), refusal) {
		t.Errorf("error = %q, does not carry the controller's reason", err)
	}
	// Nothing landed, so nothing may claim the generation.
	if len(g.resizes) != 1 {
		t.Fatalf("resizes = %d, want the one refused call", len(g.resizes))
	}
	if gen := g.resizes[0].YcpGeneration; gen == nil || *gen != 1 {
		t.Errorf("resize carried generation %v, want 1 — the drift is the controller's, not the spec's", gen)
	}
}

// A kind with no update verb at all still reports the immutability verdict, which
// is the case the console's "cannot be changed in place" copy is written for.
func TestUnsupportedVerbStaysImmutabilityDrift(t *testing.T) {
	g := &genServer{}
	b := g.start(t)

	err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindCluster, Name: "pool-a/recproj", Generation: 1,
		Spec: json.RawMessage(`{"pg_version":18}`),
	})
	if got := reconcile.PermanentReasonOf(err); got != reconcile.ReasonUnsupported {
		t.Fatalf("Update = %v (reason %v), want ReasonUnsupported", err, got)
	}
	if got := reconcile.PermanentReasonOf(err).State(); got != "drift" {
		t.Errorf("reported state = %q, want %q", got, "drift")
	}
}

// A spec that mixes the two — a field no verb carries AND a call the controller
// refuses — reports the refusal, because that is the half the operator can act on
// and "some field is immutable" would bury it.
//
// It also pins the resize's own error down. That error used to be discarded
// whenever the spec had any other drift, so a refused resize left no trace at all
// and the row named only the immutable field.
func TestRejectionOutranksUnsupportedDrift(t *testing.T) {
	const refusal = "resources.memory 4Gi is below the 8 GiB per vCPU this control plane requires"
	g := &genServer{mutateStatus: http.StatusBadRequest, mutateError: refusal}
	b := g.start(t)

	// role is create-time in the controller, so a declared read_only on a
	// read_write endpoint is drift no verb here can fix.
	err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 1,
		Spec: json.RawMessage(`{"role":"read_only","resources":{"cpu":"1"}}`),
	})
	if got := reconcile.PermanentReasonOf(err); got != reconcile.ReasonRejected {
		t.Fatalf("Update = %v (reason %v), want ReasonRejected to outrank the role drift", err, got)
	}
	if !strings.Contains(err.Error(), "role") {
		t.Errorf("error = %q, dropped the unapplied field", err)
	}
	if !strings.Contains(err.Error(), refusal) {
		t.Errorf("error = %q, dropped the resize's own refusal", err)
	}
}

// A drifted spec claims generation 0, so the echo never advances, the engine's
// permanent mark never expires, and this update is never issued again — priorTask
// does not run a second time and the task applyUpdate recorded is never read back.
// Naming the task in the row is therefore its only trace; without it a resize that
// WAS issued is indistinguishable from one that never went out, which is the
// question CLOUDGA-36992 was filed to answer.
func TestDriftedUpdateNamesTheTaskItPlanned(t *testing.T) {
	g := &genServer{} // 202 + a task, the ordinary resize path
	b := g.start(t)

	err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 1,
		Spec: json.RawMessage(`{"role":"read_only","resources":{"cpu":"1"}}`),
	})
	if got := reconcile.PermanentReasonOf(err); got != reconcile.ReasonUnsupported {
		t.Fatalf("Update = %v (reason %v), want the role drift's own reason", err, got)
	}
	if len(g.resizes) != 1 {
		t.Fatalf("resizes = %d, want the resize issued alongside the drift", len(g.resizes))
	}
	if !strings.Contains(err.Error(), genTaskID) {
		t.Errorf("error = %q, does not name the task it planned", err)
	}
	if !strings.Contains(err.Error(), "role") {
		t.Errorf("error = %q, dropped the unapplied field", err)
	}
}

// A spec that does not parse can never succeed on retry, so backing off on it
// leaves the endpoint DEGRADED with nothing saying the declaration is malformed.
// It reports as `rejected`, like every other declaration the agent refuses before
// sending it.
func TestAMalformedSpecIsRejectedNotRetried(t *testing.T) {
	g := &genServer{}
	b := g.start(t)

	err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindEndpoint, Name: genEndpointPath, Generation: 1,
		Spec: json.RawMessage(`{"resources":`),
	})
	if got := reconcile.PermanentReasonOf(err); got != reconcile.ReasonRejected {
		t.Fatalf("Update = %v (reason %v), want ReasonRejected", err, got)
	}
	if len(g.resizes) != 0 {
		t.Errorf("a spec that does not decode reached the controller: %+v", g.resizes)
	}
}
