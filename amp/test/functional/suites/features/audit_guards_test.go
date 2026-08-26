package features

import (
	"os"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// pg_read_cold_cache asserted a cold Local File Cache it never had. The LFC is
// instance-scoped shared memory (compute/pgxn/yb_amp/file_cache.c), so the old
// reconnect was served by the cache the writer had just filled, and the
// Pageserver read path this test exists for never ran. Two things keep that
// from coming back: the compute is restarted, and the miss counter proves the
// reads were actually cold.
func TestColdCacheTestIsActuallyCold(t *testing.T) {
	body := readSource(t, "regression.go")
	i := strings.Index(body, "func runReadColdCache(")
	if i < 0 {
		t.Fatal("runReadColdCache is gone")
	}
	fn := body[i:]
	if j := strings.Index(fn[1:], "\nfunc "); j >= 0 {
		fn = fn[:j+1]
	}
	if !strings.Contains(fn, "env.RestartCompute(") {
		t.Error("the cold read no longer restarts the compute; a reconnect leaves the instance-scoped LFC full and the test proves nothing")
	}
	if !strings.Contains(fn, "lfcMisses(") {
		t.Error("the cold read no longer checks the LFC miss counter, so a cached read would pass as a cold one")
	}
	if !strings.Contains(fn, "missesAfter <= missesBefore") {
		t.Error("the miss counter is read but not asserted on")
	}
	if !strings.Contains(fn, "computeRestarter") {
		t.Error("the test must require the restart capability rather than any pgEnv")
	}
}

// role never reaches EndpointSpec, so a read_only pod boots as a primary and
// queues forever for its branch's write token (meko#833). Waiting on one is a
// guaranteed timeout; planning one without waiting is fine.
func TestNoTestWaitsOnAReadOnlyCompute(t *testing.T) {
	for _, name := range sourceFiles(t) {
		for n, line := range strings.Split(readSource(t, name), "\n") {
			if !strings.Contains(line, "roleReadOnly") {
				continue
			}
			if strings.Contains(line, "createRoleEndpoint(") || strings.Contains(line, "CreateEndpointAndWait(") {
				t.Errorf("%s:%d waits for a read_only endpoint's compute, which cannot come ready (meko#833): %s", name, n+1, strings.TrimSpace(line))
			}
		}
	}
}

// The one exemption from the guard above, and the price of it: the raced create
// can win and own a task that never settles, so both waits need a deadline.
func TestDropRaceBoundsItsWaitsOnAReadOnlyEndpoint(t *testing.T) {
	body := readSource(t, "amp_endpoint_drop_race.go")
	if !strings.Contains(body, `joinRaceTaskWithin(ctx, cl, createTask, "create_endpoint", dropRaceCreateJoinBudget`) {
		t.Error("the raced create is joined without a deadline again; its task never settles, so the join takes the rest of the test's budget")
	}
	if !strings.Contains(body, "cleanupCtx, cancel := context.WithTimeout(ctx, dropRaceCleanupBudget)") {
		t.Error("dropRaceCleanup retires the read_only endpoint without a deadline again")
	}
}

func sourceFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read suite directory: %v", err)
	}
	var names []string
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			names = append(names, n)
		}
	}
	return names
}

// The externalized extension catalog is a shipping commitment, and with the
// opt-in off the exact field incident this test exists for (btree_gin missing
// from the catalog) passes.
func TestCatalogHalfIsRequiredByDefault(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Skipf("defaults not loadable here: %v", err)
	}
	if !cfg.GetBool("functional.extensions.require_catalog", false) {
		t.Error("functional.extensions.require_catalog defaults off: the catalog half of pg_extensions_matrix asserts nothing")
	}
}

func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// Dialling the controller-reported endpoint address does not refuse on a
// cluster whose pod range is unroutable from the coordinator — it times out,
// and every self-provisioned test burned its whole readiness budget before
// failing. The connection goes through a port-forward whenever kubectl is
// available.
func TestSelfProvisionedEndpointIsReachedThroughAForward(t *testing.T) {
	body := readSource(t, "amp_branch_isolation.go")
	i := strings.Index(body, "func (f *flow) endpointSQL(")
	if i < 0 {
		t.Fatal("endpointSQL is gone")
	}
	fn := body[i:]
	if !strings.Contains(fn, "kube.PortForward(") {
		t.Error("the endpoint is dialled at its reported address again; on an unroutable pod range that times out rather than refusing")
	}
	if !strings.Contains(fn, "WaitEndpointWorkloadReady(") {
		t.Error("the forward no longer waits for the compute: kubectl port-forward exits at once when the Service has no ready pod")
	}
	if !strings.Contains(fn, "forwardedConn{") {
		t.Error("the forward is no longer tied to the connection, so it outlives the endpoint it points at")
	}
}
