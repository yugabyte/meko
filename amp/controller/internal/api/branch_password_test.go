package api_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/config"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/node"
	"github.com/yugabyte/amp/controller/internal/task"
)

// rfc7677Verifier is the RFC 7677 test vector in pg_authid's form: a
// well-formed SCRAM-SHA-256 verifier at the minimum strength accepted.
const rfc7677Verifier = "SCRAM-SHA-256$4096:W22ZaJ0SNY7soEsUEjb6gQ==$" +
	"WG5d8oPm3OtcPnkdi4Uo7BkeZkBFzpcXkuLmtbsT4qY=:wfPLwcE6nTWhTAmQ7tl2KeoiWGPlZqQxSrmfPwDl2dU="

// lockedBuffer is a log sink the server goroutines and the test can share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newLoggedTestServer is newTestServerWith with the server's log captured.
func newLoggedTestServer(t *testing.T, backend *core.MemBackend, prov api.Provisioning) (*api.ClientWithResponses, *task.MemStore, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{}
	tasks := task.NewMemStore()
	logger := slog.New(slog.NewTextHandler(logs, nil))
	cfg := &config.Config{ListenAddr: ":0"}
	cfg.Deploy.Enabled = prov.Deployer != nil
	srv := api.NewServer(cfg, backend, task.NewService(tasks, tasks.Subtasks()), prov, nil, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client, err := api.NewClientWithResponses(ts.URL + "/api/v1")
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	return client, tasks, logs
}

// seedLineage seeds a root (with the given floor; "" for none) and a fork of it
// taken at forkLSN, both in one cluster.
func seedLineage(t *testing.T, client *api.ClientWithResponses, backend *core.MemBackend, rootFloor, forkLSN string) (poolID, clusterID, rootID, forkID uuid.UUID) {
	t.Helper()
	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "lineage-"+uuid.NewString()[:8])
	clusterID, rootID, forkID = uuid.New(), uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{
		ClusterID: clusterID, BranchID: rootID, Name: "main", PgVersion: 18, IsDefault: true, ForkLSN: rootFloor,
	})
	parent := rootID
	backend.SeedBranch(pool.Id, core.Branch{
		ClusterID: clusterID, BranchID: forkID, Name: "dev", PgVersion: 18, ParentBranchID: &parent, ForkLSN: forkLSN,
	})
	return pool.Id, clusterID, rootID, forkID
}

// Where no password is required, one sent is validated and then dropped: the
// writer deploys with no verifier, so the supervisor leaves the catalog's
// password alone, and the drop is logged by id without the credential.
func TestWriterPasswordDroppedWhereNotRequired(t *testing.T) {
	for name, onFork := range map[string]bool{"a fork": true, "a root with a floor": false} {
		t.Run(name, func(t *testing.T) {
			dep := &fakeDeployer{}
			prov := api.Provisioning{Deployer: dep}
			backend := core.NewMemBackend()
			client, store, logs := newLoggedTestServer(t, backend, prov)
			ctx := context.Background()
			poolID, clusterID, rootID, forkID := seedLineage(t, client, backend, "0/0", "0/16B3748")
			target := rootID
			if onFork {
				target = forkID
			}

			resp, err := client.CreateEndpointWithResponse(ctx, poolID, clusterID, target,
				api.CreateEndpointRequest{Password: strptr(testPassword)})
			if err != nil {
				t.Fatalf("CreateEndpoint: %v", err)
			}
			if resp.StatusCode() != http.StatusAccepted {
				t.Fatalf("status = %d, want 202; body = %s", resp.StatusCode(), resp.Body)
			}
			drainTasks(t, store, backend, prov)
			if len(dep.created) != 1 {
				t.Fatalf("deployer saw %d specs, want 1", len(dep.created))
			}
			if v := dep.created[0].PasswordVerifier; v != "" {
				t.Errorf("spec.PasswordVerifier = %q, want empty: a password that is not required is dropped", v)
			}

			out := logs.String()
			var warn string
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, "level=WARN") && strings.Contains(line, "password") {
					warn = line
				}
			}
			if warn == "" {
				t.Fatalf("no WARN about the dropped password; log:\n%s", out)
			}
			for _, want := range []string{clusterID.String(), target.String()} {
				if !strings.Contains(warn, want) {
					t.Errorf("warning %q does not name %s", warn, want)
				}
			}
			if strings.Contains(out, testPassword) || strings.Contains(out, "SCRAM-SHA-256$") {
				t.Errorf("the password or a verifier reached the log:\n%s", out)
			}
		})
	}
}

