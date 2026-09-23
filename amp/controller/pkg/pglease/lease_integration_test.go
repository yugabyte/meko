package pglease

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yugabyte/amp/controller/internal/db"
)

// Live-database tests for the lease contract end to end: who is granted, who is
// refused, and exactly when a key becomes takeable again. They skip when
// AMP_TEST_DATABASE_URL is unset, so `go test ./...` stays green without a
// database.
//
// The package under test calls amp_registry.token_acquire and friends, so these
// need migration 0004 applied: TestMain provisions ONE migrated database for the
// whole package rather than one per test, because DROP DATABASE costs ~70s on
// YugabyteDB. Importing internal/db is a test-only dependency — the library
// itself imports nothing from internal/, which is what keeps it importable.
//
// Every symbol here is prefixed leaseIT/TestPgIntegration so this file sits
// beside the other live-database test files without colliding with them.
//
// Timing assertions are anchored on the DATABASE clock, never on the test
// process's own: the lease value is a database clock reading, and comparing it
// against time.Now() would make the test assert that two clocks agree.

const (
	leaseITURLEnv = "AMP_TEST_DATABASE_URL"

	// The killed-holder test re-execs the test binary. The child is marked by
	// leaseITChildEnv and told which key and owner to use, so the parent
	// can assert on the exact owner it expects to find stranded in the row.
	leaseITChildEnv = "AMP_PGLEASE_IT_KILL_CHILD"
	// leaseITPeerEnv marks a peer in the multi-process tests, and leaseITPeerRole
	// selects which of them it is running.
	leaseITPeerEnv      = "AMP_PGLEASE_IT_PEER"
	leaseITPeerRole     = "AMP_PGLEASE_IT_PEER_ROLE"
	leaseITStopChildEnv = "AMP_PGLEASE_IT_STOP_CHILD"
	leaseITKeyEnv       = "AMP_PGLEASE_IT_KEY"
	leaseITOwnerEnv     = "AMP_PGLEASE_IT_OWNER"

	// Printed by the child once it actually holds the lease. Without it, a crash
	// in child setup would read as a successful fence.
	leaseITSentinel = "PGLEASE-IT-CHILD-HOLDS-LEASE"
)

// leaseITURL is the migrated scratch database TestMain provisioned, empty when
// AMP_TEST_DATABASE_URL was unset and every live test therefore skips.
var leaseITURL string

func TestMain(m *testing.M) {
	admin := os.Getenv(leaseITURLEnv)
	if admin == "" {
		os.Exit(m.Run())
	}
	// A re-exec'd child must NOT provision: the scratch name is fixed, so a child
	// doing this would drop and recreate the database its parent is mid-test on,
	// terminating its connections on the way. EVERY child marker has to be listed
	// here — the crash child inherits AMP_TEST_DATABASE_URL through os.Environ()
	// and needs no database of its own, so leaving it out is how that hazard comes
	// back. The lease children are handed the parent's URL instead.
	if os.Getenv(leaseITChildEnv) != "" || os.Getenv(leaseITStopChildEnv) != "" ||
		os.Getenv(crashChildEnv) != "" || os.Getenv(leaseITPeerEnv) != "" {
		leaseITURL = admin
		os.Exit(m.Run())
	}
	// Provisioning gets a budget of its own: ScratchMigrated queues behind a
	// machine-local lock that serializes DDL across packages.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	// "controller", like every other caller: this argument is the migration
	// search_path, so it decides where UNQUALIFIED DDL lands — 0001's tasks and
	// task_steps, the enums, schema_migrations. 0004's objects are schema-qualified
	// (amp_registry.resource_token and its functions), so naming amp_registry here
	// would buy nothing and drag the task log into the resource-model schema.
	url, drop, err := db.ScratchMigrated(ctx, admin, "amp_test_pglease", "controller")
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pglease: provision migrated database: %v\n", err)
		os.Exit(1)
	}
	leaseITURL = url
	code := m.Run()
	drop()
	os.Exit(code)
}

// leaseITEnv is one test's fixture: a pool on the migrated database, and
// a pool the test uses directly for database-clock reads and row inspection.
type leaseITEnv struct {
	t    *testing.T
	url  string
	pool *pgxpool.Pool
}

// leaseITSetup opens a pool on the package's migrated database (see TestMain).
// Tests isolate from each other by resource key, not by schema: every lease lives
// in the one amp_registry.resource_token that migration 0004 creates, so a key
// nothing else uses is the unit of isolation.
func leaseITSetup(t *testing.T) *leaseITEnv {
	t.Helper()
	if leaseITURL == "" {
		t.Skipf("%s is not set; skipping live-database test", leaseITURLEnv)
	}
	pool, err := pgxpool.New(context.Background(), leaseITURL)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	e := &leaseITEnv{t: t, url: leaseITURL, pool: pool}
	t.Cleanup(pool.Close)
	return e
}

// store builds a Store with the given takeover grace. There is no table to
// create: migration 0004 owns it.
func (e *leaseITEnv) store(skew time.Duration) *Store {
	e.t.Helper()
	s, err := NewStore(e.pool, StoreConfig{Skew: skew})
	if err != nil {
		e.t.Fatalf("new store: %v", err)
	}
	return s
}

func (e *leaseITEnv) nowMicros() int64 {
	e.t.Helper()
	var us int64
	err := e.pool.QueryRow(e.t.Context(),
		`SELECT (EXTRACT(EPOCH FROM clock_timestamp()) * 1000000)::bigint`).Scan(&us)
	if err != nil {
		e.t.Fatalf("read database clock: %v", err)
	}
	return us
}

// sleepUntil blocks until the DATABASE clock has reached us. Waiting out a lease
// against the local clock instead would be asserting that two clocks agree,
// which is the very thing the skew grace exists to tolerate.
func (e *leaseITEnv) sleepUntil(us int64) {
	e.t.Helper()
	for {
		now := e.nowMicros()
		if now >= us {
			return
		}
		wait := time.Duration(us-now) * time.Microsecond
		if wait > 30*time.Second {
			e.t.Fatalf("refusing to sleep %s: target is too far ahead of the database clock", wait)
		}
		time.Sleep(wait + 2*time.Millisecond)
	}
}

