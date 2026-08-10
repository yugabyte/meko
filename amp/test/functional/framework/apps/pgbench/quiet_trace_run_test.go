package pgbench_test

import (
	"context"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/pgbench"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// BOTH pgbench invocations must drop ysql_bench's per-statement trace, not just
// one. The first attempt at this reached only Init, so the MEASURED run kept
// flooding: its attached log was 10,486,211 bytes — the capture cap — of
// "ysql_bench: client 5 receiving", with the `-P` progress lines lost off the end.
func TestBothInitAndRunSuppressTheTrace(t *testing.T) {
	ex := (&scriptExec{}).
		on("command -v pgbench", exec.Result{Stdout: "/usr/bin/pgbench\n"}).
		on("pgbench", exec.Result{Stdout: "tps = 100.0\nlatency average = 5.000 ms\n"})
	conn := pg.ConnInfo{Host: "h", Port: 5433, User: "u", Password: "p", Database: "d"}

	if err := pgbench.Init(context.Background(), ex, pgbench.InitOptions{Conn: conn, Scale: 1}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := pgbench.Run(context.Background(), ex, pgbench.RunOptions{Conn: conn, Clients: 2}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var invocations []string
	for _, c := range ex.commands {
		if strings.Contains(c, "command -v pgbench") {
			continue // the availability probe, not a pgbench invocation
		}
		invocations = append(invocations, c)
	}
	if len(invocations) != 2 {
		t.Fatalf("ran %d pgbench commands, want 2 (init + run): %v", len(invocations), invocations)
	}
	for i, c := range invocations {
		if !strings.Contains(c, "grep -vE '^ysql_bench: client [0-9]+ '") {
			t.Errorf("pgbench command %d is unfiltered, so its log will be megabytes of client trace:\n%s", i, c)
		}
		if !strings.Contains(c, "exit $rc") {
			t.Errorf("pgbench command %d loses its exit code through the filter:\n%s", i, c)
		}
	}
}