// Validated exactly as a required one: a malformed password is still a 400
// where it would be dropped.
func TestWriterPasswordValidatedWhereNotRequired(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	backend := core.NewMemBackend()
	client, _, _ := newTestServerWith(t, backend, prov)
	ctx := context.Background()
	poolID, clusterID, _, forkID := seedLineage(t, client, backend, "0/0", "0/16B3748")

	for why, pw := range map[string]string{
		"a short plaintext":    "short",
		"a malformed verifier": "SCRAM-SHA-256$4095:" + strings.Repeat("A", 140),
	} {
		resp, err := client.CreateEndpointWithResponse(ctx, poolID, clusterID, forkID,
			api.CreateEndpointRequest{Password: strptr(pw)})
		if err != nil {
			t.Fatalf("CreateEndpoint(%s): %v", why, err)
		}
		if resp.StatusCode() != http.StatusBadRequest {
			t.Errorf("%s on a fork: status = %d, want 400; body = %s", why, resp.StatusCode(), resp.Body)
		}
	}
	if len(dep.created) != 0 {
		t.Errorf("a rejected request still reached the deployer %d time(s)", len(dep.created))
	}
}

// A fork's writer, and a root's once its floor is set, need no password.
func TestWriterNeedsNoPasswordWhereNotRequired(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	backend := core.NewMemBackend()
	client, _, _ := newTestServerWith(t, backend, prov)
	ctx := context.Background()
	poolID, clusterID, rootID, forkID := seedLineage(t, client, backend, "0/0", "0/16B3748")

	for _, branchID := range []uuid.UUID{rootID, forkID} {
		resp, err := client.CreateEndpointWithResponse(ctx, poolID, clusterID, branchID, api.CreateEndpointRequest{})
		if err != nil {
			t.Fatalf("CreateEndpoint: %v", err)
		}
		if resp.StatusCode() != http.StatusAccepted {
			t.Errorf("branch %s: status = %d, want 202; body = %s", branchID, resp.StatusCode(), resp.Body)
		}
	}
}

// A SCRAM-SHA-256 verifier is passed through as the caller sent it, on both
// create paths, so the plaintext never has to reach the controller.
func TestWriterPasswordAcceptsAVerifier(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	backend := core.NewMemBackend()
	client, _, store := newTestServerWith(t, backend, prov)
	ctx := context.Background()
	poolID, clusterID, branchID := seedBranchForCreds(t, client, backend, "verifier-pool")

	resp, err := client.CreateEndpointWithResponse(ctx, poolID, clusterID, branchID,
		api.CreateEndpointRequest{Password: strptr(rfc7677Verifier)})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateEndpoint status = %d, want 202; body = %s", resp.StatusCode(), resp.Body)
	}
	withWriter := true
	cl, err := client.CreateClusterWithResponse(ctx, poolID, api.CreateClusterRequest{
		Name: "verifier-cluster", WithWriter: &withWriter, Password: strptr(rfc7677Verifier),
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if cl.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateCluster status = %d, want 202; body = %s", cl.StatusCode(), cl.Body)
	}
	drainTasks(t, store, backend, prov)

	if len(dep.created) != 2 {
		t.Fatalf("deployer saw %d specs, want 2", len(dep.created))
	}
	for _, spec := range dep.created {
		if spec.PasswordVerifier != rfc7677Verifier {
			t.Errorf("spec.PasswordVerifier = %q, want the caller's verifier unchanged", spec.PasswordVerifier)
		}
	}
}

// localLikeDeployer is a fakeDeployer that, like deploy.LocalDeployer, places
// computes whose supervisor never applies a password, so the specs it is handed
// can be inspected.
type localLikeDeployer struct{ *fakeDeployer }

func (localLikeDeployer) AppliesComputePassword() bool { return false }

