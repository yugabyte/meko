package db

import (
	"errors"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Unavailable reports whether err is the state store being unreachable or going
// away — a connection attempt that never landed, a connection that died under an
// in-flight statement, or a server that says it is shutting down — rather than a
// statement the server rejected on its merits.
//
// It is the per-request form of the line Healthy draws for /readyz, so a caller
// gets a retryable answer during a tserver restart instead of one that reads as
// a controller bug (#886).
func Unavailable(err error) bool {
	if err == nil {
		return false
	}

	// pgx's own guarantee that the statement never reached the server: a
	// connection the pool handed over already closed, or a locked conn.
	if pgconn.SafeToRetry(err) {
		return true
	}

	// A network-level failure with no SQLSTATE: a dial refused, a host that does
	// not resolve, a dial that timed out (all of which pgx also wraps in a
	// pgconn.ConnectError — see SafeToRetry), or a reset or truncated read under
	// an in-flight statement. Matched on the concrete types rather than net.Error,
	// whose Timeout() also covers context.DeadlineExceeded — a slow query or a
	// disconnected client is not the store being down.
	//
	// Nothing here proves the error came out of pgx, so apply Unavailable only to
	// an error from a store call. On the API's read paths the store is the only
	// thing they touch; a deployer or safekeeper that cannot be reached is
	// answered by its own path, not by an error escaping to the classifier.
	var opErr *net.OpError
	var dnsErr *net.DNSError
	if errors.As(err, &opErr) || errors.As(err, &dnsErr) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	// The server answered with a SQLSTATE. Every class 08 code is about the
	// connection being broken or unestablishable, so the prefix is safe there.
	// Class 57 is not: it also carries query_canceled (57014) and
	// idle_session_timeout (57P05), which are a statement being cut short against
	// a healthy store — the same thing the type match above declines to treat as
	// an outage — so the shutdown codes are enumerated. 53300 is the store
	// refusing new work. Anything else — syntax, constraint, permission, bad
	// password — is a verdict on the statement, and stays the controller's problem.
	// The pool gave up before it ever reached the server. pgxpool's Acquire
	// retries MaxConns+1 times, destroying each candidate whose liveness ping
	// fails, and then returns a bare errors.New — no SQLSTATE, no net.OpError, no
	// SafeToRetry, so every arm above declines it and the request would land as a
	// 500 (#886's exact failure). Bounding PingTimeout is what puts this within a
	// caller's budget: unbounded, the first ping blocked until the caller's
	// context died and the caller saw that instead.
	//
	// Matched on the message because pgxpool exports no sentinel for it
	// (pgxpool/pool.go:665, pgx v5.10.0). A wording change on a version bump would
	// silently drop the match, which is what TestUnavailablePoolExhaustion pins.
	if strings.Contains(err.Error(), "too many failed attempts acquiring connection") {
		return true
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "57P01", "57P02", "57P03", "53300": // admin/crash shutdown, cannot_connect_now, too_many_connections
			return true
		}
		return strings.HasPrefix(pgErr.Code, "08")
	}

	return false
}

// ErrOutcomeUnknown marks an error that arrived after a write in the same
// request may already have become durable — see AfterWrite. It is what keeps
// SafeToRetry from reading a connection failure as proof that nothing happened,
// when the proof only covers the statement the failure came from.
var ErrOutcomeUnknown = errors.New("a write in this request may already have landed")

// AfterWrite marks err as following a durable write, so no caller replays the
// request on the strength of its error type alone. Apply it where the write has
// provably succeeded and a later statement failed — a commit that returned, or
// the read-back after an insert. Applying it to the write's *own* error would be
// wrong: that one really did leave nothing behind.
func AfterWrite(err error) error {
	if err == nil {
		return nil
	}
	return errors.Join(err, ErrOutcomeUnknown)
}

// SafeToRetry reports whether err is an Unavailable that provably occurred
// before the statement reached the server *and* nothing else in the request had
// already written, so replaying it cannot double-apply a write. A connection
// that died mid-statement is deliberately not safe: the statement may have
// committed before the connection dropped.
func SafeToRetry(err error) bool {
	if !Unavailable(err) || errors.Is(err, ErrOutcomeUnknown) {
		return false
	}
	if pgconn.SafeToRetry(err) {
		return true
	}
	// The whole connection attempt failed, so nothing was ever sent on it.
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return true
	}
	// The server refused to start work: cannot_connect_now (still starting up),
	// too_many_connections.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "57P03" || pgErr.Code == "53300"
	}
	return false
}
