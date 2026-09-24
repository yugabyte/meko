package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
)

const testPassword = "hunter2hunter2"

// seedBranchForCreds stands up a pool + branch an endpoint can be created on.
func seedBranchForCreds(t *testing.T, client *api.ClientWithResponses, backend *core.MemBackend, poolName string) (poolID, clusterID, branchID uuid.UUID) {
	t.Helper()
	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, poolName)
	clusterID, branchID = uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{
		ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true,
	})
	return pool.Id, clusterID, branchID
}

func strptr(s string) *string { return &s }

// isSCRAMVerifier reports whether s has the shape Postgres stores verbatim.
// Deliberately not a recomputation: SCRAM salts randomly, so no caller can
// predict the exact string, and the shape is the part this package owns. The
// full parse lives in core's own tests.
func isSCRAMVerifier(s string) bool {
	rest, ok := strings.CutPrefix(s, "SCRAM-SHA-256$")
	if !ok {
		return false
	}
	params, keys, ok := strings.Cut(rest, "$")
	if !ok {
		return false
	}
	return strings.Contains(params, ":") && strings.Contains(keys, ":")
}

// The writer comes up with the caller's password on the compute's default user.
// What reaches the deployer is the verifier for exactly that password — computed
// over the right user.
func TestCreateEndpointUsesTheCallersPassword(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	poolID, clusterID, branchID := seedBranchForCreds(t, client, backend, "creds-pool")

	created, err := client.CreateEndpointWithResponse(ctx, poolID, clusterID, branchID,
		api.CreateEndpointRequest{Password: strptr(testPassword)})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if created.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", created.StatusCode(), created.Body)
	}
	if strings.Contains(string(created.Body), testPassword) {
		t.Errorf("the password was echoed in the 202:\n%s", created.Body)
	}

	drainTasks(t, store, backend, prov)

	if len(dep.created) != 1 {
		t.Fatalf("deployer saw %d specs, want 1", len(dep.created))
	}
	spec := dep.created[0]
	// The verifier cannot be recomputed and compared: SCRAM salts randomly, so
	// deriving one here would produce a different (equally valid) string. What is
	// checked is that a verifier of the right shape was sent and that the
	// plaintext did not travel. That it authenticates the password is proved
	// against a live Postgres, not here.
	if !isSCRAMVerifier(spec.PasswordVerifier) {
		t.Errorf("spec.PasswordVerifier = %q, want a SCRAM-SHA-256 verifier", spec.PasswordVerifier)
	}
	if strings.Contains(spec.PasswordVerifier, testPassword) {
		t.Errorf("the plaintext appears in what was sent to the deployer: %q", spec.PasswordVerifier)
	}

	// Nothing reports the password back — not the endpoint, not the task.
	list, err := client.ListEndpointsWithResponse(ctx, poolID, clusterID, branchID, nil)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if list.JSON200 == nil || len(list.JSON200.Endpoints) != 1 {
		t.Fatalf("ListEndpoints body = %s", list.Body)
	}
	if strings.Contains(string(list.Body), testPassword) {
		t.Errorf("the password appears in the endpoint representation:\n%s", list.Body)
	}
	got, err := client.GetTaskWithResponse(ctx, created.JSON202.Id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.JSON200 == nil {
		t.Fatalf("GetTask body = %s", got.Body)
	}
	if strings.Contains(string(got.Body), testPassword) {
		t.Errorf("the password appears in the task representation:\n%s", got.Body)
	}
}

// The controller mints no password. A writer create without one is refused up
// front rather than coming up with a credential its owner never receives.
func TestCreateEndpointRequiresAPassword(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, _ := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	poolID, clusterID, branchID := seedBranchForCreds(t, client, backend, "require-pool")

	rw := api.CreateEndpointRequestRoleReadWrite
	for _, body := range []api.CreateEndpointRequest{
		{},          // nothing at all
		{Role: &rw}, // an explicit writer
	} {
		resp, err := client.CreateEndpointWithResponse(ctx, poolID, clusterID, branchID, body)
		if err != nil {
			t.Fatalf("CreateEndpoint: %v", err)
		}
		if resp.StatusCode() != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 for a writer with no password; body = %s",
				resp.StatusCode(), resp.Body)
		}
	}
	if len(dep.created) != 0 {
		t.Errorf("a rejected request still reached the deployer %d time(s)", len(dep.created))
	}
}

// The password is validated before a task exists, so a bad one is a synchronous
// 400 rather than a deploy that comes up with an unusable credential.
func TestCreateEndpointRejectsABadPassword(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, _ := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	poolID, clusterID, branchID := seedBranchForCreds(t, client, backend, "reject-pool")

	cases := map[string]string{
		"under the minimum length":     "short",
		"carrying a control character": "newline\nin\nit",
	}
	for why, pw := range cases {
		resp, err := client.CreateEndpointWithResponse(ctx, poolID, clusterID, branchID,
			api.CreateEndpointRequest{Password: strptr(pw)})
		if err != nil {
			t.Fatalf("CreateEndpoint(%s): %v", why, err)
		}
		if resp.StatusCode() != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (%s); body = %s", resp.StatusCode(), why, resp.Body)
		}
	}
	if len(dep.created) != 0 {
		t.Errorf("a rejected request still reached the deployer %d time(s)", len(dep.created))
	}
}