// On the local driver nothing applies a password, so none is required anywhere,
// and one sent with a writer is dropped — on createCluster as on createEndpoint.
func TestLocalDriverDropsTheWriterPassword(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: localLikeDeployer{dep}}
	backend := core.NewMemBackend()
	client, store, logs := newLoggedTestServer(t, backend, prov)
	ctx := context.Background()
	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "local-pw-pool")
	withWriter := true

	resp, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name: "local", WithWriter: &withWriter, Password: strptr(testPassword),
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateCluster status = %d, want 202; body = %s", resp.StatusCode(), resp.Body)
	}

	// Roots with no floor, where every other deployer requires a password.
	withPw, withPwCluster, withPwRoot, _ := seedLineage(t, client, backend, "", "0/16B3748")
	ep, err := client.CreateEndpointWithResponse(ctx, withPw, withPwCluster, withPwRoot,
		api.CreateEndpointRequest{Password: strptr(rfc7677Verifier)})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if ep.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateEndpoint with a password: status = %d, want 202; body = %s", ep.StatusCode(), ep.Body)
	}
	noPw, noPwCluster, noPwRoot, _ := seedLineage(t, client, backend, "", "0/16B3748")
	ep, err = client.CreateEndpointWithResponse(ctx, noPw, noPwCluster, noPwRoot, api.CreateEndpointRequest{})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if ep.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateEndpoint with no password: status = %d, want 202; body = %s", ep.StatusCode(), ep.Body)
	}

	drainTasks(t, store, backend, prov)
	if len(dep.created) != 3 {
		t.Fatalf("deployer saw %d specs, want 3", len(dep.created))
	}
	for _, spec := range dep.created {
		if spec.PasswordVerifier != "" {
			t.Errorf("spec %s carries a verifier nothing will apply", spec.Name)
		}
	}
	if n := strings.Count(logs.String(), "ignoring a password"); n != 2 {
		t.Errorf("logged %d warnings for the 2 dropped passwords; log:\n%s", n, logs)
	}

	bad, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name: "local-bad", WithWriter: &withWriter, Password: strptr("short"),
	})
	if err != nil {
		t.Fatalf("CreateCluster(bad): %v", err)
	}
	if bad.StatusCode() != http.StatusBadRequest {
		t.Errorf("a malformed password on the local driver: status = %d, want 400; body = %s", bad.StatusCode(), bad.Body)
	}
}