// row reads the raw lease row, so a test can check what was actually written
// rather than only what Acquire reported.
func (e *leaseITEnv) row(key string) (owner string, expiresAt, grantedAt int64, ok bool) {
	e.t.Helper()
	err := e.pool.QueryRow(e.t.Context(),
		`SELECT client_id, token, granted_us FROM amp_registry.resource_token`+
			` WHERE resource_id = $1`, key).Scan(&owner, &expiresAt, &grantedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, 0, false
	}
	if err != nil {
		e.t.Fatalf("read row %q: %v", key, err)
	}
	return owner, expiresAt, grantedAt, true
}

func leaseITRandID() string { return strings.ToLower(rand.Text()[:10]) }

func leaseITKey() string { return "key-" + strings.ToLower(rand.Text()[:16]) }

// leaseITGrant asserts the acquire was granted and returns it.
func leaseITGrant(t *testing.T, s *Store, key, owner string, ttl time.Duration) Grant {
	t.Helper()
	g, err := s.Acquire(t.Context(), key, owner, ttl)
	if err != nil {
		t.Fatalf("acquire %q as %q: %v", key, owner, err)
	}
	if !g.Granted {
		t.Fatalf("acquire %q as %q: refused, held by %q, retry after %s", key, owner, g.Holder, g.RetryAfter)
	}
	if g.Holder != owner {
		t.Errorf("granted holder = %q, want the caller %q", g.Holder, owner)
	}
	if g.RetryAfter != 0 {
		t.Errorf("granted RetryAfter = %s, want 0", g.RetryAfter)
	}
	if g.ExpiresAtMicros <= 0 {
		t.Errorf("granted ExpiresAtMicros = %d, want a positive lease value", g.ExpiresAtMicros)
	}
	return g
}

// leaseITDeny asserts the acquire was refused and returns the denial.
func leaseITDeny(t *testing.T, s *Store, key, owner string, ttl time.Duration) Grant {
	t.Helper()
	g, err := s.Acquire(t.Context(), key, owner, ttl)
	if err != nil {
		t.Fatalf("acquire %q as %q: %v", key, owner, err)
	}
	if g.Granted {
		t.Fatalf("acquire %q as %q: granted (expires %d), want refused", key, owner, g.ExpiresAtMicros)
	}
	if g.ExpiresAtMicros != 0 {
		t.Errorf("denied ExpiresAtMicros = %d, want 0", g.ExpiresAtMicros)
	}
	return g
}

func leaseITHolder(t *testing.T, s *Store, key string) *Lease {
	t.Helper()
	l, err := s.Holder(t.Context(), key)
	if err != nil {
		t.Fatalf("holder %q: %v", key, err)
	}
	return l
}

func leaseITRelease(t *testing.T, s *Store, key, owner string) bool {
	t.Helper()
	released, err := s.Release(t.Context(), key, owner)
	if err != nil {
		t.Fatalf("release %q as %q: %v", key, owner, err)
	}
	return released
}

// TestPgIntegrationAcquireOnFreeKey covers a grant on an absent key, and proves
// the lease value is the DATABASE's clock plus the ttl by bracketing it between
// two independent database clock reads.
func TestPgIntegrationAcquireOnFreeKey(t *testing.T) {
	env := leaseITSetup(t)
	s := env.store(DefaultSkew)
	key, owner := leaseITKey(), NewOwner("acquire-free")
	const ttl = 10 * time.Second

	if l := leaseITHolder(t, s, key); l != nil {
		t.Fatalf("holder on a fresh key = %+v, want nil", *l)
	}

	before := env.nowMicros()
	g := leaseITGrant(t, s, key, owner, ttl)
	after := env.nowMicros()

	lo, hi := before+ttl.Microseconds(), after+ttl.Microseconds()
	if g.ExpiresAtMicros < lo || g.ExpiresAtMicros > hi {
		t.Errorf("ExpiresAtMicros = %d, want within [%d, %d] (database clock + ttl)",
			g.ExpiresAtMicros, lo, hi)
	}

	rowOwner, rowExpiry, rowGranted, ok := env.row(key)
	if !ok {
		t.Fatal("no row written for a granted lease")
	}
	if rowOwner != owner {
		t.Errorf("row owner = %q, want %q", rowOwner, owner)
	}
	if rowExpiry != g.ExpiresAtMicros {
		t.Errorf("row expires_at_us = %d, want the returned %d", rowExpiry, g.ExpiresAtMicros)
	}
	// granted_at_us is the same clock read the expiry was built from, which is
	// what the conflict predicate compares against.
	if want := rowExpiry - ttl.Microseconds(); rowGranted != want {
		t.Errorf("row granted_at_us = %d, want expires_at_us - ttl = %d", rowGranted, want)
	}

	l := leaseITHolder(t, s, key)
	if l == nil {
		t.Fatal("holder after a grant = nil, want the lease")
	}
	if l.Owner != owner || l.ExpiresAtMicros != g.ExpiresAtMicros {
		t.Errorf("holder = %+v, want owner %q expiry %d", *l, owner, g.ExpiresAtMicros)
	}
}

// TestPgIntegrationSecondOwnerRefusedWhileHeld: a live lease refuses everyone
// else, and the denial carries the incumbent plus a usable retry hint.
func TestPgIntegrationSecondOwnerRefusedWhileHeld(t *testing.T) {
	env := leaseITSetup(t)
	s := env.store(DefaultSkew)
	key := leaseITKey()
	a, b := NewOwner("holder-a"), NewOwner("rival-b")
	const ttl = 10 * time.Second

	leaseITGrant(t, s, key, a, ttl)

	prev := time.Duration(1 << 62)
	for i := range 3 {
		d := leaseITDeny(t, s, key, b, ttl)
		if d.Holder != a {
			t.Errorf("attempt %d: denial holder = %q, want the incumbent %q", i, d.Holder, a)
		}
		if d.RetryAfter < time.Millisecond {
			t.Errorf("attempt %d: RetryAfter = %s, want at least 1ms while a holder row exists", i, d.RetryAfter)
		}
		if upper := ttl + DefaultSkew; d.RetryAfter > upper {
			t.Errorf("attempt %d: RetryAfter = %s, want at most ttl+skew = %s", i, d.RetryAfter, upper)
		}
		if d.RetryAfter > prev {
			t.Errorf("attempt %d: RetryAfter rose from %s to %s; the wait can only shrink", i, prev, d.RetryAfter)
		}
		prev = d.RetryAfter
		time.Sleep(20 * time.Millisecond)
	}

	// A denial must not have disturbed the row.
	if rowOwner, _, _, _ := env.row(key); rowOwner != a {
		t.Errorf("row owner after denials = %q, want %q", rowOwner, a)
	}
}

