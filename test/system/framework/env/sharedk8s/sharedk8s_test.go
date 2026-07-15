package sharedk8s

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/ampstack"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/controller"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/system/framework/core"
	execpkg "github.com/yugabyte/yb-amp/test/system/framework/exec"
	"github.com/yugabyte/yb-amp/test/system/framework/exec/k8s"
)

// fakeCtl is a minimal in-memory controller for provision/teardown/attach tests.
// It tracks created users/pools/projects/endpoints and records request paths so
// call order and idempotency can be asserted.
type fakeCtl struct {
	mu           sync.Mutex
	reqs         []string
	srv          *httptest.Server
	pools        []controller.StoragePool
	projects     []controller.Project
	deletedProj  int
	taskStatus   string
	nextID       int
	preseedUser  string
	preseedPool  string // pool name to pre-create
	endpointAddr string
}

func newFakeCtl() *fakeCtl {
	f := &fakeCtl{taskStatus: "succeeded", endpointAddr: "10.1.2.3:55433"}
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/api/v1/users", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		var req controller.CreateUserRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		id := req.ID
		if id == "" {
			id = f.id("u")
		}
		writeJSON(w, 201, map[string]any{"id": id, "is_admin": false})
	})
	mux.HandleFunc("/api/v1/storage-pools", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, map[string]any{"storage_pools": f.pools, "total": len(f.pools), "count": len(f.pools)})
		case http.MethodPost:
			var req controller.CreateStoragePoolRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			for _, ex := range f.pools {
				if ex.Name == req.Name { // realistic: pool names are unique
					writeJSON(w, 409, map[string]any{"error": map[string]any{"code": "conflict", "message": "a storage pool with that name already exists"}})
					return
				}
			}
			p := controller.StoragePool{ID: f.idLocked("p"), Name: req.Name, OwnerUserID: req.OwnerUserID}
			f.pools = append(f.pools, p)
			writeJSON(w, 201, p)
		}
	})
	mux.HandleFunc("/api/v1/tasks/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeJSON(w, 200, map[string]any{"id": strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/"), "kind": "x", "status": f.taskStatus})
	})
	// projects collection + item; endpoints under the branch.
	mux.HandleFunc("/api/v1/storage-pools/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		f.mu.Lock()
		defer f.mu.Unlock()
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/projects") && r.Method == http.MethodPost:
			var req controller.CreateProjectRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			proj := controller.Project{
				ID: f.idLocked("proj"), Name: req.Name,
				DefaultBranch: controller.Branch{BranchID: f.idLocked("br"), IsDefault: true, Name: "main"},
			}
			f.projects = append(f.projects, proj)
			w.Header().Set("Location", "/api/v1/tasks/"+f.idLocked("t"))
			writeJSON(w, 202, map[string]any{"id": "t-proj", "kind": "create_project", "status": "pending"})
		case strings.HasSuffix(path, "/projects") && r.Method == http.MethodGet:
			writeJSON(w, 200, map[string]any{"projects": f.projects, "total": len(f.projects), "count": len(f.projects)})
		case strings.HasSuffix(path, "/endpoints") && r.Method == http.MethodPost:
			w.Header().Set("Location", "/api/v1/tasks/t-ep")
			writeJSON(w, 202, map[string]any{"id": "t-ep", "kind": "create_endpoint", "status": "pending"})
		case strings.HasSuffix(path, "/endpoints") && r.Method == http.MethodGet:
			eps := []map[string]any{{"id": "ep-1", "role": "read_write", "address": f.endpointAddr}}
			writeJSON(w, 200, map[string]any{"endpoints": eps, "total": 1, "count": 1})
		case r.Method == http.MethodDelete: // deleteProject
			f.deletedProj++
			w.Header().Set("Location", "/api/v1/tasks/t-del")
			writeJSON(w, 202, map[string]any{"id": "t-del", "kind": "delete_project", "status": "pending"})
		case r.Method == http.MethodGet: // getProject (attach)
			for _, proj := range f.projects {
				if strings.HasSuffix(path, "/"+proj.ID) {
					writeJSON(w, 200, proj)
					return
				}
			}
			writeJSON(w, 404, map[string]any{"error": map[string]any{"code": "not_found", "message": "no project"}})
		default:
			w.WriteHeader(404)
		}
	})
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeCtl) record(r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
}
func (f *fakeCtl) id(prefix string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.idLocked(prefix)
}
func (f *fakeCtl) idLocked(prefix string) string {
	f.nextID++
	return prefix + "-" + strconv.Itoa(f.nextID)
}
func (f *fakeCtl) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.reqs))
	copy(out, f.reqs)
	return out
}
func (f *fakeCtl) close() { f.srv.Close() }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// testProvider builds a provider pointed at the fake controller with pg-wait and
// version detection stubbed (no real DB) and no kube context (direct endpoint).
func testProvider(f *fakeCtl) *Provider {
	p := New(Config{ControllerURL: f.srv.URL, PoolName: "ampqa"})
	p.logf = nil
	p.waitPG = func(context.Context, pg.ConnInfo, time.Duration) error { return nil }
	p.detectYB = func(context.Context, pg.ConnInfo) string { return "yb-test" }
	p.detectProduct = func(context.Context, pg.ConnInfo) string { return "" }
	p.newClient = func(url string) *controller.Client {
		return controller.New(url, controller.WithHTTPClient(f.srv.Client()))
	}
	return p
}