// password_required is reported on every branch and cluster read, and a root's
// floor never shows as fork_lsn.
func TestPasswordRequiredAndForkLSNProjection(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		prov      api.Provisioning
		rootFloor string
		wantRoot  bool
	}{
		"a root with no floor":                 {api.Provisioning{Deployer: &fakeDeployer{}}, "", true},
		"a root with a floor":                  {api.Provisioning{Deployer: &fakeDeployer{}}, "0/2000000", false},
		"a root with no floor, local deployer": {api.Provisioning{Deployer: deploy.LocalDeployer{}}, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			backend := core.NewMemBackend()
			client, _, _ := newTestServerWith(t, backend, tc.prov)
			poolID, clusterID, rootID, forkID := seedLineage(t, client, backend, tc.rootFloor, "0/16B3748")

			root, err := client.GetBranchWithResponse(ctx, poolID, clusterID, rootID)
			if err != nil || root.JSON200 == nil {
				t.Fatalf("GetBranch(root): %v, body = %s", err, root.Body)
			}
			if got := root.JSON200.PasswordRequired; got == nil || *got != tc.wantRoot {
				t.Errorf("root password_required = %v, want %v", deref(got), tc.wantRoot)
			}
			if root.JSON200.ForkLsn != nil {
				t.Errorf("root fork_lsn = %q, want absent: a root's floor is not reported", *root.JSON200.ForkLsn)
			}
			if strings.Contains(string(root.Body), "fork_lsn") {
				t.Errorf("root body names fork_lsn: %s", root.Body)
			}

			fork, err := client.GetBranchWithResponse(ctx, poolID, clusterID, forkID)
			if err != nil || fork.JSON200 == nil {
				t.Fatalf("GetBranch(fork): %v, body = %s", err, fork.Body)
			}
			if got := fork.JSON200.PasswordRequired; got == nil || *got {
				t.Errorf("fork password_required = %v, want false", deref(got))
			}
			if got := fork.JSON200.ForkLsn; got == nil || *got != "0/16B3748" {
				t.Errorf("fork fork_lsn = %v, want 0/16B3748", got)
			}

			cl, err := client.GetClusterWithResponse(ctx, poolID, clusterID)
			if err != nil || cl.JSON200 == nil {
				t.Fatalf("GetCluster: %v, body = %s", err, cl.Body)
			}
			if got := cl.JSON200.PasswordRequired; got == nil || *got != tc.wantRoot {
				t.Errorf("cluster password_required = %v, want %v", deref(got), tc.wantRoot)
			}
			if got := cl.JSON200.DefaultBranch.PasswordRequired; got == nil || *got != tc.wantRoot {
				t.Errorf("default_branch password_required = %v, want %v", deref(got), tc.wantRoot)
			}

			list, err := client.ListBranchesWithResponse(ctx, poolID, clusterID, nil)
			if err != nil || list.JSON200 == nil {
				t.Fatalf("ListBranches: %v, body = %s", err, list.Body)
			}
			for _, b := range list.JSON200.Branches {
				if b.PasswordRequired == nil {
					t.Errorf("listed branch %s has no password_required", b.BranchId)
				}
				if b.BranchId == rootID && b.ForkLsn != nil {
					t.Errorf("listed root fork_lsn = %q, want absent", *b.ForkLsn)
				}
			}
			clusters, err := client.ListClustersWithResponse(ctx, poolID, nil)
			if err != nil || clusters.JSON200 == nil || len(clusters.JSON200.Clusters) != 1 {
				t.Fatalf("ListClusters: %v, body = %s", err, clusters.Body)
			}
			if got := clusters.JSON200.Clusters[0].PasswordRequired; got == nil || *got != tc.wantRoot {
				t.Errorf("listed cluster password_required = %v, want %v", deref(got), tc.wantRoot)
			}
		})
	}
}

func deref(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

// A fork below its parent's floor is refused before any task is planned, with
// the LSNs compared as numbers rather than strings.
func TestCreateBranchRefusesAForkBelowTheFloor(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		rootFloor, forkFloor string // the seeded root's and fork's fork_lsn
		parentIsFork         bool
		at                   string
		wantStatus           int
		wantMsg              []string
	}{
		"below a fork's floor": {
			rootFloor: "0/0", forkFloor: "0/200", parentIsFork: true, at: "0/100",
			wantStatus: http.StatusBadRequest, wantMsg: []string{"0/100", "0/200"},
		},
		"at a fork's floor": {
			rootFloor: "0/0", forkFloor: "0/200", parentIsFork: true, at: "0/200",
			wantStatus: http.StatusAccepted,
		},
		// "0/FF" sorts after "0/100" as a string.
		"below by number, above by string": {
			rootFloor: "0/0", forkFloor: "0/100", parentIsFork: true, at: "0/FF",
			wantStatus: http.StatusBadRequest, wantMsg: []string{"0/FF", "0/100"},
		},
		"above by number, below by string": {
			rootFloor: "0/0", forkFloor: "0/FF", parentIsFork: true, at: "0/100",
			wantStatus: http.StatusAccepted,
		},
		"the high half counts": {
			rootFloor: "0/0", forkFloor: "1/0", parentIsFork: true, at: "0/FFFFFFFF",
			wantStatus: http.StatusBadRequest, wantMsg: []string{"0/FFFFFFFF", "1/0"},
		},
		"below a root's floor": {
			rootFloor: "0/3000000", forkFloor: "0/3000000", at: "0/2000000",
			wantStatus: http.StatusBadRequest, wantMsg: []string{"0/2000000", "0/3000000", "password"},
		},
		"a root with no floor": {
			rootFloor: "", forkFloor: "0/2000000", at: "0/1",
			wantStatus: http.StatusAccepted,
		},
	} {
		t.Run(name, func(t *testing.T) {
			backend := core.NewMemBackend()
			client, _, _ := newTestServerWith(t, backend, api.Provisioning{Deployer: &fakeDeployer{}})
			poolID, clusterID, rootID, forkID := seedLineage(t, client, backend, tc.rootFloor, tc.forkFloor)
			parent := rootID
			if tc.parentIsFork {
				parent = forkID
			}
			resp, err := client.CreateBranchWithResponse(ctx, poolID, clusterID, api.CreateBranchRequest{
				Name: "child", ParentBranchId: &parent, ForkPointLsn: strptr(tc.at),
			})
			if err != nil {
				t.Fatalf("CreateBranch: %v", err)
			}
			if resp.StatusCode() != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode(), tc.wantStatus, resp.Body)
			}
			for _, want := range tc.wantMsg {
				if !strings.Contains(string(resp.Body), want) {
					t.Errorf("body %s does not mention %q", resp.Body, want)
				}
			}
		})
	}
}