// TestPgIntegrationHolderRenewsWithoutGrace: the holder's renewal has no time
// condition at all. It is granted while the lease is live, after the expiry but
// inside the grace, and after the grace is spent too — provided nobody took over.
func TestPgIntegrationHolderRenewsWithoutGrace(t *testing.T) {
	env := leaseITSetup(t)
	const skew = time.Second
	s := env.store(skew)
	key, a := leaseITKey(), NewOwner("renewer-a")
	const ttl = 300 * time.Millisecond

	g := leaseITGrant(t, s, key, a, ttl)
	prev := g.ExpiresAtMicros

	// While live: every renewal is granted and pushes the expiry out.
	for i := range 5 {
		time.Sleep(30 * time.Millisecond)
		g = leaseITGrant(t, s, key, a, ttl)
		if g.ExpiresAtMicros <= prev {
			t.Fatalf("renewal %d: expiry %d did not advance past %d", i, g.ExpiresAtMicros, prev)
		}
		prev = g.ExpiresAtMicros
	}

	// Lapsed but inside the grace: still the holder's to renew.
	env.sleepUntil(prev + skew.Microseconds()/2)
	if l := leaseITHolder(t, s, key); l == nil {
		t.Fatal("holder inside the grace = nil, want the lapsed lease")
	} else if l.ExpiresIn != 0 || l.TakeoverIn <= 0 {
		t.Errorf("in-grace lease = %+v, want ExpiresIn 0 and TakeoverIn > 0", *l)
	}
	g = leaseITGrant(t, s, key, a, ttl)
	prev = g.ExpiresAtMicros

	// Grace spent, and nobody took over: the holder still gets it back. This is
	// the asymmetry — takeover is time-gated, renewal is not.
	env.sleepUntil(prev + skew.Microseconds() + 50_000)
	if l := leaseITHolder(t, s, key); l != nil {
		t.Fatalf("holder once the grace is spent = %+v, want nil (free to a new owner)", *l)
	}
	g = leaseITGrant(t, s, key, a, ttl)
	if rowOwner, rowExpiry, _, _ := env.row(key); rowOwner != a || rowExpiry != g.ExpiresAtMicros {
		t.Errorf("row after a long-lapsed renewal = (%q, %d), want (%q, %d)",
			rowOwner, rowExpiry, a, g.ExpiresAtMicros)
	}
}

// TestPgIntegrationSilentHolderTakeableOnlyAfterExpiryPlusSkew: a holder that
// stops responding (here, simply never renews) keeps the key until its expiry
// AND its grace are both spent — not a moment before.
func TestPgIntegrationSilentHolderTakeableOnlyAfterExpiryPlusSkew(t *testing.T) {
	env := leaseITSetup(t)
	a, b := NewOwner("silent-a"), NewOwner("taker-b")
	const ttl = 300 * time.Millisecond

	t.Run("grace_blocks_takeover", func(t *testing.T) {
		const skew = time.Second
		s := env.store(skew)
		key := leaseITKey()
		g := leaseITGrant(t, s, key, a, ttl)

		// Live: refused.
		leaseITDeny(t, s, key, b, ttl)

		// Lapsed, inside the grace: still refused, and the hint says how much of
		// the grace is left.
		env.sleepUntil(g.ExpiresAtMicros + 50_000)
		d := leaseITDeny(t, s, key, b, ttl)
		if d.Holder != a {
			t.Errorf("in-grace denial holder = %q, want %q", d.Holder, a)
		}
		if d.RetryAfter <= 0 || d.RetryAfter > skew {
			t.Errorf("in-grace RetryAfter = %s, want within (0, %s]", d.RetryAfter, skew)
		}

		// Grace spent: granted, and the row is now B's.
		env.sleepUntil(g.ExpiresAtMicros + skew.Microseconds() + 20_000)
		if l := leaseITHolder(t, s, key); l != nil {
			t.Errorf("holder once expiry+skew passed = %+v, want nil", *l)
		}
		bg := leaseITGrant(t, s, key, b, ttl)
		rowOwner, rowExpiry, _, _ := env.row(key)
		if rowOwner != b || rowExpiry != bg.ExpiresAtMicros {
			t.Errorf("row after takeover = (%q, %d), want (%q, %d)", rowOwner, rowExpiry, b, bg.ExpiresAtMicros)
		}
	})

	t.Run("zero_skew_takes_over_at_the_expiry", func(t *testing.T) {
		// Skew is a takeover-policy knob only: with no grace, takeover is legal
		// the instant the lease lapses.
		s := env.store(DisableSkew)
		if s.Skew() != 0 {
			t.Fatalf("Skew() = %s, want 0 taken literally", s.Skew())
		}
		key := leaseITKey()
		// A longer ttl than the rest of this test: with no grace at all, the
		// denial below has to complete before the lease lapses, so it needs a
		// whole second of headroom rather than a round trip's worth.
		const ttl = 2 * time.Second
		g := leaseITGrant(t, s, key, a, ttl)

		env.sleepUntil(g.ExpiresAtMicros - time.Second.Microseconds())
		leaseITDeny(t, s, key, b, ttl)

		env.sleepUntil(g.ExpiresAtMicros + 5_000)
		if l := leaseITHolder(t, s, key); l != nil {
			t.Errorf("holder with skew 0 after the expiry = %+v, want nil", *l)
		}
		leaseITGrant(t, s, key, b, ttl)
	})
}