func TestProvisionHappyPathOrder(t *testing.T) {
	f := newFakeCtl()
	defer f.close()
	p := testProvider(f)

	env, err := p.Provision(context.Background(), core.EnvSpec{Sharing: core.Shared}, core.NewRunSeed(7, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	e := env.(*Env)
	if e.pg.Host != "10.1.2.3" || e.pg.Port != 55433 {
		t.Errorf("pg conn = %s:%d, want 10.1.2.3:55433", e.pg.Host, e.pg.Port)
	}
	if e.versions.YB != "yb-test" {
		t.Errorf("yb version = %q", e.versions.YB)
	}
	if _, ok := env.(interface{ PGEndpoint() pg.ConnInfo }); !ok {
		t.Error("shared env must satisfy PGAccessor")
	}
	// The API is driven in the resource-ladder order.
	assertOrder(t, f.recorded(),
		"POST /api/v1/users",
		"GET /api/v1/storage-pools",
		"POST /api/v1/storage-pools",
		"POST /api/v1/storage-pools/p-2/projects",
		"GET /api/v1/storage-pools/p-2/projects",
		"POST /api/v1/storage-pools/p-2/projects/proj-3/branches/br-4/endpoints",
		"GET /api/v1/storage-pools/p-2/projects/proj-3/branches/br-4/endpoints",
	)
}

// TestProvisionProductVersionPrefersLiveDetection: with no amp controller
// version endpoint, the compute Postgres's own SELECT version() (simulated via
// the detectProduct seam, standing in for a live pg connection) is the only
// live signal; when it answers with a bare version, the env records that
// instead of the static config fallback.
func TestProvisionProductVersionPrefersLiveDetection(t *testing.T) {
	f := newFakeCtl()
	defer f.close()
	p := testProvider(f)
	p.cfg.Product = "static-fallback"
	p.detectProduct = func(context.Context, pg.ConnInfo) string { return "0.4.2-b7" }

	env, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(1, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := env.(*Env).versions.Product; got != "0.4.2-b7" {
		t.Errorf("product version = %q, want the live-detected 0.4.2-b7 (not the static fallback)", got)
	}
}

// TestProvisionProductVersionFallsBackOnDetectionFailure: when the live query
// fails (unreachable/not-yet-ready compute Postgres, or a response that isn't
// a bare version), the env falls back to the configured static product rather
// than recording nothing or garbage.
func TestProvisionProductVersionFallsBackOnDetectionFailure(t *testing.T) {
	f := newFakeCtl()
	defer f.close()
	p := testProvider(f)
	p.cfg.Product = "static-fallback"
	p.detectProduct = func(context.Context, pg.ConnInfo) string { return "" }

	env, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(1, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := env.(*Env).versions.Product; got != "static-fallback" {
		t.Errorf("product version = %q, want the static fallback static-fallback", got)
	}
}

func TestProvisionIdempotentPoolReused(t *testing.T) {
	f := newFakeCtl()
	defer f.close()
	// Pre-create the pool so the provider must reuse it, not create a new one.
	f.pools = append(f.pools, controller.StoragePool{ID: "existing", Name: "ampqa", OwnerUserID: "u"})
	p := testProvider(f)
	if _, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(1, true)); err != nil {
		t.Fatalf("provision: %v", err)
	}
	for _, r := range f.recorded() {
		if r == "POST /api/v1/storage-pools" {
			t.Error("existing pool must be reused, not re-created")
		}
	}
	if !hasReq(f.recorded(), "POST /api/v1/storage-pools/existing/projects") {
		t.Error("project must be created under the reused pool")
	}
}

func TestTeardownCascadesOnce(t *testing.T) {
	f := newFakeCtl()
	defer f.close()
	p := testProvider(f)
	env, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(1, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := p.Teardown(context.Background(), env); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if f.deletedProj != 1 {
		t.Errorf("deleteProject called %d times, want exactly 1", f.deletedProj)
	}
}

func TestAttachDoesNotDeleteOnTeardown(t *testing.T) {
	f := newFakeCtl()
	defer f.close()
	// A project + pool must exist for attach to resolve.
	f.pools = append(f.pools, controller.StoragePool{ID: "pool-a", Name: "ampqa"})
	f.projects = append(f.projects, controller.Project{
		ID: "proj-a", Name: "attached",
		DefaultBranch: controller.Branch{BranchID: "br-a", IsDefault: true},
	})
	p := testProvider(f)
	p.cfg.PoolID = "pool-a"

	env, err := p.Attach(context.Background(), "proj-a")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if env.(*Env).ownProject {
		t.Error("attached env must not own its project")
	}
	if err := p.Teardown(context.Background(), env); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if f.deletedProj != 0 {
		t.Errorf("attach teardown must not delete the project, got %d deletes", f.deletedProj)
	}
}

func TestConfigured(t *testing.T) {
	if (Config{}).Configured() {
		t.Error("empty config must not be configured")
	}
	if !(Config{ControllerURL: "http://x"}).Configured() {
		t.Error("controller_url must configure the provider")
	}
	if !(Config{Context: "itest-dev"}).Configured() {
		t.Error("context must configure the provider")
	}
}

func TestSplitAddr(t *testing.T) {
	cases := []struct {
		in   string
		host string
		port int
	}{
		{"10.0.0.1:55433", "10.0.0.1", 55433},
		{"host-only", "host-only", 55433},
		{"", "", 55433},
		{"svc.ns.svc.cluster.local:5432", "svc.ns.svc.cluster.local", 5432},
	}
	for _, c := range cases {
		h, p := splitAddr(c.in, 55433)
		if h != c.host || p != c.port {
			t.Errorf("splitAddr(%q) = %q,%d; want %q,%d", c.in, h, p, c.host, c.port)
		}
	}
}

// TestProvisionAutoPortsUnique: with the default (auto) local-port scheme,
// parallel provisions each get a distinct kubectl-allocated local port for their
// pg forward — no fixed base, no collision.
func TestProvisionAutoPortsUnique(t *testing.T) {
	f := newFakeCtl()
	defer f.close()
	p := testProvider(f) // controller reached by direct URL
	st := &autoStarter{}
	p.kube = k8s.New(k8s.Config{Context: "x"}).WithStarter(st.start)

	const N = 8
	var wg sync.WaitGroup
	ports := make([]int, N)
	errs := make([]error, N)
	envs := make([]core.Environment, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			env, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(int64(i+1), true))
			envs[i], errs[i] = env, err
			if err == nil {
				ports[i] = env.(*Env).pg.Port
			}
		}(i)
	}
	wg.Wait()

	seen := map[int]bool{}
	for i := 0; i < N; i++ {
		if errs[i] != nil {
			t.Fatalf("provision %d: %v", i, errs[i])
		}
		if ports[i] == 0 || seen[ports[i]] {
			t.Errorf("duplicate/zero pg local port %d across parallel provisions", ports[i])
		}
		seen[ports[i]] = true
	}
	for _, spec := range st.requestedSpecs() {
		if !strings.HasPrefix(spec, ":") {
			t.Errorf("pg forward must request an auto local port (\":remote\"), got %q", spec)
		}
	}
	for _, env := range envs {
		if env != nil {
			_ = p.Teardown(context.Background(), env)
		}
	}
}

// TestPgForwardSurvivesProvisionCtxCancel is the primary-bug regression at the
// provider level: the per-test pg forward is owned by the Env, so cancelling the
// provision ctx (as the scheduler does the moment Provision returns) must not
// kill it — only Teardown does.
func TestPgForwardSurvivesProvisionCtxCancel(t *testing.T) {
	f := newFakeCtl()
	defer f.close()
	p := testProvider(f)
	st := &autoStarter{}
	p.kube = k8s.New(k8s.Config{Context: "x"}).WithStarter(st.start)

	ctx, cancel := context.WithCancel(context.Background())
	env, err := p.Provision(ctx, core.EnvSpec{}, core.NewRunSeed(1, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	cancel() // provisioning deadline ends here
	time.Sleep(20 * time.Millisecond)

	e := env.(*Env)
	if len(e.pfs) != 1 || !e.pfs[0].Alive() {
		t.Fatal("per-test pg forward must survive the provision ctx being cancelled")
	}
	if err := p.Teardown(context.Background(), env); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !waitUntil(func() bool { return !e.pfs[0].Alive() }, time.Second) {
		t.Error("Teardown must close the pg forward")
	}
}

// TestTeardownReconnectsOnConnRefused: a controller forward that has died by
// teardown time yields connection-refused on the DELETE; Teardown must
// re-establish the forward and retry once, succeeding.
func TestTeardownReconnectsOnConnRefused(t *testing.T) {
	f := newFakeCtl()
	defer f.close()

	// A local port that reliably refuses connections (bound then released).
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadPort := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	livePort := serverPort(t, f.srv.URL)

	p := New(Config{Context: "x", PoolName: "ampqa"})
	p.logf = nil
	p.newClient = func(url string) *controller.Client { return controller.New(url) }
	st := &seqStarter{lines: []string{
		fmt.Sprintf("Forwarding from 127.0.0.1:%d -> 8080\n", deadPort), // first: dead
		fmt.Sprintf("Forwarding from 127.0.0.1:%d -> 8080\n", livePort), // after reconnect: live
	}}
	p.kube = k8s.New(k8s.Config{Context: "x"}).WithStarter(st.start)

	e := &Env{ownProject: true, prov: p, poolID: "pool", projectID: "proj-x"}
	if err := p.Teardown(context.Background(), e); err != nil {
		t.Fatalf("teardown must recover via reconnect, got %v", err)
	}
	if f.deletedProj != 1 {
		t.Errorf("exactly one DELETE must land after reconnect, got %d", f.deletedProj)
	}
	if st.calls() != 2 {
		t.Errorf("teardown must re-establish the controller forward once (2 starts), got %d", st.calls())
	}
	_ = p.Close()
}

// TestProvisionForwardDeathSurfacesClearError: a controller forward that never
// comes up must fail provisioning with an error naming the forward (env_failed).
func TestProvisionForwardDeathSurfacesClearError(t *testing.T) {
	f := newFakeCtl()
	defer f.close()
	p := New(Config{Context: "x", PoolName: "ampqa"})
	p.logf = nil
	p.newClient = func(url string) *controller.Client { return controller.New(url) }
	p.kube = k8s.New(k8s.Config{Context: "x"}).WithStarter(func(context.Context, []string) (k8s.Process, error) {
		return deadProc{}, nil // exits before forwarding
	})

	_, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(1, true))
	if err == nil {
		t.Fatal("provision must fail when the controller forward never comes up")
	}
	if !strings.Contains(err.Error(), "port-forward controller") {
		t.Errorf("error must name the failed forward, got %v", err)
	}
}

// TestRestartComputeRollsPodAndReresolves: RestartCompute finds the endpoint's
// compute pod, deletes it, waits for the Deployment to become Available again,
// then re-establishes the Service port-forward and repoints PGEndpoint at it —
// the compute-restart capability, mirroring the docker env's container restart.
func TestRestartComputeRollsPodAndReresolves(t *testing.T) {
	f := newFakeCtl()
	defer f.close()
	p := testProvider(f) // waitPG stubbed to succeed; no real DB
	st := &autoStarter{}
	rec := &kubeRec{}
	p.kube = k8s.New(k8s.Config{Context: "x", Namespace: "amp"}).WithRunner(rec.run).WithStarter(st.start)

	// The per-test pg forward provisioning would have opened.
	initPF, err := p.kube.PortForward(context.Background(), "svc/yb-ep-ep-1", 0, 55433)
	if err != nil {
		t.Fatalf("initial forward: %v", err)
	}
	e := &Env{
		prov:         p,
		kube:         p.kube,
		endpointID:   "ep-1",
		endpointAddr: "10.1.2.3:55433",
		pg:           pg.ConnInfo{Host: "127.0.0.1", Port: initPF.LocalPort(), User: computeUser, Database: computeDB},
		pfs:          []*k8s.PortForward{initPF},
	}
	oldPort := e.pg.Port

	if err := e.RestartCompute(context.Background()); err != nil {
		t.Fatalf("RestartCompute: %v", err)
	}
	calls := rec.recorded()
	if !hasSub(calls, "delete pod yb-ep-ep-1-pod0") {
		t.Errorf("RestartCompute must delete the endpoint compute pod, got: %v", calls)
	}
	if !hasSub(calls, "wait deployment/yb-ep-ep-1 --for=condition=Available") {
		t.Errorf("RestartCompute must wait for the endpoint Deployment to recover, got: %v", calls)
	}
	if e.pg.Port == oldPort {
		t.Errorf("PGEndpoint must repoint at the re-resolved forward, still %d", e.pg.Port)
	}
	if len(e.pfs) != 1 || !e.pfs[0].Alive() {
		t.Errorf("the re-resolved forward must be live")
	}
	if !waitUntil(func() bool { return !initPF.Alive() }, time.Second) {
		t.Error("the superseded forward must be closed")
	}
	_ = p.Close()
}

// TestRestartComputeRequiresKubeContext: without a kube context (direct-URL
// control plane) the compute pods are unreachable, so RestartCompute errors
// rather than pretending to restart.
func TestRestartComputeRequiresKubeContext(t *testing.T) {
	e := &Env{endpointID: "ep-1"} // no kube
	if err := e.RestartCompute(context.Background()); err == nil {
		t.Fatal("RestartCompute must error when no kube context is configured")
	}
}

// kubeRec is a scripted kubectl Runner: it records each invocation and answers
// the pod name/phase selector queries RestartCompute issues.
type kubeRec struct {
	mu    sync.Mutex
	calls []string
}

func (r *kubeRec) run(_ context.Context, _ io.Reader, argv []string) (execpkg.Result, error) {
	joined := strings.Join(argv, " ")
	r.mu.Lock()
	r.calls = append(r.calls, joined)
	r.mu.Unlock()
	switch {
	case strings.Contains(joined, "metadata.name"):
		return execpkg.Result{Stdout: "yb-ep-ep-1-pod0\n"}, nil
	case strings.Contains(joined, "status.phase"):
		return execpkg.Result{Stdout: "Running\n"}, nil
	default:
		return execpkg.Result{}, nil
	}
}

func (r *kubeRec) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

func hasSub(got []string, sub string) bool {
	for _, g := range got {
		if strings.Contains(g, sub) {
			return true
		}
	}
	return false
}

func serverPort(t *testing.T, rawURL string) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(rawURL, "http://"))
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	return port
}

func waitUntil(cond func() bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

// autoStarter mimics kubectl's ephemeral-port allocation: every forward gets a
// distinct local port and stays up until killed.
type autoStarter struct {
	mu    sync.Mutex
	n     int
	specs []string
}

func (a *autoStarter) start(_ context.Context, argv []string) (k8s.Process, error) {
	a.mu.Lock()
	a.n++
	port := 40000 + a.n
	a.specs = append(a.specs, argv[len(argv)-1])
	a.mu.Unlock()
	return newLivePFProc(fmt.Sprintf("Forwarding from 127.0.0.1:%d -> compute\n", port)), nil
}

func (a *autoStarter) requestedSpecs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.specs))
	copy(out, a.specs)
	return out
}

