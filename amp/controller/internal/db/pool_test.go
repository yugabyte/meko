package db

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const testDSN = "postgres://yugabyte@yb-tserver:5433/yb_storage?sslmode=disable"

// TestApplyPoolBoundsSetsRecoveryBounds pins every bound that decides how long
// the pool stays wedged on sockets to a departed backing pod (#961). pgx sets
// most of them, but for hygiene rather than for a vanished peer — 1h lifetime,
// 30m idle, 1m health check. PingTimeout is the one it leaves at zero, meaning
// "no timeout": one dead socket then consumed a caller's whole deadline, so the
// pool shed one connection per /readyz probe.
func TestApplyPoolBoundsSetsRecoveryBounds(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(testDSN)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Guard the premise, and be exact about it: only PingTimeout and
	// ConnectTimeout arrive at zero. The other three arrive slack, not absent,
	// which is the distinction that decides whether reverting to pgx's defaults
	// looks safe.
	if cfg.PingTimeout != 0 || cfg.ConnConfig.ConnectTimeout != 0 {
		t.Fatalf("expected pgx to default PingTimeout and ConnectTimeout to 0, got %v / %v",
			cfg.PingTimeout, cfg.ConnConfig.ConnectTimeout)
	}
	for _, d := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"MaxConnLifetime", cfg.MaxConnLifetime, time.Hour},
		{"MaxConnIdleTime", cfg.MaxConnIdleTime, 30 * time.Minute},
		{"HealthCheckPeriod", cfg.HealthCheckPeriod, time.Minute},
	} {
		if d.got != d.want {
			t.Fatalf("premise moved: pgx now defaults %s to %v, not %v — the comments say we tighten it, not that we introduce it",
				d.name, d.got, d.want)
		}
	}

	applyPoolBounds(cfg)

	for _, tc := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"PingTimeout", cfg.PingTimeout, pingTimeout},
		{"ConnectTimeout", cfg.ConnConfig.ConnectTimeout, connectTimeout},
		{"MaxConnIdleTime", cfg.MaxConnIdleTime, maxConnIdleTime},
		{"HealthCheckPeriod", cfg.HealthCheckPeriod, healthCheckPeriod},
		{"MaxConnLifetime", cfg.MaxConnLifetime, maxConnLifetime},
		{"MaxConnLifetimeJitter", cfg.MaxConnLifetimeJitter, maxConnLifetimeJitter},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	if cfg.ConnConfig.DialFunc == nil {
		t.Error("DialFunc must be set alongside ConnectTimeout, or the dial is unbounded")
	}
}

// TestApplyPoolBoundsKeepsExplicitConnectTimeout: a DSN that sets
// connect_timeout keeps it as the dial deadline. The dialer is still replaced —
// it carries the socket-level bounds, which a connection string cannot express —
// but it is built from the DSN's value, so the deadline and the dialer agree.
func TestApplyPoolBoundsKeepsExplicitConnectTimeout(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(testDSN + "&connect_timeout=3")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.ConnConfig.ConnectTimeout != 3*time.Second {
		t.Fatalf("precondition: pgx parsed connect_timeout as %v, want 3s", cfg.ConnConfig.ConnectTimeout)
	}

	applyPoolBounds(cfg)

	if got := cfg.ConnConfig.ConnectTimeout; got != 3*time.Second {
		t.Errorf("ConnectTimeout = %v, want the DSN's 3s left alone", got)
	}
	// The pool-level bounds still apply: only the dial pair is deferred to.
	if cfg.PingTimeout != pingTimeout {
		t.Errorf("PingTimeout = %v, want %v", cfg.PingTimeout, pingTimeout)
	}
}

// TestPingTimeoutIsBoundedButNotTight holds both sides of the ping bound.
//
// Bounded at all is the fix: at zero, one dead socket takes the caller's whole
// deadline. But the default ShouldPing fires on any connection idle over 1s —
// every /readyz probe at periodSeconds 5 — so a tight bound turns a merely-slow
// backing into a pool-wide destroy-and-redial at its worst moment. A tablet
// leader election on RF=3 routinely outruns a few hundred milliseconds, and
// TCP_USER_TIMEOUT is what actually catches a departed peer, so this only has to
// bound the pathological case.
func TestPingTimeoutIsBoundedButNotTight(t *testing.T) {
	if pingTimeout == 0 {
		t.Fatal("pingTimeout must be bounded; zero is the pgx default this exists to replace")
	}
	if pingTimeout <= tcpUserTimeout/4 {
		t.Errorf("pingTimeout %v is tight next to tcpUserTimeout %v: a slow-but-alive store would "+
			"lose its idle pool before the kernel bound has any say", pingTimeout, tcpUserTimeout)
	}
	// A metrics scrape (5s) must still be able to shed more than one dead
	// connection, or the serial drain is back.
	if perScrape := int((5 * time.Second) / pingTimeout); perScrape < 2 {
		t.Errorf("a 5s scrape clears only %d dead connections at pingTimeout=%v; want >= 2",
			perScrape, pingTimeout)
	}
}

// TestUnavailablePoolExhaustionIsRetryable pins the classification of the error
// bounding PingTimeout newly makes reachable. pgxpool's Acquire retries
// MaxConns+1 times and then returns a bare errors.New with no SQLSTATE, no
// net.OpError and no SafeToRetry marker — so without an explicit arm it reads as
// a controller bug and the request answers 500 instead of a retryable 503, which
// is the regression #886 exists to prevent.
//
// The literal is pgxpool/pool.go:665 as of pgx v5.10.0. pgxpool exports no
// sentinel, so a reworded message on a version bump silently drops the match;
// this test is the thing that catches it.
func TestUnavailablePoolExhaustionIsRetryable(t *testing.T) {
	const pgxpoolMessage = "pgxpool: too many failed attempts acquiring connection; " +
		"likely bug in PrepareConn, BeforeAcquire, or ShouldPing hook"

	if !Unavailable(errors.New(pgxpoolMessage)) {
		t.Error("pool exhaustion must classify as unavailable (retryable 503), not as a controller bug")
	}
	if !Unavailable(fmt.Errorf("count branches: %w", errors.New(pgxpoolMessage))) {
		t.Error("pool exhaustion must survive the wrapping every store call adds")
	}
	// The arm must not swallow a genuine controller error.
	if Unavailable(errors.New("acquiring connection went badly")) {
		t.Error("a message that merely mentions acquiring a connection must not be treated as an outage")
	}
}
