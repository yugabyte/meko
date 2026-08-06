package main

import "testing"

// Nothing listens on port 1 of the loopback, so a connect there fails fast. What
// these tests exercise is the retry and caching decisions, not pgx.
const unreachableDSN = "postgres://u:p@127.0.0.1:1/postgres?connect_timeout=1"

// The data-plane database is declared as desired state, so on the tick that creates
// it the StatefulSet is still initialising and the first connect always fails. The
// pool must keep trying: latching on that first failure leaves the workloads that
// depend on it unconfigured forever, next to a database the fleet view calls healthy.
func TestDBPoolRetriesAfterFailure(t *testing.T) {
	p := &dbPool{}

	if got := p.use(unreachableDSN); got != nil {
		t.Fatal("use() returned a pool for a DSN nothing listens on")
	}
	if !p.warned[unreachableDSN] {
		t.Error("the first failure should have logged (warned)")
	}
	// A failure must not be cached as a result: the next tick has to attempt again.
	if _, cached := p.conns[unreachableDSN]; cached {
		t.Error("a failed connect must not leave an entry in the connection cache")
	}
	if got := p.use(unreachableDSN); got != nil {
		t.Fatal("use() returned a pool on retry")
	}
}

// An empty DSN means no database is declared: no pool, and no error either — the
// workloads that need one report why, and the next tick tries again.
func TestDBPoolEmptyDSN(t *testing.T) {
	p := &dbPool{}
	if got := p.use(""); got != nil {
		t.Fatal("use(\"\") must not produce a pool")
	}
	if p.warned[""] {
		t.Error("an absent DSN is not a failure to warn about")
	}
}

// The cache is keyed, not a single slot: an environment can declare more than one
// database, and a single slot would tear the connection down and rebuild it on every
// tick, alternating between them.
func TestDBPoolKeepsPerDSNState(t *testing.T) {
	p := &dbPool{}
	other := "postgres://u:p@127.0.0.1:1/other?connect_timeout=1"

	p.use(unreachableDSN)
	p.use(other)

	if !p.warned[unreachableDSN] || !p.warned[other] {
		t.Errorf("each DSN warns for itself: %v", p.warned)
	}
	if len(p.conns) != 0 {
		t.Errorf("no connection should be cached when both failed: %v", p.conns)
	}
}

// close() has to be safe on a pool that never connected — the agent defers it at
// startup, before any database is declared.
func TestDBPoolCloseWithoutConnections(t *testing.T) {
	p := &dbPool{}
	p.close()
	p.use(unreachableDSN)
	p.close()
	if len(p.conns) != 0 {
		t.Errorf("close() left entries behind: %v", p.conns)
	}
}

// logOnce suppresses repetition, not the message. A YugabyteDB takes about ninety
// seconds to come up and the loop ticks every ten, so three reconcilers all waiting on
// it would print twenty-seven lines saying the same thing — and the transitions an
// operator is actually watching for would be somewhere in the middle of them.
func TestLogOnce(t *testing.T) {
	p := &dbPool{}

	// Same key, same message: said once. Same key, changed message: said again. The
	// point is that a state CHANGE is never swallowed.
	p.logOnce("mcp-server.mcp", "not accepting connections yet")
	if got := p.said["mcp-server.mcp"]; got != "not accepting connections yet" {
		t.Fatalf("said[mcp-server.mcp] = %q, want the first message recorded", got)
	}
	p.logOnce("mcp-server.mcp", "not accepting connections yet")
	p.logOnce("mcp-server.mcp", "permission denied for database meko")
	if got := p.said["mcp-server.mcp"]; got != "permission denied for database meko" {
		t.Errorf("said[mcp-server.mcp] = %q, want the changed message recorded", got)
	}

	// Keyed per caller, so one reconciler going quiet does not silence another.
	p.logOnce("rag-worker.rag", "not accepting connections yet")
	if len(p.said) != 2 {
		t.Errorf("said = %v, want one entry per caller", p.said)
	}
}
