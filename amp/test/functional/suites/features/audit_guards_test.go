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

// Skipping on ANY error let read_only resize coverage disappear with nothing
// red: a 500, a timeout or a dead transport read exactly like "the role is not
// plumbed yet". Only a terminal 4xx is the product saying it does not support
// this.
func TestReadOnlyResizeSkipsOnlyOnARefusal(t *testing.T) {
	body := readSource(t, "amp_endpoint_resize.go")
	i := strings.Index(body, "func (a *ampFeatureRun) endpointResizeReadOnly(")
	if i < 0 {
		t.Fatal("endpointResizeReadOnly is gone")
	}
	fn := body[i : i+1600]
	if !strings.Contains(fn, "apiErr.StatusCode >= 400 && apiErr.StatusCode < 500") {
		t.Error("the skip is not restricted to a terminal 4xx refusal")
	}
	if !strings.Contains(fn, "return fmt.Errorf(\"create %s endpoint to resize") {
		t.Error("a non-refusal error no longer fails the test")
	}
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