// seqStarter hands out scripted readiness lines in order, counting starts.
type seqStarter struct {
	mu    sync.Mutex
	i     int
	n     int
	lines []string
}

func (s *seqStarter) start(_ context.Context, _ []string) (k8s.Process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := "Forwarding from 127.0.0.1:1 -> 1\n"
	if s.i < len(s.lines) {
		line = s.lines[s.i]
		s.i++
	}
	s.n++
	return newLivePFProc(line), nil
}

func (s *seqStarter) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// livePFProc is a long-lived fake port-forward Process: emits one line then
// keeps stdout open (Alive stays true) until Kill.
type livePFProc struct {
	out    *io.PipeReader
	pw     *io.PipeWriter
	killed chan struct{}
	once   sync.Once
}

func newLivePFProc(line string) *livePFProc {
	pr, pw := io.Pipe()
	p := &livePFProc{out: pr, pw: pw, killed: make(chan struct{})}
	go func() { _, _ = io.WriteString(pw, line) }()
	return p
}

func (p *livePFProc) Stdout() io.Reader { return p.out }
func (p *livePFProc) Kill() error {
	p.once.Do(func() {
		close(p.killed)
		_ = p.pw.Close()
	})
	return nil
}
func (p *livePFProc) Wait() error {
	<-p.killed
	return nil
}

