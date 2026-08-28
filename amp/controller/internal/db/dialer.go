package db

import (
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Socket-level bounds. These cover what the pool-level bounds cannot: a socket
// whose peer stopped existing. pgxpool can only notice a dead connection when
// something uses it, and then only after waiting; the kernel can notice on its
// own, and it notices every dead socket at once rather than one per caller.
const (
	// keepAlive probes an otherwise idle connection, so a peer that went away
	// with nothing in flight is discovered instead of sitting in the pool
	// looking usable. Go's default is the same 15s; naming it here records that
	// the recovery story depends on it.
	keepAlive = 15 * time.Second

	// tcpUserTimeout caps how long transmitted data may stay unacknowledged
	// before the kernel fails the socket (TCP_USER_TIMEOUT, Linux). Without it
	// a write to a departed pod retransmits for ~15 minutes, which is the
	// difference between "the pool recovers" and "the pool is wedged until
	// something restarts it".
	//
	// 3s is the value the compute side already runs in production for the same
	// option — endpoint_supervisor's registry and lease connections both set
	// it (TCP_USER_TIMEOUT_MS, LEASE_TCP_USER_TIMEOUT_MS). Matching it keeps one
	// number for one failure across both languages, and the bound does not need
	// to scale with how much work the connection does: see below.
	//
	// It does NOT bound slow queries, which is the usual worry: the timer only
	// runs while data is unacknowledged at the TCP layer. A server that ACKs
	// the request and then thinks for a minute has nothing outstanding, so the
	// timer is not running. Only a peer that has stopped acknowledging — dead,
	// or partitioned away — trips it.
	tcpUserTimeout = 3 * time.Second
)

// dialFuncWithTimeout builds the pool's dialer: pgx's own connect_timeout dialer
// (pgconn.makeConnectTimeoutDialFunc) is unexported, and it would not carry the
// socket options anyway, which no DSN setting can express.
func dialFuncWithTimeout(timeout time.Duration) pgconn.DialFunc {
	d := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: keepAlive,
		Control:   setSocketTimeouts,
	}
	return d.DialContext
}
