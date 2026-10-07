package foundation

import (
	"os/exec"
	"strings"
	"testing"
)

// TestShellExportsSurvivesEval pins the contract with amp/deploy/up-gke.sh,
// which consumes this output as `eval "$(foundation connstr --shell)"`.
//
// A Go-side assertion that the string merely CONTAINS "export YB_HOST_PORTS="
// would not catch what actually breaks here: an address or DSN that is not
// shell-safe produces a line that evals to the wrong value, or to nothing, and
// the script then deploys the controller against an empty DB_URL. So run the
// output through a real shell and read the variables back.
func TestShellExportsSurvivesEval(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	info := &Info{
		YSQLHostPorts: "foundation-1954916679-yb-tserver-conn.amp.svc.cluster.local:5433",
		RPCHostPorts:  "foundation-1954916679-yb-tserver-conn.amp.svc.cluster.local:9100",
		DBURL: "postgres://yugabyte@foundation-1954916679-yb-tserver-conn.amp.svc" +
			".cluster.local:5433/yb_storage?sslmode=disable",
	}

	// set -u so an export the script relies on but this never emitted fails
	// loudly rather than expanding to empty, which is exactly how up-gke.sh
	// runs.
	script := "set -euo pipefail\n" + info.ShellExports() +
		"\nprintf '%s\\n%s\\n' \"$YB_HOST_PORTS\" \"$DB_URL\"\n"

	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("eval of ShellExports failed: %v\n%s\n--- script ---\n%s", err, out, script)
	}
	got := strings.Split(strings.TrimSpace(string(out)), "\n")
	want := []string{info.YSQLHostPorts, info.DBURL}
	if len(got) != len(want) {
		t.Fatalf("read back %d values, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("value %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// An Info the deployer could not build a DSN for has no DBURL, and up-gke.sh
// checks for it — so the export must be absent rather than present and empty,
// which would pass the script's -n check with a broken value.
func TestShellExportsOmitsAnEmptyDBURL(t *testing.T) {
	info := &Info{YSQLHostPorts: "h:5433", RPCHostPorts: "r:9100"}
	if strings.Contains(info.ShellExports(), "DB_URL") {
		t.Errorf("an empty DBURL was still exported:\n%s", info.ShellExports())
	}
}
