package pgbench

import (
	"errors"
	"os/exec"
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

// The client error that aborted a run must survive a flood of torn trace
// fragments after it; the bounded tail alone kept only the flood.
func TestQuietTraceKeepsTheErrorBehindATornFlood(t *testing.T) {
	fake := `sh -c 'echo "client 3 aborted in command 4 (SQL) of script 0; ERROR: boom" >&2; ` +
		`i=0; while [ $i -lt 3000 ]; do echo "ysql_bench:ysql_bench: client 1 receiving torn fragment" >&2; i=$((i+1)); done; ` +
		`echo "tps = 1.0"; exit 2'`
	cmd := exec.Command("sh", "-c", quietTrace(fake))
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("exit code lost: %v", err)
	}
	if !strings.Contains(stderr.String(), "ERROR: boom") {
		t.Error("the aborting client's error did not survive the flood")
	}
	if !strings.Contains(stdout.String(), "tps = 1.0") {
		t.Error("the summary did not survive")
	}
}