// backfillingBackend is a MemBackend whose one-root backfill finds a writer, as
// PgBackend's does for one an older controller created.
type backfillingBackend struct {
	*core.MemBackend
	calls int
}

func (b *backfillingBackend) BackfillRootForkLSNOf(ctx context.Context, branchID uuid.UUID) (bool, error) {
	b.calls++
	return true, b.SetRootForkLSN(ctx, branchID, "0/0")
}

// A root with no floor has no password yet, so a fork of it would have none and,
// as a fork, never be asked for one: 503, so a caller retries once the floor is
// recorded, and nothing is planned.
func TestCreateBranchRefusesAForkOfARootWithNoPassword(t *testing.T) {
	ctx := context.Background()
	recording := func(dep deploy.Deployer) api.Provisioning {
		return api.Provisioning{Deployer: dep, Node: node.NewClient(), ControllerURL: "http://sc:50051"}
	}
	for name, tc := range map[string]struct {
		prov       api.Provisioning
		rootFloor  string
		backfills  bool
		wantStatus int
	}{
		"no floor":                       {recording(&fakeDeployer{}), "", false, http.StatusServiceUnavailable},
		"no floor, one the backfill set": {recording(&fakeDeployer{}), "", true, http.StatusAccepted},
		"a floor":                        {recording(&fakeDeployer{}), "0/0", false, http.StatusAccepted},
		// Without these the step never records a floor, so none is waited for.
		"no floor, local driver":      {recording(localLikeDeployer{&fakeDeployer{}}), "", false, http.StatusAccepted},
		"no floor, no controller URL": {api.Provisioning{Deployer: &fakeDeployer{}, Node: node.NewClient()}, "", false, http.StatusAccepted},
	} {
		t.Run(name, func(t *testing.T) {
			mem := core.NewMemBackend()
			var backend core.Backend = mem
			bf := &backfillingBackend{MemBackend: mem}
			if tc.backfills {
				backend = bf
			}
			client, tasks := newTestServerBackend(t, backend, tc.prov)
			// Through mem, which is what both backends store in.
			poolID, clusterID, rootID, _ := seedLineage(t, client, mem, tc.rootFloor, "0/2000000")
			resp, err := client.CreateBranchWithResponse(ctx, poolID, clusterID, api.CreateBranchRequest{
				Name: "child", ParentBranchId: &rootID, ForkPointLsn: strptr("0/3000000"),
			})
			if err != nil {
				t.Fatalf("CreateBranch: %v", err)
			}
			if resp.StatusCode() != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode(), tc.wantStatus, resp.Body)
			}
			if tc.backfills && bf.calls != 1 {
				t.Errorf("the one-root backfill ran %d times, want once", bf.calls)
			}
			if tc.wantStatus != http.StatusServiceUnavailable {
				return
			}
			if resp.HTTPResponse.Header.Get("Retry-After") == "" {
				t.Error("503 carries no Retry-After")
			}
			if !strings.Contains(string(resp.Body), "no password") {
				t.Errorf("body %s does not say the parent has no password", resp.Body)
			}
			if got, _, _ := tasks.List(ctx, task.TaskFilter{}, 100, 0); len(got) != 0 {
				t.Errorf("%d tasks planned for a refused fork, want none", len(got))
			}
		})
	}
}
