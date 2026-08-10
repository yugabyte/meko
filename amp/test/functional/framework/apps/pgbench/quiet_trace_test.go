package pgbench

import (
	"strings"
	"testing"
)

// The per-statement client trace is dropped before capture; everything else,
// including pgbench's exit code and any real error, survives.
func TestQuietTraceDropsClientNoiseAndKeepsExitCode(t *testing.T) {
	cmd := quietTrace("pgbench -c 8")
	if !strings.Contains(cmd, "exit $rc") {
		t.Error("pgbench's exit code is not preserved through the filter")
	}
	if !strings.Contains(cmd, "^ysql_bench: client [0-9]+ ") {
		t.Error("the filter does not anchor on the client-trace prefix, so it could eat real output")
	}
	if strings.Contains(cmd, "grep -E") {
		t.Error("the filter must be a denylist; an allowlist would discard real errors with the noise")
	}
}