// The one-call onboarding path credentials its writer exactly as createEndpoint
// does — including requiring a password — and refuses one for a writer it is not
// being asked to create.
func TestCreateClusterCredentialsTheWriter(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "writer-pool")
	withWriter := true

	noPw, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name: "nopw", WithWriter: &withWriter,
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if noPw.StatusCode() != http.StatusBadRequest {
		t.Errorf("with_writer and no password = %d, want 400; body = %s", noPw.StatusCode(), noPw.Body)
	}

	created, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name: "shop", WithWriter: &withWriter, Password: strptr(testPassword),
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if created.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", created.StatusCode(), created.Body)
	}

	drainTasks(t, store, backend, prov)

	if len(dep.created) != 1 {
		t.Fatalf("deployer saw %d specs, want 1 (the writer)", len(dep.created))
	}
	if got := dep.created[0].PasswordVerifier; !isSCRAMVerifier(got) {
		t.Errorf("writer spec.PasswordVerifier = %q, want a SCRAM-SHA-256 verifier", got)
	}

	// Without a writer there is no endpoint to credential, so the request is
	// refused rather than silently ignored — the same contract `connectivity` has.
	resp, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name: "no-writer", Password: strptr(testPassword),
	})
	if err != nil {
		t.Fatalf("CreateCluster(no-writer): %v", err)
	}
	if resp.StatusCode() != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a password with no writer; body = %s", resp.StatusCode(), resp.Body)
	}
}

// The constraint that makes this safe to ship onto a running fleet: a redeploy
// of an existing endpoint carries no verifier, so nothing resets the password
// its clients hold.
func TestRedeployDoesNotResetThePassword(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep, EndpointVersion: testEndpointVersion}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	poolID, clusterID, branchID := seedBranchForCreds(t, client, backend, "redeploy-pool")

	if _, err := client.CreateEndpointWithResponse(ctx, poolID, clusterID, branchID,
		api.CreateEndpointRequest{Password: strptr(testPassword)}); err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	drainTasks(t, store, backend, prov)

	rolled, err := client.UpgradeEndpointsWithResponse(ctx, api.UpgradeEndpointsRequest{})
	if err != nil {
		t.Fatalf("UpgradeEndpoints: %v", err)
	}
	if rolled.StatusCode() != http.StatusAccepted {
		t.Fatalf("UpgradeEndpoints status = %d, want 202; body = %s", rolled.StatusCode(), rolled.Body)
	}
	drainTasks(t, store, backend, prov)

	if len(dep.created) != 2 {
		t.Fatalf("deployer saw %d specs, want 2 (the create and the upgrade's redeploy)", len(dep.created))
	}
	if pw := dep.created[1].PasswordVerifier; pw != "" {
		t.Errorf("redeploy spec.PasswordVerifier = %q, want empty: nothing stores the verifier, so a redeploy could only re-password with a wrong one", pw)
	}
}

// The compute's default user lives in the branch's catalog, which every endpoint
// on the branch shares. So a read_only endpoint must NOT set a password: it would
// silently rotate the credential the writer's clients are already using. That is
// also why the requirement is writer-only.
func TestReadOnlyEndpointSetsNoPassword(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	poolID, clusterID, branchID := seedBranchForCreds(t, client, backend, "ro-pool")

	rw := api.CreateEndpointRequestRoleReadWrite
	ro := api.CreateEndpointRequestRoleReadOnly

	if _, err := client.CreateEndpointWithResponse(ctx, poolID, clusterID, branchID,
		api.CreateEndpointRequest{Role: &rw, Password: strptr(testPassword)}); err != nil {
		t.Fatalf("CreateEndpoint(read_write): %v", err)
	}
	drainTasks(t, store, backend, prov)

	// No password, and that is not an error: a replica sets none.
	reader, err := client.CreateEndpointWithResponse(ctx, poolID, clusterID, branchID,
		api.CreateEndpointRequest{Role: &ro})
	if err != nil {
		t.Fatalf("CreateEndpoint(read_only): %v", err)
	}
	if reader.StatusCode() != http.StatusAccepted {
		t.Fatalf("read_only status = %d, want 202; body = %s", reader.StatusCode(), reader.Body)
	}
	drainTasks(t, store, backend, prov)

	if len(dep.created) != 2 {
		t.Fatalf("deployer saw %d specs, want 2", len(dep.created))
	}
	if pw := dep.created[1].PasswordVerifier; pw != "" {
		t.Errorf("read_only spec.PasswordVerifier = %q, want empty: it would rotate the writer's credential", pw)
	}

	// And asking for one is refused rather than silently dropped.
	resp, err := client.CreateEndpointWithResponse(ctx, poolID, clusterID, branchID,
		api.CreateEndpointRequest{Role: &ro, Password: strptr(testPassword)})
	if err != nil {
		t.Fatalf("CreateEndpoint(read_only, password): %v", err)
	}
	if resp.StatusCode() != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a password on a read_only endpoint; body = %s",
			resp.StatusCode(), resp.Body)
	}
}

// An endpoint created before this shipped has no verifier in its stored config,
// and must deploy exactly as it did then: nothing asked for, no password touched.
func TestPreCredentialsEndpointDeploysUnchanged(t *testing.T) {
	spec := deploy.EndpointSpecFrom(core.EndpointDeploy{ID: uuid.New(), BranchID: uuid.New(), PgVersion: 18},
		deploy.YBConn{}, "", nil)
	if spec.PasswordVerifier != "" {
		t.Errorf("spec.PasswordVerifier = %q, want empty", spec.PasswordVerifier)
	}
}