// TestPgIntegrationReleaseBypassesGraceAndIsOwnerOnly: a graceful handoff needs
// no clock agreement, so a released key is takeable immediately; and a release
// only ever removes the caller's own lease.
func TestPgIntegrationReleaseBypassesGraceAndIsOwnerOnly(t *testing.T) {
	env := leaseITSetup(t)
	s := env.store(DefaultSkew)
	a, b := NewOwner("releaser-a"), NewOwner("successor-b")
	const ttl = 30 * time.Second

	t.Run("release_by_a_stranger_is_a_no_op", func(t *testing.T) {
		key := leaseITKey()
		g := leaseITGrant(t, s, key, a, ttl)
		if leaseITRelease(t, s, key, b) {
			t.Error("B's release of A's lease reported true, want false")
		}
		if rowOwner, rowExpiry, _, ok := env.row(key); !ok || rowOwner != a || rowExpiry != g.ExpiresAtMicros {
			t.Errorf("row after a stranger's release = (%q, %d, ok=%v), want A's untouched row",
				rowOwner, rowExpiry, ok)
		}
		// A's lease survived it: A can still renew and B is still refused.
		leaseITGrant(t, s, key, a, ttl)
		leaseITDeny(t, s, key, b, ttl)
	})

	t.Run("release_frees_the_key_immediately", func(t *testing.T) {
		key := leaseITKey()
		g := leaseITGrant(t, s, key, a, ttl)
		if !leaseITRelease(t, s, key, a) {
			t.Fatal("A's release of its own lease reported false, want true")
		}
		if _, _, _, ok := env.row(key); ok {
			t.Error("row still present after a release")
		}
		if l := leaseITHolder(t, s, key); l != nil {
			t.Errorf("holder after a release = %+v, want nil", *l)
		}
		// The whole point: no waiting out ttl+skew.
		leaseITGrant(t, s, key, b, ttl)
		if now := env.nowMicros(); now >= g.ExpiresAtMicros {
			t.Fatal("the handoff took longer than A's whole ttl; the test cannot tell a release from an expiry")
		}
		if rowOwner, _, _, _ := env.row(key); rowOwner != b {
			t.Errorf("row owner after the handoff = %q, want %q", rowOwner, b)
		}
	})

	t.Run("second_release_is_a_no_op", func(t *testing.T) {
		key := leaseITKey()
		leaseITGrant(t, s, key, a, ttl)
		if !leaseITRelease(t, s, key, a) {
			t.Fatal("first release reported false, want true")
		}
		if leaseITRelease(t, s, key, a) {
			t.Error("second release reported true, want false")
		}
	})
}

// TestPgIntegrationTakeoverOnlyWhenGivenUp is the "if and only if": a second
// owner is granted exactly when the lease was given up, whatever the reason, and
// never while it is being held. The death case is the subprocess test below.
func TestPgIntegrationTakeoverOnlyWhenGivenUp(t *testing.T) {
	env := leaseITSetup(t)
	const skew = 500 * time.Millisecond
	s := env.store(skew)
	a, b := NewOwner("incumbent-a"), NewOwner("challenger-b")

	t.Run("not_given_up_never_grants", func(t *testing.T) {
		key := leaseITKey()
		const ttl = 2 * time.Second
		g := leaseITGrant(t, s, key, a, ttl)
		// A renews well inside its ttl over a span longer than ttl+skew, measured
		// on the database clock; B is refused every single time. The ttl is what
		// gives each iteration room to be slow without the lease lapsing.
		until := g.ExpiresAtMicros + (ttl + skew).Microseconds()
		for env.nowMicros() < until {
			leaseITGrant(t, s, key, a, ttl)
			d := leaseITDeny(t, s, key, b, ttl)
			if d.Holder != a {
				t.Fatalf("denial holder = %q, want %q", d.Holder, a)
			}
			time.Sleep(200 * time.Millisecond)
		}
	})

	t.Run("given_up_by_release_grants", func(t *testing.T) {
		key := leaseITKey()
		const ttl = 30 * time.Second
		leaseITGrant(t, s, key, a, ttl)
		leaseITDeny(t, s, key, b, ttl)
		if !leaseITRelease(t, s, key, a) {
			t.Fatal("release reported false")
		}
		leaseITGrant(t, s, key, b, ttl)
	})

	t.Run("given_up_by_expiry_grants", func(t *testing.T) {
		key := leaseITKey()
		const ttl = 300 * time.Millisecond
		g := leaseITGrant(t, s, key, a, ttl)
		leaseITDeny(t, s, key, b, ttl)
		env.sleepUntil(g.ExpiresAtMicros + 20_000)
		leaseITDeny(t, s, key, b, ttl) // lapsed, but the grace is not spent
		env.sleepUntil(g.ExpiresAtMicros + skew.Microseconds() + 20_000)
		leaseITGrant(t, s, key, b, ttl)
	})
}

// TestPgIntegrationHolderProbeReportsHeldInGraceAndFree walks the probe's three
// states. "Free" means free to a NEW owner, so the probe returning nothing and
// what a subsequent Acquire decides can never disagree.
func TestPgIntegrationHolderProbeReportsHeldInGraceAndFree(t *testing.T) {
	env := leaseITSetup(t)
	const skew = time.Second
	s := env.store(skew)
	key, a := leaseITKey(), NewOwner("probe-a")
	const ttl = 400 * time.Millisecond

	if l := leaseITHolder(t, s, key); l != nil {
		t.Fatalf("probe on an absent key = %+v, want nil", *l)
	}

	g := leaseITGrant(t, s, key, a, ttl)
	held := leaseITHolder(t, s, key)
	if held == nil {
		t.Fatal("probe on a live lease = nil")
	}
	switch {
	case held.Owner != a:
		t.Errorf("owner = %q, want %q", held.Owner, a)
	case held.ExpiresAtMicros != g.ExpiresAtMicros:
		t.Errorf("ExpiresAtMicros = %d, want %d", held.ExpiresAtMicros, g.ExpiresAtMicros)
	case held.ExpiresIn <= 0:
		t.Errorf("ExpiresIn = %s, want positive on a live lease", held.ExpiresIn)
	case held.TakeoverIn < held.ExpiresIn:
		t.Errorf("TakeoverIn %s < ExpiresIn %s; the grace can only add time", held.TakeoverIn, held.ExpiresIn)
	case held.TakeoverIn > ttl+skew:
		t.Errorf("TakeoverIn = %s, want at most ttl+skew = %s", held.TakeoverIn, ttl+skew)
	}
	if got, want := held.ExpiresAt().UnixMicro(), g.ExpiresAtMicros; got != want {
		t.Errorf("ExpiresAt().UnixMicro() = %d, want %d", got, want)
	}

	env.sleepUntil(g.ExpiresAtMicros + 50_000)
	grace := leaseITHolder(t, s, key)
	if grace == nil {
		t.Fatal("probe inside the grace = nil, want the lapsed lease still reported")
	}
	if grace.ExpiresIn != 0 {
		t.Errorf("in-grace ExpiresIn = %s, want 0", grace.ExpiresIn)
	}
	if grace.TakeoverIn <= 0 || grace.TakeoverIn > skew {
		t.Errorf("in-grace TakeoverIn = %s, want within (0, %s]", grace.TakeoverIn, skew)
	}

	env.sleepUntil(g.ExpiresAtMicros + skew.Microseconds() + 20_000)
	if l := leaseITHolder(t, s, key); l != nil {
		t.Errorf("probe once expiry+skew passed = %+v, want nil", *l)
	}

	// And a released lease reads free at once, without waiting out the grace.
	leaseITGrant(t, s, key, a, 30*time.Second)
	if !leaseITRelease(t, s, key, a) {
		t.Fatal("release reported false")
	}
	if l := leaseITHolder(t, s, key); l != nil {
		t.Errorf("probe after a release = %+v, want nil", *l)
	}
}

