package ampbackend

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"meko/ycp/internal/api"
	"meko/ycp/reconcile"
)

// These run the engine and this backend together over a stateful fake controller,
// tick by tick, for the create-path scenarios of CLOUDGA-37405. The unit tests
// beside them pin each verdict; these pin that the verdicts compose: that a
// refused create stops costing a task per tick, that a writer swap goes through
// on its own, and that a cluster created at a later generation converges.

// treeServer is a controller holding one pool, its clusters (each with a default
// branch "main"), and the endpoints on cluster recproj's main branch. Every create
// and delete plans a task that stays running until the test settles it, as the
// real worker would.
type treeServer struct {
	t   *testing.T
	url string
	mu  sync.Mutex

	clusters  map[string]string // name -> id
	endpoints map[string]treeEndpoint
	tasks     map[string]*treeTask
	latest    map[string]string // "<kind>/<name>" -> its newest task id
	posts     map[string]int    // "<kind>/<name>" -> creates issued
	seq       int
}

type treeEndpoint struct{ id, role string }

type treeTask struct {
	status, err, code string
	// apply is the task's effect on the tree, run when it succeeds.
	apply func()
}

func newTreeServer(t *testing.T) *treeServer {
	s := &treeServer{
		t:         t,
		clusters:  map[string]string{"recproj": genClusterID},
		endpoints: map[string]treeEndpoint{"rw": {id: genEndpointID, role: "read_write"}},
		tasks:     map[string]*treeTask{},
		latest:    map[string]string{},
		posts:     map[string]int{},
	}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

func (s *treeServer) serve(w http.ResponseWriter, r *http.Request) {
	if serveCatalog(w, r) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	seg := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	const ts = `"created_at":"2026-01-01T00:00:00Z"`

	switch {
	case len(seg) == 1 && seg[0] == "storage-pools":
		fmt.Fprintf(w, `{"storage_pools":[{"id":%q,"name":"pool-a","owner_user_id":"11111111-1111-1111-1111-111111111111",%s}]}`, genPoolID, ts)
	case len(seg) == 3 && seg[2] == "clusters" && r.Method == http.MethodPost:
		var req struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.plan(w, KindCluster, req.Name, func() { s.clusters[req.Name] = clusterID(req.Name) })
	case len(seg) == 3 && seg[2] == "clusters":
		var rows []string
		for name, id := range s.clusters {
			rows = append(rows, fmt.Sprintf(`{"id":%q,"name":%q,"storage_pool_id":%q,%s,"default_branch":%s}`,
				id, name, genPoolID, ts, branchRow(id)))
		}
		fmt.Fprintf(w, `{"clusters":[%s]}`, strings.Join(rows, ","))
	case len(seg) == 5 && seg[4] == "branches":
		fmt.Fprintf(w, `{"branches":[%s]}`, branchRow(seg[3]))
	case len(seg) == 7 && seg[6] == "endpoints" && r.Method == http.MethodPost:
		var req struct {
			Name string
			Role string
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.plan(w, KindEndpoint, req.Name, func() {
			s.endpoints[req.Name] = treeEndpoint{id: fmt.Sprintf("99999999-9999-9999-9999-%012d", s.seq), role: req.Role}
		})
	case len(seg) == 7 && seg[6] == "endpoints":
		var rows []string
		if seg[3] == genClusterID {
			for name, ep := range s.endpoints {
				rows = append(rows, fmt.Sprintf(`{"id":%q,"name":%q,"branch_id":%q,"cluster_id":%q,"role":%q,`+
					`"address":"ep.host:5433","resources":{"cpu":"2","memory":"4Gi"},%s}`,
					ep.id, name, genBranchID, genClusterID, ep.role, ts))
			}
		}
		fmt.Fprintf(w, `{"endpoints":[%s]}`, strings.Join(rows, ","))
	case len(seg) == 8 && seg[6] == "endpoints" && r.Method == http.MethodDelete:
		for name, ep := range s.endpoints {
			if ep.id == seg[7] {
				s.plan(w, "delete-"+KindEndpoint, name, func() { delete(s.endpoints, name) })
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case len(seg) == 2 && seg[0] == "tasks":
		tk, ok := s.tasks[seg[1]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":"not_found","message":"unknown task"}}`)
			return
		}
		body := map[string]any{"id": seg[1], "kind": "create", "status": tk.status,
			"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"}
		if tk.err != "" {
			body["error"] = tk.err
		}
		if tk.code != "" {
			body["error_code"] = tk.code
		}
		_ = json.NewEncoder(w).Encode(body)
	default:
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":"not_found","message":"no such route"}}`)
	}
}

// plan records a running task for kind/name and answers 202 with it. Caller holds mu.
func (s *treeServer) plan(w http.ResponseWriter, kind, name string, apply func()) {
	s.seq++
	id := fmt.Sprintf("77777777-0000-0000-0000-%012d", s.seq)
	s.tasks[id] = &treeTask{status: "running", apply: apply}
	s.latest[kind+"/"+name] = id
	if !strings.HasPrefix(kind, "delete-") {
		s.posts[kind+"/"+name]++
	}
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"id":%q,"kind":"create","status":"running","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`, id)
}

// succeed and fail settle kind/name's newest task, as the worker would.
func (s *treeServer) succeed(kind, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tk := s.task(kind, name)
	tk.status = "succeeded"
	tk.apply()
}

func (s *treeServer) fail(kind, name, err, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tk := s.task(kind, name)
	tk.status, tk.err, tk.code = "failed", err, code
}

func (s *treeServer) task(kind, name string) *treeTask {
	id, ok := s.latest[kind+"/"+name]
	if !ok {
		s.t.Fatalf("no task planned for %s/%s", kind, name)
	}
	return s.tasks[id]
}

func (s *treeServer) creates(kind, name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posts[kind+"/"+name]
}

func clusterID(name string) string {
	return fmt.Sprintf("33333333-3333-3333-3333-%012x", len(name))
}

func branchRow(clusterID string) string {
	id := genBranchID
	if clusterID != genClusterID {
		id = "44444444-0000-0000-0000-" + clusterID[len(clusterID)-12:]
	}
	return fmt.Sprintf(`{"branch_id":%q,"cluster_id":%q,"name":"main","is_default":true,"pg_version":18,`+
		`"created_at":"2026-01-01T00:00:00Z"}`, id, clusterID)
}

// scenario wires the engine over a treeServer with no backoff, so a retryable
// failure is retried on the very next tick and only a held verdict can stop one.
type scenario struct {
	t   *testing.T
	srv *treeServer
	b   *Backend
	eng *reconcile.Engine
}

func newScenario(t *testing.T) *scenario {
	srv := newTreeServer(t)
	b, err := New(srv.url, "", nil, noCtl())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &scenario{t: t, srv: srv, b: b,
		eng: reconcile.New(b, reconcile.Options{BaseBackoff: time.Nanosecond, MaxBackoff: time.Nanosecond})}
}

func (sc *scenario) tick(desired ...api.DesiredResource) reconcile.Result {
	sc.t.Helper()
	res, err := sc.eng.Reconcile(sc.t.Context(), desired, true)
	if err != nil {
		sc.t.Fatalf("Reconcile: %v", err)
	}
	return res
}

// tree is the declared pool/cluster/branch every scenario keeps, plus extra.
func tree(extra ...api.DesiredResource) []api.DesiredResource {
	return append([]api.DesiredResource{
		{Kind: KindPool, Name: "pool-a", Spec: json.RawMessage(`{}`)},
		{Kind: KindCluster, Name: "pool-a/recproj", Spec: json.RawMessage(`{}`)},
		{Kind: KindBranch, Name: genBranchPath, Spec: json.RawMessage(`{}`)},
	}, extra...)
}

func endpointRes(name, role string) api.DesiredResource {
	return api.DesiredResource{Kind: KindEndpoint, Name: genBranchPath + "/" + name,
		Spec: json.RawMessage(`{"role":"` + role + `"}`)}
}

func endpointKey(name string) reconcile.ResourceKey {
	return reconcile.ResourceKey{Kind: KindEndpoint, Name: genBranchPath + "/" + name}
}

// The ticket's scenario. A second writer is refused inside its task; the agent
// must report it, stop planning tasks for it, keep the first writer untouched —
// and let it through once the first writer is withdrawn, without the second
// writer's own declaration changing.
func TestScenarioSecondWriterIsHeldUntilTheFirstGoes(t *testing.T) {
	sc := newScenario(t)
	rw, rw2 := endpointRes("rw", "read_write"), endpointRes("rw2", "read_write")

	res := sc.tick(tree(rw, rw2)...)
	if _, ok := res.Updating[endpointKey("rw2")]; !ok {
		t.Fatalf("tick 1: rw2 not reported in flight; updating=%v", res.Updating)
	}
	sc.tick(tree(rw, rw2)...)
	if n := sc.srv.creates(KindEndpoint, "rw2"); n != 1 {
		t.Fatalf("create calls while the task runs = %d, want 1", n)
	}

	const refusal = "ensure endpoint record: the branch already has a read_write endpoint, or an endpoint named \"rw2\": conflict"
	sc.srv.fail(KindEndpoint, "rw2", refusal, "conflict")
	for i := 0; i < 10; i++ {
		res = sc.tick(tree(rw, rw2)...)
		err, held := res.Permanent[endpointKey("rw2")]
		if !held || reconcile.PermanentReasonOf(err) != reconcile.ReasonRejected || !strings.Contains(err.Error(), refusal) {
			t.Fatalf("tick %d: rw2 = %v (held %v), want rejected carrying the controller's refusal", i, err, held)
		}
		if len(res.Failed) != 0 {
			t.Fatalf("tick %d: failed = %v, want none — a held refusal does not degrade the environment", i, res.Failed)
		}
	}
	if n := sc.srv.creates(KindEndpoint, "rw2"); n != 1 {
		t.Fatalf("create calls after the refusal = %d, want 1 — the agent must stop planning tasks", n)
	}

	// The operator withdraws rw. rw2's declaration is untouched, so only the
	// removal can release it.
	sc.tick(tree(rw2)...)
	sc.srv.succeed("delete-"+KindEndpoint, "rw")
	res = sc.tick(tree(rw2)...)
	if _, held := res.Permanent[endpointKey("rw2")]; held {
		t.Fatalf("rw2 still held after rw was deleted: %v", res.Permanent)
	}
	if n := sc.srv.creates(KindEndpoint, "rw2"); n != 2 {
		t.Fatalf("create calls after rw went = %d, want 2", n)
	}
	sc.srv.succeed(KindEndpoint, "rw2")
	res = sc.tick(tree(rw2)...)
	if len(res.Permanent)+len(res.Failed)+len(res.Updating) != 0 || res.Created != 0 {
		t.Fatalf("after rw2 landed: permanent=%v failed=%v updating=%v created=%d, want converged",
			res.Permanent, res.Failed, res.Updating, res.Created)
	}
}

// The writer swap: one declaration withdraws rw and adds rw2. The engine issues
// both in the same tick and rw2's task loses the race to rw's delete. That must
// never read as rejected — it clears by itself.
func TestScenarioWriterSwapGoesThrough(t *testing.T) {
	sc := newScenario(t)
	rw2 := endpointRes("rw2", "read_write")

	sc.tick(tree(rw2)...)
	if sc.srv.creates(KindEndpoint, "rw2") != 1 {
		t.Fatal("tick 1 did not create rw2 alongside rw's delete")
	}
	sc.srv.fail(KindEndpoint, "rw2", "ensure endpoint record: the branch already has a read_write endpoint: conflict", "conflict")

	res := sc.tick(tree(rw2)...)
	if err, held := res.Permanent[endpointKey("rw2")]; held {
		t.Fatalf("rw2 held while rw's delete is still running: %v", err)
	}
	if _, failed := res.Failed[endpointKey("rw2")]; !failed {
		t.Fatalf("rw2 = not failed; want a retryable failure carrying the conflict (failed=%v)", res.Failed)
	}

	sc.srv.succeed("delete-"+KindEndpoint, "rw")
	sc.tick(tree(rw2)...)
	if n := sc.srv.creates(KindEndpoint, "rw2"); n != 2 {
		t.Fatalf("create calls once rw went = %d, want 2", n)
	}
	sc.srv.succeed(KindEndpoint, "rw2")
	res = sc.tick(tree(rw2)...)
	if len(res.Permanent)+len(res.Failed) != 0 {
		t.Fatalf("after the swap: permanent=%v failed=%v, want converged", res.Permanent, res.Failed)
	}
}

// A cluster declared at a generation above 0 — re-declared after a rejection,
// or edited before its create landed. It echoes no generation and has no update
// path, so once it appears only the create's own record can say its spec landed.
func TestScenarioClusterCreatedAtALaterGenerationConverges(t *testing.T) {
	sc := newScenario(t)
	desired := tree(endpointRes("rw", "read_write"),
		api.DesiredResource{Kind: KindCluster, Name: "pool-a/newdb", Generation: 2, Spec: json.RawMessage(`{}`)},
		api.DesiredResource{Kind: KindBranch, Name: "pool-a/newdb/main", Spec: json.RawMessage(`{}`)})

	sc.tick(desired...)
	sc.srv.succeed(KindCluster, "newdb")
	for i := 0; i < 3; i++ {
		res := sc.tick(desired...)
		if len(res.Permanent) != 0 {
			t.Fatalf("tick %d: permanent = %v, want none — the cluster's create carried this generation", i, res.Permanent)
		}
		if len(res.Failed)+len(res.Updating) != 0 {
			t.Fatalf("tick %d: failed=%v updating=%v, want converged", i, res.Failed, res.Updating)
		}
	}
	if n := sc.srv.creates(KindCluster, "newdb"); n != 1 {
		t.Errorf("cluster create calls = %d, want 1", n)
	}
}

// A failure that is not a conflict is the controller's own trouble, not the
// declaration's: retried, and never held.
func TestScenarioFailedCreateRetries(t *testing.T) {
	sc := newScenario(t)
	ro := endpointRes("ro", "read_only")
	desired := tree(endpointRes("rw", "read_write"), ro)

	sc.tick(desired...)
	sc.srv.fail(KindEndpoint, "ro", "deploy endpoint: helm --wait timed out", "")
	res := sc.tick(desired...)
	if _, failed := res.Failed[endpointKey("ro")]; !failed {
		t.Fatalf("ro = not failed (permanent=%v), want a retryable failure", res.Permanent)
	}
	sc.tick(desired...)
	if n := sc.srv.creates(KindEndpoint, "ro"); n != 2 {
		t.Fatalf("create calls = %d, want 2 — a failed create is retried", n)
	}
	sc.srv.succeed(KindEndpoint, "ro")
	if res := sc.tick(desired...); len(res.Failed)+len(res.Permanent) != 0 {
		t.Fatalf("after ro landed: failed=%v permanent=%v", res.Failed, res.Permanent)
	}
	// Created at generation 0, so the engine plans nothing more for it and nothing
	// would read its create mark again: List must drop it.
	if _, kept := sc.b.creating[endpointKey("ro")]; kept {
		t.Error("ro's create mark outlived its listing")
	}
}
