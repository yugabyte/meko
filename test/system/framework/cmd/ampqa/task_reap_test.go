package main

import (
	"strings"
	"testing"

	"github.com/yugabyte/yb-amp/test/system/framework/task"
)

// TestReapTaskReachableFromCLI guards that the registry-TTL reap task is wired
// into the CLI (cmd/ampqa blank-imports task/tasks). TTL enforcement has no
// automatic sweeper by design, so if this task were ever unregistered, expired
// standing envs would leak silently with no reachable way to reclaim them.
func TestReapTaskReachableFromCLI(t *testing.T) {
	rt, ok := task.Default().Get("reap")
	if !ok {
		t.Fatal("reap task not registered — TTL enforcement is unreachable via `ampqa task reap`")
	}
	// The doc must state the scheduling contract so operators know nothing sweeps
	// expired envs on its own.
	if !strings.Contains(strings.ToLower(rt.Doc), "schedule") {
		t.Errorf("reap task doc must document the scheduling contract, got: %q", rt.Doc)
	}
}