// TestPgIntegrationClientRenewsAndFencesOthers drives the renewal loop against
// the real store: a held lease keeps being renewed with no grace needed, keeps
// everyone else out for as long as it is held, and frees the key on Release.
func TestPgIntegrationClientRenewsAndFencesOthers(t *testing.T) {
	env := leaseITSetup(t)
	s := env.store(DefaultSkew)
	key, rival := leaseITKey(), NewOwner("rival")

	aborts := make(chan error, 4)
	// Timings with room for a slow round trip against a loaded single-node
	// cluster: proving renewal needs the lease held past one ttl, not a tight
	// cadence.
	c, err := New(s, NewOwner("client-a"), Config{
		TTL:          6 * time.Second,
		RenewEvery:   time.Second,
		RenewTimeout: 2 * time.Second,
		SafetyMargin: 2 * time.Second,
		Abort:        func(_ string, cause error) { aborts <- cause },
		Logger:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	h, err := c.Acquire(t.Context(), key)
	if err != nil {
		t.Fatalf("client acquire: %v", err)
	}
	released := false
	defer func() {
		if !released {
			_ = h.Release(context.WithoutCancel(t.Context()))
		}
	}()

	start := h.ExpiresAtMicros()
	prev := start
	// Held for well over one ttl: without renewal the rival would be granted.
	for range 12 {
		time.Sleep(700 * time.Millisecond)
		if err := h.Err(); err != nil {
			t.Fatalf("lease ended while it should be held: %v", err)
		}
		got := h.ExpiresAtMicros()
		if got < prev {
			t.Fatalf("ExpiresAtMicros fell from %d to %d", prev, got)
		}
		prev = got
		d := leaseITDeny(t, s, key, rival, 6*time.Second)
		if d.Holder != c.Owner() {
			t.Fatalf("denial holder = %q, want %q", d.Holder, c.Owner())
		}
	}
	if prev <= start {
		t.Errorf("ExpiresAtMicros never advanced from %d; the renewal loop is not renewing", start)
	}

	if err := h.Release(t.Context()); err != nil {
		t.Fatalf("release: %v", err)
	}
	released = true
	if err := h.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("Err() after Release = %v, want context.Canceled", err)
	}
	if l := leaseITHolder(t, s, key); l != nil {
		t.Errorf("holder after Release = %+v, want nil", *l)
	}
	leaseITGrant(t, s, key, rival, 6*time.Second)

	select {
	case cause := <-aborts:
		t.Errorf("abort fired while the lease was healthy: %v", cause)
	default:
	}
}

// TestPgIntegrationKilledHolderTakeableAfterExpiry is the death case against the
// real database: a child process holds and renews a real lease, is SIGKILLed
// with nothing cleaned up, and the key becomes takeable only once its expiry and
// grace are spent. SIGKILL rather than a fake, because the point is that nothing
// runs on the way out — no release, no defer, no final renewal.
func TestPgIntegrationKilledHolderTakeableAfterExpiry(t *testing.T) {
	if os.Getenv(leaseITChildEnv) == "1" {
		leaseITKillChild(t)
		return
	}
	if testing.Short() {
		t.Skip("re-execs the test binary; skipped under -short")
	}

	env := leaseITSetup(t)
	s := env.store(DefaultSkew)
	key := leaseITKey()
	childOwner := "killed-child-" + leaseITRandID()
	parent := NewOwner("parent")

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^TestPgIntegrationKilledHolderTakeableAfterExpiry$",
		"-test.count=1",
		"-test.timeout=80s",
		"-test.v",
	)
	cmd.Env = append(os.Environ(),
		leaseITChildEnv+"=1",
		// The child must use the database the parent migrated, not the admin URL
		// it would otherwise inherit — amp_registry does not exist there.
		leaseITURLEnv+"="+leaseITURL,
		leaseITKeyEnv+"="+key,
		leaseITOwnerEnv+"="+childOwner,
	)
	out := &leaseITWatcher{want: leaseITSentinel, hit: make(chan struct{})}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = 10 * time.Second

	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	killed := false
	defer func() {
		if !killed && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	select {
	case <-out.hit:
	case <-time.After(60 * time.Second):
		t.Fatalf("child never reported holding the lease; output:\n%s", out.String())
	}

	// The child really holds it: the row is its, and we are refused.
	if l := leaseITHolder(t, s, key); l == nil || l.Owner != childOwner {
		t.Fatalf("holder while the child runs = %v, want owner %q; child output:\n%s",
			l, childOwner, out.String())
	}
	leaseITDeny(t, s, key, parent, 5*time.Second)

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	killed = true
	_ = cmd.Wait()

	// Nothing was cleaned up: the row is still the dead process's, and it still
	// fences us until its expiry and grace are spent.
	stranded := leaseITHolder(t, s, key)
	if stranded == nil || stranded.Owner != childOwner {
		t.Fatalf("holder after the kill = %v, want the dead child's lease; child output:\n%s",
			stranded, out.String())
	}
	if stranded.TakeoverIn < time.Second {
		t.Fatalf("the dead child's lease had only %s left; the kill took so long that the fence below would prove nothing",
			stranded.TakeoverIn)
	}
	d := leaseITDeny(t, s, key, parent, 5*time.Second)
	if d.Holder != childOwner {
		t.Errorf("denial holder after the kill = %q, want %q", d.Holder, childOwner)
	}

	env.sleepUntil(stranded.ExpiresAtMicros + DefaultSkew.Microseconds() + 50_000)
	if l := leaseITHolder(t, s, key); l != nil {
		t.Errorf("holder once the dead child's lease lapsed = %+v, want nil", *l)
	}
	g := leaseITGrant(t, s, key, parent, 5*time.Second)
	if rowOwner, rowExpiry, _, _ := env.row(key); rowOwner != parent || rowExpiry != g.ExpiresAtMicros {
		t.Errorf("row after taking over from the dead child = (%q, %d), want (%q, %d)",
			rowOwner, rowExpiry, parent, g.ExpiresAtMicros)
	}
}

// leaseITKillChild is the child half: hold a real, renewing lease and block. It
// must never skip — a skipping child exits 0 and the parent's failure would then
// be about the wrong thing.
func leaseITKillChild(t *testing.T) {
	url := os.Getenv(leaseITURLEnv)
	key := os.Getenv(leaseITKeyEnv)
	owner := os.Getenv(leaseITOwnerEnv)
	if url == "" || key == "" || owner == "" {
		t.Fatalf("child started without its environment: url set=%v key=%q owner=%q",
			url != "", key, owner)
	}

	ctx := context.Background()
	s, err := Open(ctx, url, StoreConfig{Skew: DefaultSkew})
	if err != nil {
		t.Fatalf("child: open store: %v", err)
	}
	defer s.Close()

	// Generous enough that the child cannot fence itself before the parent kills
	// it: the parent waits out the stranded lease on the database clock, so a
	// long ttl costs the test nothing but makes it deterministic.
	c, err := New(s, owner, leaseITChildConfig)
	if err != nil {
		t.Fatalf("child: new client: %v", err)
	}
	h, err := c.AcquireWait(ctx, key)
	if err != nil {
		t.Fatalf("child: acquire %q: %v", key, err)
	}

	// Only now may the parent kill us.
	fmt.Println(leaseITSentinel, h.ExpiresAtMicros())

	select {
	case <-time.After(60 * time.Second):
		t.Fatal("child: still alive after 60s; the parent never killed it")
	case <-h.Context().Done():
		t.Fatalf("child: lease ended on its own: %v", h.Err())
	}
}

// leaseITChildConfig is what both child halves hold their lease with: a ttl long
// enough that a slow round trip cannot abort the child, and renewals frequent
// enough that the row is demonstrably being kept alive.
var leaseITChildConfig = Config{
	TTL:          10 * time.Second,
	RenewEvery:   time.Second,
	RenewTimeout: 3 * time.Second,
	SafetyMargin: 4 * time.Second,
	Logger:       slog.New(slog.DiscardHandler),
}

// TestPgIntegrationStoppedHolderIsFencedOnResume is requirement 5 in its
// strongest form: a holder that is ALIVE the whole time but stops refreshing.
// SIGSTOP freezes it mid-lease, the lease lapses on the database clock, another
// owner legally takes over, and SIGCONT resumes the original — which must die
// rather than carry on writing. A stalled process, not a dead one, is the case a
// takeover cannot otherwise be safe against.
func TestPgIntegrationStoppedHolderIsFencedOnResume(t *testing.T) {
	if os.Getenv(leaseITStopChildEnv) == "1" {
		leaseITStopChild(t)
		return
	}
	if testing.Short() {
		t.Skip("re-execs the test binary; skipped under -short")
	}

	env := leaseITSetup(t)
	s := env.store(DefaultSkew)
	key := leaseITKey()
	childOwner := "stopped-child-" + leaseITRandID()
	parent := NewOwner("parent")

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^TestPgIntegrationStoppedHolderIsFencedOnResume$",
		"-test.count=1",
		"-test.timeout=110s",
		"-test.v",
	)
	cmd.Env = append(os.Environ(),
		leaseITStopChildEnv+"=1",
		// The child must use the database the parent migrated, not the admin URL
		// it would otherwise inherit — amp_registry does not exist there.
		leaseITURLEnv+"="+leaseITURL,
		leaseITKeyEnv+"="+key,
		leaseITOwnerEnv+"="+childOwner,
	)
	out := &leaseITWatcher{want: leaseITSentinel, hit: make(chan struct{})}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = 10 * time.Second

	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	reaped := false
	defer func() {
		if !reaped && cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGCONT)
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	select {
	case <-out.hit:
	case <-time.After(60 * time.Second):
		t.Fatalf("child never reported holding the lease; output:\n%s", out.String())
	}

	// Frozen, not killed: every goroutine it has, including the renewal loop,
	// stops where it stands.
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("stop child: %v", err)
	}
	stranded := leaseITHolder(t, s, key)
	if stranded == nil || stranded.Owner != childOwner {
		t.Fatalf("holder while the child is stopped = %v, want the child's lease; output:\n%s",
			stranded, out.String())
	}

	env.sleepUntil(stranded.ExpiresAtMicros + DefaultSkew.Microseconds() + 50_000)
	g := leaseITGrant(t, s, key, parent, 30*time.Second)

	if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatalf("resume child: %v", err)
	}
	err := cmd.Wait()
	reaped = true

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		t.Fatalf("resumed child exited 0: it must die rather than resume writing; output:\n%s", out.String())
	case !errors.As(err, &exitErr):
		t.Fatalf("child did not exit with a status: %v; output:\n%s", err, out.String())
	case exitErr.ExitCode() != AbortExitCode:
		// 1 is the child's own t.Fatal, which means the abort never ran.
		t.Fatalf("resumed child exit code = %d, want AbortExitCode %d; output:\n%s",
			exitErr.ExitCode(), AbortExitCode, out.String())
	}
	// Either cause is correct here: the deadline elapsed while it was frozen,
	// and the row is someone else's now. What matters is that it died.
	if want := fmt.Sprintf("FATAL: pglease: lease %q lost", key); !strings.Contains(out.String(), want) {
		t.Errorf("child output lacks %q:\n%s", want, out.String())
	}
	// The successor is untouched: the resumed child neither renewed nor released.
	if rowOwner, rowExpiry, _, _ := env.row(key); rowOwner != parent || rowExpiry != g.ExpiresAtMicros {
		t.Errorf("row after the resumed child died = (%q, %d), want the successor's (%q, %d)",
			rowOwner, rowExpiry, parent, g.ExpiresAtMicros)
	}
}