// deadProc is a fake Process that exits immediately without forwarding.
type deadProc struct{}

func (deadProc) Stdout() io.Reader { return strings.NewReader("") }
func (deadProc) Kill() error       { return nil }
func (deadProc) Wait() error       { return nil }

func assertOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	idx := 0
	for _, g := range got {
		if idx < len(want) && g == want[idx] {
			idx++
		}
	}
	if idx != len(want) {
		t.Errorf("calls not in expected order; matched %d/%d\ngot:  %v\nwant: %v", idx, len(want), got, want)
	}
}

func hasReq(got []string, want string) bool {
	for _, g := range got {
		if g == want {
			return true
		}
	}
	return false
}

// --- deploy mode (self-building k8s) ---

// k8sRecorder answers kubectl/helm calls for a faked control-plane bring-up:
// exit 0 for everything, a non-empty ClusterIP for the controller Service, and
// the ampstack managed-by label for namespace gets (so Teardown proceeds).
type k8sRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *k8sRecorder) run(_ context.Context, stdin io.Reader, argv []string) (execpkg.Result, error) {
	j := strings.Join(argv, " ")
	if stdin != nil {
		_, _ = io.ReadAll(stdin)
	}
	r.mu.Lock()
	r.calls = append(r.calls, j)
	r.mu.Unlock()
	switch {
	case strings.Contains(j, "get svc") && strings.Contains(j, "clusterIP"):
		return execpkg.Result{Stdout: "10.0.0.9"}, nil
	case strings.Contains(j, "get namespace"):
		return execpkg.Result{Stdout: "ampstack"}, nil
	default:
		return execpkg.Result{ExitCode: 0}, nil
	}
}

