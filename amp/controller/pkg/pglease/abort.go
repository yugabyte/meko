package pglease

import (
	"fmt"
	"os"
	"runtime"
)

// AbortExitCode is the status AbortProcess exits with. It matches the compute
// fence's exit code, so one log scan or supervisor rule recognises "fenced out"
// across both implementations, and it is distinct from 1 (a test's t.Fatal) and
// 2 (a panic or runtime crash), so a test asserting on it cannot pass by
// accident.
const AbortExitCode = 66

// AbortFunc ends the process. It must not return: a Client whose abort returns
// keeps running with a lease it does not own, which is the failure this package
// exists to prevent. Tests inject one that records.
type AbortFunc func(key string, cause error)

// AbortProcess is the default AbortFunc. It writes the cause and a goroutine
// dump straight to stderr — not through a logger, which may batch or ship
// asynchronously, and this process has microseconds to live — then exits with
// AbortExitCode.
//
// os.Exit, deliberately, rather than a panic or a signal: it skips deferred
// functions (a defer running after the lease is lost is cleanup performed
// without ownership), cannot be recovered by the very code being fenced off,
// and yields a deterministic status. Death by SIGABRT does not: the Go runtime
// catches it and turns it into exit status 2 unless GOTRACEBACK=crash.
func AbortProcess(key string, cause error) {
	fmt.Fprintf(os.Stderr, "FATAL: pglease: lease %q lost: %v\n", key, cause)
	// Every goroutine, not just this one: the renewal goroutine's own stack says
	// nothing about the workers still writing under the lease it just lost.
	buf := make([]byte, 1<<20)
	_, _ = os.Stderr.Write(buf[:runtime.Stack(buf, true)])
	os.Exit(AbortExitCode)
}