// leaseITStopChild holds a real lease and then does nothing at all. It must not
// watch its own context: the only correct way out of here is AbortProcess, so
// surviving the sleep is the failure and exits 1 rather than AbortExitCode.
func leaseITStopChild(t *testing.T) {
	url := os.Getenv(leaseITURLEnv)
	key := os.Getenv(leaseITKeyEnv)
	owner := os.Getenv(leaseITOwnerEnv)
	if url == "" || key == "" || owner == "" {
		t.Fatalf("child started without its environment: url set=%v key=%q owner=%q",
			url != "", key, owner)
	}

	ctx := context.Background()
	s, err := Open(ctx, url, StoreConfig{Skew: DefaultSkew})
	if err != nil {
		t.Fatalf("child: open store: %v", err)
	}
	defer s.Close()

	// Abort is left nil: the child must run the real AbortProcess.
	c, err := New(s, owner, leaseITChildConfig)
	if err != nil {
		t.Fatalf("child: new client: %v", err)
	}
	h, err := c.AcquireWait(ctx, key)
	if err != nil {
		t.Fatalf("child: acquire %q: %v", key, err)
	}

	fmt.Println(leaseITSentinel, h.ExpiresAtMicros())
	time.Sleep(90 * time.Second)
	t.Fatalf("child: still alive after losing lease %q (%v): the fence never fired", key, h.Err())
}

