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
	// recordEarly lists an endpoint, with no address, as soon as its create is
	// planned, as the real controller does; the task then only deploys it.
	recordEarly bool
	// noCreateTask lists endpoints without create_task, as a controller that
	// predates it does.
	noCreateTask bool
}

// treeEndpoint is an endpoint row. pending is one recordEarly listed before its
// deploy, which has no address; createTask is the task that created it.
type treeEndpoint struct {
	id, role   string
	pending    bool
	createTask string
}

type treeTask struct {
	// kind is the controller's task kind, as create_task reports it.
	kind              string
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
		epID := fmt.Sprintf("99999999-9999-9999-9999-%012d", s.seq+1)
		var taskID string
		taskID = s.plan(w, KindEndpoint, req.Name, func() {
			s.endpoints[req.Name] = treeEndpoint{id: epID, role: req.Role, createTask: taskID}
		})
		s.tasks[taskID].kind = "create_endpoint"
		if s.recordEarly {
			s.endpoints[req.Name] = treeEndpoint{id: epID, role: req.Role, pending: true, createTask: taskID}
		}
	case len(seg) == 7 && seg[6] == "endpoints":
		var rows []string
		if seg[3] == genClusterID {
			for name, ep := range s.endpoints {
				addr := "ep.host:5433"
				if ep.pending {
					addr = ""
				}
				rows = append(rows, fmt.Sprintf(`{"id":%q,"name":%q,"branch_id":%q,"cluster_id":%q,"role":%q,`+
					`"address":%q,"resources":{"cpu":"2","memory":"4Gi"},%s%s}`,
					ep.id, name, genBranchID, genClusterID, ep.role, addr, ts, s.createTaskField(ep)))
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

// createTaskField is the create_task an endpoint row carries, as `,"create_task":…`,
// or "" when it has none. Caller holds mu.
func (s *treeServer) createTaskField(ep treeEndpoint) string {
	tk, ok := s.tasks[ep.createTask]
	if s.noCreateTask || !ok {
		return ""
	}
	ct := map[string]any{"id": ep.createTask, "kind": tk.kind, "status": tk.status}
	if tk.err != "" {
		ct["error"] = tk.err
	}
	if tk.code != "" {
		ct["error_code"] = tk.code
	}
	b, _ := json.Marshal(ct)
	return `,"create_task":` + string(b)
}

// plan records a running task for kind/name, answers 202 with it, and returns its
// id. Caller holds mu.
func (s *treeServer) plan(w http.ResponseWriter, kind, name string, apply func()) string {
	s.seq++
	id := fmt.Sprintf("77777777-0000-0000-0000-%012d", s.seq)
	s.tasks[id] = &treeTask{status: "running", apply: apply}
	s.latest[kind+"/"+name] = id
	if !strings.HasPrefix(kind, "delete-") {
		s.posts[kind+"/"+name]++
	}
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"id":%q,"kind":"create","status":"running","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`, id)
	return id
}

// reclaim removes an endpoint row, as the controller's failed-create sweep does.
func (s *treeServer) reclaim(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.endpoints, name)
}

// seedFailedWriter lists an address-less endpoint whose create failed inside a
// task of taskKind that this agent never issued — a writer a create_cluster or
// branch made, or one created before the agent restarted.
func (s *treeServer) seedFailedWriter(name, taskKind, cause string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := fmt.Sprintf("77777777-0000-0000-0000-%012d", s.seq)
	s.tasks[id] = &treeTask{kind: taskKind, status: "failed", err: cause}
	s.endpoints[name] = treeEndpoint{id: fmt.Sprintf("99999999-9999-9999-9999-%012d", s.seq),
		role: "read_write", pending: true, createTask: id}
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

// restart replaces the agent's backend and engine with fresh ones over the same
// controller, as a pod restart does: nothing the old process remembered survives.
func (sc *scenario) restart() {
	b, err := New(sc.srv.url, "", nil, noCtl())
	if err != nil {
		sc.t.Fatalf("New: %v", err)
	}
	sc.b = b
	sc.eng = reconcile.New(b, reconcile.Options{BaseBackoff: time.Nanosecond, MaxBackoff: time.Nanosecond})
}

func actualOf(res reconcile.Result, k reconcile.ResourceKey) (reconcile.Actual, bool) {
	for _, a := range res.Actual {
		if a.Key == k {
			return a, true
		}
	}
	return reconcile.Actual{}, false
}

// assertFailedRow checks a row reads as a failed create: state error, not Ready,
// and the controller's own error in Failed — which is what degrades the
// environment on this tick.
func assertFailedRow(t *testing.T, res reconcile.Result, k reconcile.ResourceKey, wants ...string) {
	t.Helper()
	a, ok := actualOf(res, k)
	if !ok || a.State != "error" || a.Ready || a.Detail != "" {
		t.Fatalf("%s = %+v (listed %v), want an unready error row whose reason is the failure alone", k, a, ok)
	}
	err, failed := res.Failed[k]
	if !failed {
		t.Fatalf("%s not in Failed (failed=%v)", k, res.Failed)
	}
	for _, w := range wants {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("%s failure = %q, want it to contain %q", k, err, w)
		}
	}
}

const capacity = "deploy endpoint: pod could not be scheduled: 0/6 nodes are available: 3 Insufficient ephemeral-storage"

// CLOUDGA-37682. The controller lists an endpoint before deploying it and keeps
// the row after the deploy fails, until its sweep reclaims it. In between the row
// must read as failed, on every tick, without a second create; once reclaimed the
// create is issued again and the old failure does not stick to the new attempt.
func TestScenarioFailedCreateIsReportedOnTheListedRow(t *testing.T) {
	sc := newScenario(t)
	sc.srv.recordEarly = true
	ro := endpointRes("ro", "read_only")
	desired := tree(endpointRes("rw", "read_write"), ro)

	sc.tick(desired...)
	res := sc.tick(desired...)
	if a, _ := actualOf(res, endpointKey("ro")); a.State != "provisioning" {
		t.Fatalf("ro while its task runs = %q, want provisioning", a.State)
	}
	if _, failed := res.Failed[endpointKey("ro")]; failed {
		t.Fatal("ro failed while its task is still running")
	}

	sc.srv.fail(KindEndpoint, "ro", capacity, "")
	for i := 0; i < 3; i++ {
		assertFailedRow(t, sc.tick(desired...), endpointKey("ro"), "create_endpoint task", capacity)
	}
	if n := sc.srv.creates(KindEndpoint, "ro"); n != 1 {
		t.Fatalf("create calls while the failed row is listed = %d, want 1", n)
	}

	sc.srv.reclaim("ro")
	sc.tick(desired...)
	if n := sc.srv.creates(KindEndpoint, "ro"); n != 2 {
		t.Fatalf("create calls once reclaimed = %d, want 2", n)
	}
	res = sc.tick(desired...)
	if a, _ := actualOf(res, endpointKey("ro")); a.State != "provisioning" {
		t.Fatalf("ro on its retry = %q, want provisioning — the old failure must not stick", a.State)
	}
	if _, failed := res.Failed[endpointKey("ro")]; failed {
		t.Fatalf("ro on its retry still failed: %v", res.Failed[endpointKey("ro")])
	}

	sc.srv.succeed(KindEndpoint, "ro")
	res = sc.tick(desired...)
	if a, _ := actualOf(res, endpointKey("ro")); a.State != "ready" || len(res.Failed) != 0 {
		t.Fatalf("ro once deployed = %q, failed=%v; want ready and nothing failed", a.State, res.Failed)
	}
}

// The failure is read from the controller every tick, not remembered, so an agent
// that restarts after the create failed reports it on its first tick.
func TestScenarioFailedCreateSurvivesAnAgentRestart(t *testing.T) {
	sc := newScenario(t)
	sc.srv.recordEarly = true
	desired := tree(endpointRes("rw", "read_write"), endpointRes("ro", "read_only"))
	sc.tick(desired...)
	sc.srv.fail(KindEndpoint, "ro", capacity, "")

	sc.restart()
	assertFailedRow(t, sc.tick(desired...), endpointKey("ro"), capacity)
	if n := sc.srv.creates(KindEndpoint, "ro"); n != 1 {
		t.Fatalf("create calls after the restart = %d, want 1 — the row is listed", n)
	}
}

// A writer is created inside a create_cluster or branch task, not by this agent's
// own create: the same failure, reported the same way.
func TestScenarioFailedWriterOfAnotherTaskKind(t *testing.T) {
	for _, kind := range []string{"create_cluster", "branch"} {
		t.Run(kind, func(t *testing.T) {
			sc := newScenario(t)
			sc.srv.seedFailedWriter("w", kind, capacity)
			desired := tree(endpointRes("rw", "read_write"), endpointRes("w", "read_write"))
			for i := 0; i < 2; i++ {
				assertFailedRow(t, sc.tick(desired...), endpointKey("w"), kind+" task", capacity)
			}
			if n := sc.srv.creates(KindEndpoint, "w"); n != 0 {
				t.Errorf("create calls = %d, want 0 — the row is listed", n)
			}
		})
	}
}

// A failed endpoint nobody declares any more is collected, and its failure is
// owed to no one: it must not degrade the environment while the delete runs.
func TestScenarioWithdrawnFailedEndpointIsCollectedNotReported(t *testing.T) {
	sc := newScenario(t)
	sc.srv.seedFailedWriter("w", "create_cluster", capacity)
	res := sc.tick(tree(endpointRes("rw", "read_write"))...)
	if _, failed := res.Failed[endpointKey("w")]; failed {
		t.Fatalf("withdrawn w reported failed: %v", res.Failed[endpointKey("w")])
	}
	sc.srv.mu.Lock()
	_, deleting := sc.srv.latest["delete-"+KindEndpoint+"/w"]
	sc.srv.mu.Unlock()
	if !deleting {
		t.Error("withdrawn w was not deleted")
	}
}

// A controller that predates create_task says nothing about the create, so the
// row is what it always was: provisioning, left to the stuck timer.
func TestScenarioControllerWithoutCreateTask(t *testing.T) {
	sc := newScenario(t)
	sc.srv.recordEarly, sc.srv.noCreateTask = true, true
	desired := tree(endpointRes("rw", "read_write"), endpointRes("ro", "read_only"))
	sc.tick(desired...)
	sc.srv.fail(KindEndpoint, "ro", capacity, "")
	res := sc.tick(desired...)
	if a, _ := actualOf(res, endpointKey("ro")); a.State != "provisioning" || a.Failure != nil {
		t.Fatalf("ro = %+v, want provisioning with no failure", a)
	}
	if _, failed := res.Failed[endpointKey("ro")]; failed {
		t.Fatal("ro failed with nothing to say so")
	}
}

// A create task that failed after the endpoint was deployed (a later step of a
// create_cluster, say) leaves a working endpoint: it reads as its address and
// health say, not as a failed create.
func TestScenarioDeployedEndpointOfAFailedTaskIsNotFailed(t *testing.T) {
	sc := newScenario(t)
	sc.srv.seedFailedWriter("w", "create_cluster", "a later step failed")
	sc.srv.mu.Lock()
	ep := sc.srv.endpoints["w"]
	ep.pending = false
	sc.srv.endpoints["w"] = ep
	sc.srv.mu.Unlock()

	res := sc.tick(tree(endpointRes("rw", "read_write"), endpointRes("w", "read_write"))...)
	if a, _ := actualOf(res, endpointKey("w")); a.State != "ready" || a.Failure != nil {
		t.Fatalf("w = %+v, want ready", a)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("failed = %v, want none", res.Failed)
	}
}

// settleWithoutDeploy marks kind/name's newest task succeeded without running
// its effect: a create that finished before its address was recorded.
func (s *treeServer) settleWithoutDeploy(kind, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.task(kind, name).status = "succeeded"
}

func (s *treeServer) setNoCreateTask(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noCreateTask = v
}

// create_task absent means unknown — the controller's lookup failed, or it is an
// older one mid-rollback — so a failed row goes back to provisioning rather than
// keeping a verdict nothing is reporting any more, and is failed again once the
// field is back.
func TestScenarioCreateTaskThatGoesMissingIsUnknownNotFailed(t *testing.T) {
	sc := newScenario(t)
	sc.srv.recordEarly = true
	desired := tree(endpointRes("rw", "read_write"), endpointRes("ro", "read_only"))
	sc.tick(desired...)
	sc.srv.fail(KindEndpoint, "ro", capacity, "")
	assertFailedRow(t, sc.tick(desired...), endpointKey("ro"), capacity)

	sc.srv.setNoCreateTask(true)
	res := sc.tick(desired...)
	if a, _ := actualOf(res, endpointKey("ro")); a.State != "provisioning" || a.Failure != nil {
		t.Fatalf("ro without create_task = %+v, want provisioning", a)
	}
	if _, failed := res.Failed[endpointKey("ro")]; failed {
		t.Fatal("ro still failed with nothing reporting it")
	}

	sc.srv.setNoCreateTask(false)
	assertFailedRow(t, sc.tick(desired...), endpointKey("ro"), capacity)
	if n := sc.srv.creates(KindEndpoint, "ro"); n != 1 {
		t.Errorf("create calls = %d, want 1 — the row stayed listed throughout", n)
	}
}

// A create that succeeded is not a failure, address or not: the endpoint stays
// provisioning until its address lands.
func TestScenarioSucceededCreateWithoutAnAddressIsProvisioning(t *testing.T) {
	sc := newScenario(t)
	sc.srv.recordEarly = true
	desired := tree(endpointRes("rw", "read_write"), endpointRes("ro", "read_only"))
	sc.tick(desired...)
	sc.srv.settleWithoutDeploy(KindEndpoint, "ro")
	res := sc.tick(desired...)
	if a, _ := actualOf(res, endpointKey("ro")); a.State != "provisioning" || a.Failure != nil {
		t.Fatalf("ro = %+v, want provisioning", a)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("failed = %v, want none", res.Failed)
	}
}

// One endpoint's failed create says nothing about its neighbours: only it is
// failed, and the others keep converging.
func TestScenarioFailedCreateIsConfinedToItsEndpoint(t *testing.T) {
	sc := newScenario(t)
	sc.srv.recordEarly = true
	desired := tree(endpointRes("rw", "read_write"), endpointRes("ro1", "read_only"), endpointRes("ro2", "read_only"))
	sc.tick(desired...)
	sc.srv.fail(KindEndpoint, "ro1", capacity, "")
	sc.srv.succeed(KindEndpoint, "ro2")
	res := sc.tick(desired...)

	assertFailedRow(t, res, endpointKey("ro1"), capacity)
	if len(res.Failed) != 1 {
		t.Errorf("failed = %v, want only ro1", res.Failed)
	}
	for _, name := range []string{"rw", "ro2"} {
		if a, _ := actualOf(res, endpointKey(name)); a.State != "ready" || a.Failure != nil {
			t.Errorf("%s = %+v, want ready", name, a)
		}
	}
}