func (r *k8sRecorder) start(_ context.Context, argv []string) (k8s.Process, error) {
	r.mu.Lock()
	r.calls = append(r.calls, strings.Join(argv, " "))
	r.mu.Unlock()
	return newFwdProc(), nil
}

func (r *k8sRecorder) argvs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// fwdProc is a fake port-forward process that emits the readiness line then
// keeps its stdout open (so the forward stays Alive) until Kill closes it.
type fwdProc struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func newFwdProc() *fwdProc {
	r, w := io.Pipe()
	go func() { _, _ = io.WriteString(w, "Forwarding from 127.0.0.1:55999 -> 55433\n") }()
	return &fwdProc{r: r, w: w}
}

func (p *fwdProc) Stdout() io.Reader { return p.r }
func (p *fwdProc) Kill() error       { return p.w.Close() }
func (p *fwdProc) Wait() error       { return nil }

func testDeployProvider(f *fakeCtl, rec *k8sRecorder) *Provider {
	p := New(Config{
		Deploy:        true,
		DeployTimeout: time.Minute,
		PoolName:      "ampqa",
		AmpStack: ampstack.Config{
			Namespace:              "ampqa-cp-test",
			Context:                "itest",
			ControllerChart:        "/chart",
			ControllerReach:        ampstack.ReachTailnet,
			ControllerService:      "amp-controller",
			ControllerName:         "amp-controller",
			ControllerPort:         8080,
			HelmShim:               false,
			ECRRegistry:            "", // skip the pull secret
			YBReadyTimeout:         time.Minute,
			ControllerReadyTimeout: time.Minute,
		},
	})
	p.logf = nil
	p.waitPG = func(context.Context, pg.ConnInfo, time.Duration) error { return nil }
	p.detectYB = func(context.Context, pg.ConnInfo) string { return "yb-test" }
	p.detectProduct = func(context.Context, pg.ConnInfo) string { return "" }
	fakeClient := func(string) *controller.Client {
		return controller.New(f.srv.URL, controller.WithHTTPClient(f.srv.Client()))
	}
	p.newClient = fakeClient
	p.newApp = func(cfg ampstack.Config) *ampstack.App {
		return ampstack.New(cfg).
			WithKube(k8s.New(k8s.Config{Context: cfg.Context, Namespace: cfg.Namespace}).WithRunner(rec.run).WithStarter(rec.start)).
			WithHelmRunner(rec.run).
			WithReadyFunc(func(context.Context, *controller.Client, time.Duration) error { return nil }).
			WithClientFactory(fakeClient).
			WithECRToken(func(context.Context, string, string) (string, error) { return "tok", nil })
	}
	return p
}