// TestPgIntegrationTakeoverFencesTheHoldingClient: a row that becomes somebody
// else's under a live holder — a forced handoff, or any out-of-band change —
// must fence the incumbent on its next renewal, with ErrLost rather than a
// timeout. Without row surgery this path is unreachable against the real SQL:
// Config.Validate guarantees a holder self-aborts on its own deadline before a
// rival can legally take over.
func TestPgIntegrationTakeoverFencesTheHoldingClient(t *testing.T) {
	env := leaseITSetup(t)
	s := env.store(DefaultSkew)
	key := leaseITKey()

	aborts := make(chan error, 4)
	c, err := New(s, NewOwner("incumbent"), Config{
		TTL:          10 * time.Second,
		RenewEvery:   300 * time.Millisecond,
		RenewTimeout: 3 * time.Second,
		SafetyMargin: 4 * time.Second,
		Abort:        func(_ string, cause error) { aborts <- cause },
		Logger:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	h, err := c.Acquire(t.Context(), key)
	if err != nil {
		t.Fatalf("client acquire: %v", err)
	}

	rival := NewOwner("forced-successor")
	now := env.nowMicros()
	tag, err := env.pool.Exec(t.Context(),
		`UPDATE amp_registry.resource_token`+
			` SET client_id = $2, token = $3, granted_us = $4 WHERE resource_id = $1`,
		key, rival, now+(30*time.Second).Microseconds(), now)
	if err != nil {
		t.Fatalf("hand the row to the rival: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("handover updated %d rows, want 1", tag.RowsAffected())
	}

	select {
	case cause := <-aborts:
		if !errors.Is(cause, ErrLost) {
			t.Errorf("abort cause = %v, want ErrLost: the renewal was refused, not merely unanswered", cause)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the incumbent was never fenced; Err() = %v", h.Err())
	}
	if err := h.Err(); !errors.Is(err, ErrLost) {
		t.Errorf("Err() = %v, want ErrLost", err)
	}
	// The fenced incumbent leaves the successor's row exactly as it found it.
	if rowOwner, _, _, _ := env.row(key); rowOwner != rival {
		t.Errorf("row owner after the fence = %q, want the rival %q", rowOwner, rival)
	}
	if l := leaseITHolder(t, s, key); l == nil || l.Owner != rival {
		t.Errorf("holder after the fence = %v, want the rival %q", l, rival)
	}
}

// TestPgIntegrationConcurrentAcquireGrantsExactlyOne is the headline property
// under contention: N processes acquiring one free key at once, one winner. It
// is what the conditional upsert and the 40001 replay exist for, and no
// sequential test can show it.
func TestPgIntegrationConcurrentAcquireGrantsExactlyOne(t *testing.T) {
	env := leaseITSetup(t)
	s := env.store(DefaultSkew)
	key := leaseITKey()
	const (
		racers = 8
		ttl    = 20 * time.Second
	)

	type outcome struct {
		owner string
		grant Grant
		err   error
	}
	results := make(chan outcome, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range racers {
		owner := fmt.Sprintf("racer-%d-%s", i, leaseITRandID())
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			g, err := s.Acquire(t.Context(), key, owner, ttl)
			results <- outcome{owner: owner, grant: g, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var granted []string
	for r := range results {
		if r.err != nil {
			t.Errorf("acquire as %q: %v", r.owner, r.err)
			continue
		}
		if r.grant.Granted {
			granted = append(granted, r.owner)
		}
	}
	if len(granted) != 1 {
		t.Fatalf("%d of %d racers were granted the same key: %v", len(granted), racers, granted)
	}
	if rowOwner, _, _, _ := env.row(key); rowOwner != granted[0] {
		t.Errorf("row owner = %q, want the one winner %q", rowOwner, granted[0])
	}
	if l := leaseITHolder(t, s, key); l == nil || l.Owner != granted[0] {
		t.Errorf("holder = %v, want the one winner %q", l, granted[0])
	}
}

// leaseITWatcher accumulates a child's output and signals once a sentinel shows
// up in it. It is an io.Writer rather than a scanned pipe so nothing of ours
// touches *testing.T from a goroutine that may outlive the test.
type leaseITWatcher struct {
	want string
	hit  chan struct{}

	mu   sync.Mutex
	buf  strings.Builder
	seen bool
}

func (w *leaseITWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if !w.seen && strings.Contains(w.buf.String(), w.want) {
		w.seen = true
		close(w.hit)
	}
	return len(p), nil
}

func (w *leaseITWatcher) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// leaseITPeer runs one peer process: it takes the lease on the key it is given
// and reports what happened on stdout, which the parent parses. Split out so both
// multi-process tests share one child entrypoint.
func leaseITPeer(t *testing.T) {
	t.Helper()
	url, key := os.Getenv(leaseITURLEnv), os.Getenv(leaseITKeyEnv)
	owner, role := os.Getenv(leaseITOwnerEnv), os.Getenv(leaseITPeerRole)
	if url == "" || key == "" || owner == "" {
		t.Fatalf("peer started without its environment: url set=%v key=%q owner=%q", url != "", key, owner)
	}
	ctx := context.Background()
	s, err := Open(ctx, url, StoreConfig{Skew: DefaultSkew})
	if err != nil {
		t.Fatalf("peer: open store: %v", err)
	}
	defer s.Close()

	const ttl = 10 * time.Second
	c, err := New(s, owner, Config{
		TTL: ttl, RenewEvery: 2 * time.Second,
		RenewTimeout: time.Second, SafetyMargin: 2 * time.Second,
		Abort: func(string, error) {}, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("peer: new client: %v", err)
	}

	switch role {
	case "race":
		// One attempt, no waiting: the parent counts winners.
		h, err := c.Acquire(ctx, key)
		if err != nil {
			var d *Denial
			if errors.As(err, &d) {
				fmt.Printf("PEER DENIED %s\n", owner)
				return
			}
			t.Fatalf("peer: acquire: %v", err)
		}
		fmt.Printf("PEER GOT %s %d\n", owner, h.ExpiresAtMicros()-ttl.Microseconds())
		// Hold while the others attempt, then exit WITHOUT releasing: the row has
		// to outlive this process for the parent to check who the database says
		// won, and a holder that exits still holding is the controller-crash shape
		// anyway. The key is unique per run, so the lease is left to lapse.
		time.Sleep(2 * time.Second)

	case "queue":
		// Wait for a turn, hold briefly, report the window and release.
		wctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		h, err := c.AcquireWait(wctx, key)
		if err != nil {
			t.Fatalf("peer: acquire-wait: %v", err)
		}
		start := h.ExpiresAtMicros() - ttl.Microseconds()
		time.Sleep(500 * time.Millisecond)
		var end int64
		if err := s.pool.QueryRow(ctx,
			`SELECT (EXTRACT(EPOCH FROM clock_timestamp()) * 1000000)::bigint`).Scan(&end); err != nil {
			t.Fatalf("peer: read clock: %v", err)
		}
		if err := h.Release(ctx); err != nil {
			t.Fatalf("peer: release: %v", err)
		}
		fmt.Printf("PEER HELD %s %d %d\n", owner, start, end)

	default:
		t.Fatalf("peer: unknown role %q", role)
	}
}

// leaseITStartPeers launches n peer processes on one key and returns their
// combined output once they have all exited.
func leaseITStartPeers(t *testing.T, test, role, key string, n int) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	out := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.CommandContext(ctx, os.Args[0],
				"-test.run=^"+test+"$", "-test.count=1", "-test.timeout=150s", "-test.v")
			cmd.Env = append(os.Environ(),
				leaseITPeerEnv+"=1",
				leaseITPeerRole+"="+role,
				leaseITURLEnv+"="+leaseITURL,
				leaseITKeyEnv+"="+key,
				leaseITOwnerEnv+"="+NewOwner(fmt.Sprintf("peer-%d", i)),
			)
			b, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("peer %d exited with %v:\n%s", i, err, b)
			}
			out[i] = string(b)
		}(i)
	}
	wg.Wait()
	return out
}

// TestPgIntegrationMultiProcessOnlyOneAcquires: several independent processes
// reach for one key at once and exactly one gets it.
//
// This is the multi-controller case, and nothing else in this file covers it: the
// in-process tests share a Client, whose per-key claim map refuses a second
// acquire before the database is ever asked. Here each process has its own
// Client, its own owner id and its own connection, so the row is the only thing
// that can serialise them.
func TestPgIntegrationMultiProcessOnlyOneAcquires(t *testing.T) {
	if os.Getenv(leaseITPeerEnv) == "1" {
		leaseITPeer(t)
		return
	}
	if testing.Short() {
		t.Skip("re-execs the test binary; skipped under -short")
	}
	env := leaseITSetup(t)
	key := leaseITKey()

	const peers = 4
	outs := leaseITStartPeers(t, "TestPgIntegrationMultiProcessOnlyOneAcquires", "race", key, peers)

	var got, denied int
	for i, o := range outs {
		switch {
		case strings.Contains(o, "PEER GOT "):
			got++
		case strings.Contains(o, "PEER DENIED "):
			denied++
		default:
			t.Fatalf("peer %d reported neither outcome:\n%s", i, o)
		}
	}
	if got != 1 || denied != peers-1 {
		t.Fatalf("%d processes acquired and %d were denied, want exactly 1 and %d — "+
			"two controllers holding one resource is the failure this exists to prevent",
			got, denied, peers-1)
	}
	// The winner is the owner the row actually records.
	rowOwner, _, _, ok := env.row(key)
	if !ok {
		t.Fatal("no row after the race, want the winner's lease")
	}
	if !strings.Contains(strings.Join(outs, "\n"), "PEER GOT "+rowOwner) {
		t.Fatalf("row owner %q is not the process that reported winning", rowOwner)
	}
}

// TestPgIntegrationMultiProcessHandoffIsSerialized: several independent processes
// queue for one key, and their hold windows do not overlap.
//
// The rolling-restart shape: every controller wants the resource, each waits its
// turn, and no two are ever inside the critical section together. Windows are
// database clock readings taken by the peers themselves, so no local clock is
// compared against another.
func TestPgIntegrationMultiProcessHandoffIsSerialized(t *testing.T) {
	if os.Getenv(leaseITPeerEnv) == "1" {
		leaseITPeer(t)
		return
	}
	if testing.Short() {
		t.Skip("re-execs the test binary; skipped under -short")
	}
	leaseITSetup(t)
	key := leaseITKey()

	const peers = 3
	outs := leaseITStartPeers(t, "TestPgIntegrationMultiProcessHandoffIsSerialized", "queue", key, peers)

	type window struct {
		owner      string
		start, end int64
	}
	var held []window
	for i, o := range outs {
		var w window
		found := false
		for _, line := range strings.Split(o, "\n") {
			if _, err := fmt.Sscanf(strings.TrimSpace(line), "PEER HELD %s %d %d",
				&w.owner, &w.start, &w.end); err == nil {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("peer %d never reported a hold window:\n%s", i, o)
		}
		held = append(held, w)
	}
	if len(held) != peers {
		t.Fatalf("%d peers held the lease, want %d", len(held), peers)
	}
	for i := range held {
		for j := i + 1; j < len(held); j++ {
			a, b := held[i], held[j]
			if a.start > b.start {
				a, b = b, a
			}
			if b.start < a.end {
				t.Fatalf("processes %s [%d, %d] and %s [%d, %d] held the key at the same time",
					a.owner, a.start, a.end, b.owner, b.start, b.end)
			}
		}
	}
}
