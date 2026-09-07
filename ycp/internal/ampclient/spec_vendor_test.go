package ampclient

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// controllerSpec is the spec this package's client is generated from. ycp and
// amp/controller are separate Go modules, so the dependency has to be a file
// path rather than an import.
const controllerSpec = "../../../amp/controller/internal/api/openapi_spec.yaml"

// TestVendoredSpecMatchesController fails when the vendored spec has fallen
// behind amp/controller's.
//
// Nothing else catches this. The generated client is built from the vendored
// copy, so a stale copy is entirely self-consistent: it compiles, vets and
// passes every behavioural test, and the only symptom is that the agent cannot
// express a field the controller has grown. There is no assertion to write
// about a capability you do not know exists — which is why the copy sat two
// features behind with CI green. Comparing the two artifacts directly is the
// only check that works.
func TestVendoredSpecMatchesController(t *testing.T) {
	// Deliberately fatal rather than skipped when the controller tree is
	// absent: a skip here would restore exactly the silence this test exists
	// to remove. Tests only ever run from the monorepo (the image builds use a
	// ycp-only context and just `go build`), so this cannot fire in CI.
	want, err := os.ReadFile(controllerSpec)
	if err != nil {
		t.Fatalf("cannot read the controller's spec at %s: %v\n"+
			"This test compares the two files directly, so it needs the amp tree. "+
			"If ycp has been split out of the monorepo, the vendoring contract "+
			"needs rethinking, not this read relaxing.", controllerSpec, err)
	}

	got, err := os.ReadFile("openapi_spec.yaml")
	if err != nil {
		t.Fatalf("cannot read the vendored spec: %v", err)
	}

	if bytes.Equal(got, want) {
		return
	}

	gotLines := strings.Split(string(got), "\n")
	wantLines := strings.Split(string(want), "\n")
	t.Errorf("the vendored amp spec is out of date (%d lines vendored vs %d in the controller's).\n"+
		"Re-sync it and the generated client with ./sync-spec.sh, then commit both.\n"+
		"First difference at line %d:\n  vendored:   %s\n  controller: %s",
		len(gotLines), len(wantLines), firstDiff(gotLines, wantLines),
		line(gotLines, firstDiff(gotLines, wantLines)),
		line(wantLines, firstDiff(gotLines, wantLines)))
}

// firstDiff returns the 1-based number of the first line that differs.
func firstDiff(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i + 1
		}
	}
	return min(len(a), len(b)) + 1
}

// line returns the 1-based nth line, or a marker when the file ends first.
func line(lines []string, n int) string {
	if n < 1 || n > len(lines) {
		return "<end of file>"
	}
	return lines[n-1]
}