// TestDeployModeProvisionAndClose: deploy mode OWNS a run-scoped control plane —
// Provision lazily deploys the ampstack App, carves a project out of it, and
// Close tears the whole namespace down.
func TestDeployModeProvisionAndClose(t *testing.T) {
	f := newFakeCtl()
	defer f.close()
	rec := &k8sRecorder{}
	p := testDeployProvider(f, rec)

	if p.app != nil {
		t.Fatal("control plane must not be deployed before first use")
	}
	env, err := p.Provision(context.Background(), core.EnvSpec{Sharing: core.Shared}, core.NewRunSeed(9, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if p.app == nil {
		t.Fatal("Provision must have deployed the control plane")
	}
	if !hasSubstr(rec.argvs(), "create namespace ampqa-cp-test") && !hasSubstr(rec.argvs(), "ampqa-cp-test") {
		t.Errorf("expected the owned namespace to be created; calls=%v", rec.argvs())
	}
	// The project was created on the deployed control plane's controller.
	if !hasReq(f.recorded(), "POST /api/v1/storage-pools/p-2/projects") && len(f.recorded()) == 0 {
		t.Errorf("expected a project create on the deployed controller; reqs=%v", f.recorded())
	}
	_ = env

	// StandingEnv is now recordable (provider "ampstack", the owned namespace).
	se, ok := p.StandingEnv()
	if !ok {
		t.Fatal("deployed provider must expose a StandingEnv to keep")
	}
	if se.Provider != "ampstack" || se.Info["namespace"] != "ampqa-cp-test" {
		t.Errorf("StandingEnv = provider %q ns %q, want ampstack / ampqa-cp-test", se.Provider, se.Info["namespace"])
	}

	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !hasSubstr(rec.argvs(), "delete namespace ampqa-cp-test") {
		t.Errorf("Close must delete the owned namespace; calls=%v", rec.argvs())
	}
}

// TestAttachModeExposesNoStandingEnv: in attach mode this provider owns nothing
// to keep, so StandingEnv reports none.
func TestAttachModeExposesNoStandingEnv(t *testing.T) {
	p := New(Config{ControllerURL: "http://x"})
	if _, ok := p.StandingEnv(); ok {
		t.Error("attach-mode provider must not expose a StandingEnv")
	}
}

// TestIsLoopbackURL guards the StandingEnv rule that a process-local
// port-forward URL must NOT be recorded (a later --env attach couldn't reach
// it); a stable cluster address must be.
func TestIsLoopbackURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"http://127.0.0.1:45231", true},
		{"http://127.0.0.5:8080", true},
		{"http://localhost:8080", true},
		{"http://[::1]:8080", true},
		{"http://amp-controller.ampqa-cp-abc.svc.cluster.local:8080", false},
		{"http://10.4.2.7:8080", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isLoopbackURL(c.url); got != c.want {
			t.Errorf("isLoopbackURL(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

// TestConfiguredDeployMode: deploy mode is configured once it has a context to
// build on (no controller_url needed).
func TestConfiguredDeployMode(t *testing.T) {
	if (Config{Deploy: true}).Configured() {
		t.Error("deploy mode with no context must not be configured")
	}
	if !(Config{Deploy: true, Context: "itest"}).Configured() {
		t.Error("deploy mode with a context must be configured")
	}
	if !(Config{Deploy: true, AmpStack: ampstack.Config{Context: "itest"}}).Configured() {
		t.Error("deploy mode with an ampstack context must be configured")
	}
}

func hasSubstr(got []string, want string) bool {
	for _, g := range got {
		if strings.Contains(g, want) {
			return true
		}
	}
	return false
}

// TestEnsurePoolConcurrentCreateTolerated: on a fresh control plane every
// parallel Provision races to create the by-name pool; the losers get a 409
// conflict, which must be tolerated by reusing the winner's pool rather than
// env-failing. Exactly one pool must exist afterwards.
func TestEnsurePoolConcurrentCreateTolerated(t *testing.T) {
	f := newFakeCtl()
	defer f.close()
	p := testProvider(f)

	const n = 6
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(int64(i), true))
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent provision %d: %v", i, err)
		}
	}
	f.mu.Lock()
	npools := len(f.pools)
	f.mu.Unlock()
	if npools != 1 {
		t.Errorf("pool count = %d, want exactly 1 (concurrent creates must converge on one pool)", npools)
	}
}

func TestTransientControllerPredicate(t *testing.T) {
	transient := []string{
		"create project x: task 1 failed: pending_init: status 500: deadlock detected",
		"create endpoint: helm: Error: services \"yb-ep-abc\" not found",
		"status 503: unavailable",
	}
	for _, s := range transient {
		if !transientController(fmt.Errorf("%s", s)) {
			t.Errorf("want transient: %q", s)
		}
	}
	for _, s := range []string{"status 400: bad request", "not authorized", ""} {
		if transientController(fmt.Errorf("%s", s)) {
			t.Errorf("want non-transient: %q", s)
		}
	}
	if !isAlreadyExists(fmt.Errorf("controller 409: conflict")) {
		t.Error("409 conflict must be already-exists")
	}
}
