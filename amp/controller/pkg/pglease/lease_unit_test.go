package pglease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The in-process fixture. RenewEvery deliberately does not divide
// TTL-SafetyMargin evenly (25s / 7s), so no tick coincides with the abort
// deadline and the attempt count is assertable.
// TestUnitDeadlineWinsATieWithATick covers the coincidence itself.
const (
	unitTTL          = 30 * time.Second
	unitRenewEvery   = 7 * time.Second
	unitRenewTimeout = 1 * time.Second
	unitSafetyMargin = 5 * time.Second
	unitDeadline     = unitTTL - unitSafetyMargin
)

func unitLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// unitWaitEnded blocks until the lease ends, and fails instead of hanging if it
// never does: a regression that keeps a lease alive past its deadline must read
// as a failure, not as a stuck test.
func unitWaitEnded(t *testing.T, h *Held) {
	t.Helper()
	limit := 4 * unitTTL
	select {
	case <-h.Context().Done():
	case <-time.After(limit):
		t.Fatalf("lease still held after %s, want it ended; Err() = %v", limit, h.Err())
	}
}

func unitConfig(abort AbortFunc) Config {
	return Config{
		TTL:          unitTTL,
		RenewEvery:   unitRenewEvery,
		RenewTimeout: unitRenewTimeout,
		SafetyMargin: unitSafetyMargin,
		Abort:        abort,
		Logger:       unitLogger(),
	}
}

// unitGrant is what a healthy backend answers: an expiry read from the (bubble)
// clock at the moment of the call.
func unitGrant(ttl time.Duration) Grant {
	return Grant{Granted: true, ExpiresAtMicros: time.Now().Add(ttl).UnixMicro()}
}

type unitAcquireCall struct {
	Key   string
	Owner string
	TTL   time.Duration
	At    time.Time
}

// unitBackend is an in-memory Backend whose per-attempt behaviour is scripted by
// acquireFn, which receives the 1-based attempt number and the attempt's own
// context so a test can block inside a call.
type unitBackend struct {
	mu        sync.Mutex
	acquires  []unitAcquireCall
	releases  int
	events    []string
	acquireFn func(ctx context.Context, attempt int) (Grant, error)
	releaseFn func(ctx context.Context) (bool, error)
}

func (b *unitBackend) Acquire(ctx context.Context, key, owner string, ttl time.Duration) (Grant, error) {
	b.mu.Lock()
	b.acquires = append(b.acquires, unitAcquireCall{Key: key, Owner: owner, TTL: ttl, At: time.Now()})
	n := len(b.acquires)
	b.events = append(b.events, fmt.Sprintf("acquire:start:%d", n))
	fn := b.acquireFn
	b.mu.Unlock()

	g := unitGrant(ttl)
	var err error
	if fn != nil {
		g, err = fn(ctx, n)
	}

	b.mu.Lock()
	b.events = append(b.events, fmt.Sprintf("acquire:end:%d", n))
	b.mu.Unlock()
	return g, err
}

func (b *unitBackend) Release(ctx context.Context, key, owner string) (bool, error) {
	b.mu.Lock()
	b.releases++
	b.events = append(b.events, fmt.Sprintf("release:%d", b.releases))
	fn := b.releaseFn
	b.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return true, nil
}

func (b *unitBackend) attempts() []unitAcquireCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]unitAcquireCall(nil), b.acquires...)
}

func (b *unitBackend) releaseCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.releases
}

func (b *unitBackend) log() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.events...)
}

type unitAbortCall struct {
	Key    string
	Cause  error
	CtxErr error // Held.Context().Err() as observed from inside the abort
}

// unitAbortRecorder is the injected fail-fast seam: it records instead of ending
// the process, so the trigger is assertable without killing the test binary.
type unitAbortRecorder struct {
	mu    sync.Mutex
	calls []unitAbortCall
}

func (r *unitAbortRecorder) fn(held *atomic.Pointer[Held]) AbortFunc {
	return func(key string, cause error) {
		var ctxErr error
		if h := held.Load(); h != nil {
			ctxErr = h.Context().Err()
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, unitAbortCall{Key: key, Cause: cause, CtxErr: ctxErr})
	}
}

func (r *unitAbortRecorder) snapshot() []unitAbortCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]unitAbortCall(nil), r.calls...)
}

// unitPool builds a pool over an address nothing listens on. pgxpool connects
// lazily, so any call that reaches the pool fails — which is exactly what proves
// an argument check ran before the I/O.
func unitPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://pglease:pglease@127.0.0.1:1/pglease?sslmode=disable")
	if err != nil {
		t.Fatalf("build lazy pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestUnitConfigValidate(t *testing.T) {
	base := func() Config { return unitConfig(nil) }
	with := func(mutate func(*Config)) Config {
		c := base()
		mutate(&c)
		return c
	}

	cases := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"wholly zero value means the defaults", Config{}, true},
		{"non-timing fields alone still mean the defaults",
			Config{Abort: func(string, error) {}, Logger: unitLogger()}, true},
		{"fixture", base(), true},
		{"ttl zero", with(func(c *Config) { c.TTL = 0 }), false},
		{"ttl negative", with(func(c *Config) { c.TTL = -time.Millisecond }), false},
		{"ttl at MaxTTL", with(func(c *Config) { c.TTL = MaxTTL }), true},
		{"ttl above MaxTTL", with(func(c *Config) { c.TTL = MaxTTL + time.Millisecond }), false},
		{"renew interval zero", with(func(c *Config) { c.RenewEvery = 0 }), false},
		{"renew interval negative", with(func(c *Config) { c.RenewEvery = -time.Second }), false},
		{"renew timeout zero", with(func(c *Config) { c.RenewTimeout = 0 }), false},
		{"renew timeout negative", with(func(c *Config) { c.RenewTimeout = -time.Second }), false},
		{"safety margin zero", with(func(c *Config) { c.SafetyMargin = 0 }), false},
		{"safety margin negative", with(func(c *Config) { c.SafetyMargin = -time.Second }), false},
		{"renew interval equal to the abort deadline",
			with(func(c *Config) { c.RenewEvery = unitDeadline }), false},
		{"renew interval past the abort deadline",
			with(func(c *Config) { c.RenewEvery = unitDeadline + time.Second }), false},
		{"renew interval one tick inside the abort deadline",
			with(func(c *Config) { c.RenewEvery = unitDeadline - time.Millisecond }), true},
		{"safety margin under the renew timeout",
			with(func(c *Config) { c.RenewTimeout = c.SafetyMargin + time.Second }), false},
		{"safety margin equal to the renew timeout", with(func(c *Config) { c.RenewTimeout = c.SafetyMargin }), true},
		{"ttl below one millisecond", with(func(c *Config) { c.TTL = 500 * time.Microsecond }), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			switch {
			case tc.ok && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case !tc.ok && err == nil:
				t.Fatal("Validate() = nil, want an error")
			case !tc.ok && !errors.Is(err, ErrConfig):
				t.Fatalf("Validate() = %v, want it to wrap ErrConfig", err)
			}
		})
	}
}

// The defaults are the cadence calculation nothing else checks: enough attempts
// must fit inside the abort deadline that two consecutive failures are
// survivable.
func TestUnitDefaultCadenceIsCoherent(t *testing.T) {
	if err := (Config{}).Validate(); err != nil {
		t.Fatalf("default Config invalid: %v", err)
	}
	for _, c := range []Config{{}, unitConfig(nil)} {
		c = c.normalize()
		deadline := c.TTL - c.SafetyMargin
		if c.RenewEvery >= deadline {
			t.Fatalf("renew interval %s is not inside the abort deadline %s", c.RenewEvery, deadline)
		}
		if ticks := deadline / c.RenewEvery; ticks < 2 {
			t.Fatalf("only %d renewal attempts fit before the abort deadline; "+
				"two consecutive failures must be survivable", ticks)
		}
	}
}

func TestUnitNewRejectsBadArguments(t *testing.T) {
	be := &unitBackend{}
	cases := []struct {
		name  string
		bknd  Backend
		owner string
		cfg   Config
	}{
		{"nil backend", nil, "owner-a", unitConfig(nil)},
		{"empty owner", be, "", unitConfig(nil)},
		{"invalid config", be, "owner-a",
			Config{TTL: time.Second, RenewEvery: 0, RenewTimeout: time.Second, SafetyMargin: time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(tc.bknd, tc.owner, tc.cfg)
			if err == nil {
				t.Fatal("New() = nil error, want an error")
			}
			if c != nil {
				t.Fatalf("New() = %v, want a nil Client on error", c)
			}
			if !errors.Is(err, ErrConfig) {
				t.Fatalf("New() = %v, want it to wrap ErrConfig", err)
			}
		})
	}
}

func TestUnitStoreConfigValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  StoreConfig
		ok   bool
	}{
		{"zero value uses the defaults", StoreConfig{}, true},
		{"skew zero means the default, not no grace", StoreConfig{Skew: 0}, true},
		{"DisableSkew is the explicit opt-out", StoreConfig{Skew: DisableSkew}, true},
		{"other negatives are still rejected", StoreConfig{Skew: -2 * time.Millisecond}, false},
		{"skew at MaxSkew", StoreConfig{Skew: MaxSkew}, true},
		{"skew above MaxSkew", StoreConfig{Skew: MaxSkew + time.Millisecond}, false},
		{"skew at MinTTL", StoreConfig{Skew: MinTTL}, true},
		{"skew below one millisecond truncates to no grace on the wire", StoreConfig{Skew: 500 * time.Microsecond}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			switch {
			case tc.ok && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case !tc.ok && err == nil:
				t.Fatal("Validate() = nil, want an error")
			case !tc.ok && !errors.Is(err, ErrConfig):
				t.Fatalf("Validate() = %v, want it to wrap ErrConfig", err)
			}
		})
	}
}

func TestUnitStoreSkewIsReportedLiterally(t *testing.T) {
	pool := unitPool(t)
	// Zero resolves to the default; everything else is taken as given. The
	// unset case carrying a grace is the point: no-grace has to be asked for.
	for _, tc := range []struct{ given, want time.Duration }{
		{0, DefaultSkew},
		{DefaultSkew, DefaultSkew},
		{MaxSkew, MaxSkew},
		{DisableSkew, 0},
	} {
		s, err := NewStore(pool, StoreConfig{Skew: tc.given})
		if err != nil {
			t.Fatalf("NewStore(skew=%s): %v", tc.given, err)
		}
		if got := s.Skew(); got != tc.want {
			t.Fatalf("Skew() for %s = %s, want %s", tc.given, got, tc.want)
		}
	}
	// DisableSkew is what reaches the database as a literal zero.
	s, err := NewStore(pool, StoreConfig{Skew: DisableSkew})
	if err != nil {
		t.Fatalf("NewStore(DisableSkew): %v", err)
	}
	if got := s.cfg.effectiveSkew(); got != 0 {
		t.Fatalf("effectiveSkew() with DisableSkew = %s, want 0", got)
	}
	if _, err := NewStore(nil, StoreConfig{}); !errors.Is(err, ErrConfig) {
		t.Fatalf("NewStore(nil pool) = %v, want ErrConfig", err)
	}
}

// A rejected argument must never reach the pool: the store here is built over an
// address nothing listens on, so any I/O would fail with a dial error instead.
func TestUnitStoreAcquireRejectsBadArgumentsBeforeIO(t *testing.T) {
	s, err := NewStore(unitPool(t), StoreConfig{Skew: DefaultSkew})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	cases := []struct {
		name  string
		key   string
		owner string
		ttl   time.Duration
	}{
		{"empty key", "", "owner-a", time.Second},
		{"empty owner", "k", "", time.Second},
		{"ttl zero", "k", "owner-a", 0},
		{"ttl negative", "k", "owner-a", -time.Millisecond},
		{"ttl below one millisecond", "k", "owner-a", 500 * time.Microsecond},
		{"ttl above MaxTTL", "k", "owner-a", MaxTTL + time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			g, err := s.Acquire(ctx, tc.key, tc.owner, tc.ttl)
			if !errors.Is(err, ErrConfig) {
				t.Fatalf("Acquire = %v, want ErrConfig (checked before any I/O)", err)
			}
			if g != (Grant{}) {
				t.Fatalf("Acquire = %+v, want the zero Grant", g)
			}
		})
	}
}

func TestUnitGrantNormalization(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	str := func(v string) *string { return &v }

	cases := []struct {
		name    string
		granted bool
		exp     *int64
		hold    *string
		hint    *int64
		want    Grant
	}{
		{
			name:    "grant discards the row's holder and hint",
			granted: true,
			exp:     i64(1_700_000_000_000_000), hold: str("someone"), hint: i64(4200),
			want: Grant{Granted: true, ExpiresAtMicros: 1_700_000_000_000_000},
		},
		{
			name:    "grant with no denial context",
			granted: true,
			exp:     i64(42),
			want:    Grant{Granted: true, ExpiresAtMicros: 42},
		},
		{
			name: "denial carries the holder and the hint",
			hold: str("owner-b"), hint: i64(1500),
			want: Grant{Holder: "owner-b", RetryAfter: 1500 * time.Millisecond},
		},
		{
			name: "the smallest hint the statement can report",
			hold: str("owner-b"), hint: i64(1),
			want: Grant{Holder: "owner-b", RetryAfter: time.Millisecond},
		},
		{
			// The row vanished under the acquire: retry now, do not sleep.
			name: "vanished row",
			want: Grant{},
		},
		{
			name: "negative hint clamps to zero",
			hold: str("owner-b"), hint: i64(-5),
			want: Grant{Holder: "owner-b"},
		},
		{
			name: "zero hint stays zero",
			hold: str("owner-b"), hint: i64(0),
			want: Grant{Holder: "owner-b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := grantFromRow(tc.granted, tc.exp, tc.hold, tc.hint)
			if got != tc.want {
				t.Fatalf("grantFromRow = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestUnitExpiresAtIsZeroWhenUnset(t *testing.T) {
	if got := (Grant{}).ExpiresAt(); !got.IsZero() {
		t.Fatalf("Grant{}.ExpiresAt() = %s, want the zero time", got)
	}
	if got := (Lease{}).ExpiresAt(); !got.IsZero() {
		t.Fatalf("Lease{}.ExpiresAt() = %s, want the zero time", got)
	}
	us := int64(1_700_000_000_123_456)
	if got := (Grant{ExpiresAtMicros: us}).ExpiresAt().UnixMicro(); got != us {
		t.Fatalf("Grant.ExpiresAt().UnixMicro() = %d, want %d", got, us)
	}
	if got := (Lease{ExpiresAtMicros: us}).ExpiresAt().UnixMicro(); got != us {
		t.Fatalf("Lease.ExpiresAt().UnixMicro() = %d, want %d", got, us)
	}
}

// The rule lives in migration 0006, and the point of this package is that it is
// not restated here. These assertions are what stops it creeping back: a future
// edit that inlines the predicate to "save a function call" would reintroduce the
// second copy that can drift out of step with the Rust client.
func TestUnitStatementsCallTheMigrationsFunctions(t *testing.T) {
	mustContain := func(name, stmt string, subs ...string) {
		t.Helper()
		for _, sub := range subs {
			if !strings.Contains(stmt, sub) {
				t.Fatalf("%s statement is missing %q:\n%s", name, sub, stmt)
			}
		}
	}
	mustNotContain := func(name, stmt string, subs ...string) {
		t.Helper()
		for _, sub := range subs {
			if strings.Contains(stmt, sub) {
				t.Fatalf("%s statement must not contain %q:\n%s", name, sub, stmt)
			}
		}
	}

	for name, want := range map[string]struct{ stmt, fn string }{
		"acquire": {acquireStmt, "amp_registry.token_acquire("},
		"release": {releaseStmt, "amp_registry.token_release("},
		"holder":  {holderStmt, "amp_registry.token_holder("},
	} {
		// Schema-qualified, because search_path is never set on the connection:
		// a library must not depend on a session GUC.
		mustContain(name, want.stmt, want.fn)

		// No second copy of the rule: no predicate, no clock read, no DDL. The
		// arithmetic and the comparison are the function's job.
		mustNotContain(name, want.stmt,
			"ON CONFLICT", "clock_timestamp", "now()", "CREATE ", "DELETE ", "UPDATE ",
			"EXCLUDED", "999")
	}
}

func TestUnitDenialUnwrapsToNotAcquired(t *testing.T) {
	d := &Denial{Key: "k", Holder: "owner-b", RetryAfter: 900 * time.Millisecond}
	if !errors.Is(d, ErrNotAcquired) {
		t.Fatal("Denial does not match ErrNotAcquired")
	}
	if msg := d.Error(); !strings.Contains(msg, "owner-b") || !strings.Contains(msg, "900ms") {
		t.Fatalf("Denial.Error() = %q, want the holder and the wait in it", msg)
	}
	// A vanished row has no holder to name.
	if msg := (&Denial{Key: "k"}).Error(); strings.Contains(msg, `by ,`) {
		t.Fatalf("Denial.Error() with no holder = %q", msg)
	}
}

func TestUnitNewOwnerIdentifiesOneProcess(t *testing.T) {
	a, b := NewOwner("pglease-test"), NewOwner("pglease-test")
	if a == b {
		t.Fatalf("NewOwner returned the same id twice (%q): two processes sharing an id are both the holder", a)
	}
	for _, got := range []string{a, b} {
		if !strings.HasPrefix(got, "pglease-test@") {
			t.Fatalf("NewOwner = %q, want it to start with the label", got)
		}
		if len(got) < len("pglease-test@")+8 {
			t.Fatalf("NewOwner = %q, want host, pid and nonce in it", got)
		}
	}
}

func TestUnitAcquireDenialCarriesRetryHint(t *testing.T) {
	be := &unitBackend{acquireFn: func(context.Context, int) (Grant, error) {
		return Grant{Holder: "owner-b", RetryAfter: 1200 * time.Millisecond}, nil
	}}
	c, err := New(be, "owner-a", unitConfig(func(string, error) { t.Error("abort must not fire on a denial") }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := c.Acquire(context.Background(), "k")
	if h != nil {
		t.Fatal("Acquire returned a Held without the lease")
	}
	var d *Denial
	if !errors.As(err, &d) {
		t.Fatalf("Acquire = %v, want a *Denial", err)
	}
	if !errors.Is(err, ErrNotAcquired) {
		t.Fatalf("Acquire = %v, want it to match ErrNotAcquired", err)
	}
	if d.Key != "k" || d.Holder != "owner-b" || d.RetryAfter != 1200*time.Millisecond {
		t.Fatalf("Denial = %+v, want the key, holder and hint from the Grant", d)
	}
	if n := len(be.attempts()); n != 1 {
		t.Fatalf("backend acquires = %d, want 1 (no renewal for a lease we do not hold)", n)
	}
}

func TestUnitAcquireBackendErrorPropagates(t *testing.T) {
	boom := errors.New("backend unavailable")
	be := &unitBackend{acquireFn: func(context.Context, int) (Grant, error) { return Grant{}, boom }}
	c, err := New(be, "owner-a", unitConfig(func(string, error) { t.Error("abort must not fire") }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := c.Acquire(context.Background(), "k")
	if h != nil || !errors.Is(err, boom) {
		t.Fatalf("Acquire = (%v, %v), want (nil, the backend error)", h, err)
	}
	var d *Denial
	if errors.As(err, &d) {
		t.Fatal("a failed call must not be reported as a denial")
	}
}

func TestUnitAcquireWaitSleepsTheHintAndFloorsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		be := &unitBackend{acquireFn: func(_ context.Context, attempt int) (Grant, error) {
			switch attempt {
			case 1:
				return Grant{Holder: "owner-b", RetryAfter: time.Second}, nil
			case 2:
				return Grant{Holder: "owner-b"}, nil // vanished row: retry now
			default:
				return unitGrant(unitTTL), nil
			}
		}}
		c, err := New(be, "owner-a", unitConfig(func(string, error) { t.Error("abort must not fire") }))
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		start := time.Now()
		h, err := c.AcquireWait(context.Background(), "k")
		if err != nil {
			t.Fatalf("AcquireWait: %v", err)
		}
		// 1s from the hint, then the 250ms floor for the "retry now" denial.
		if got, want := time.Since(start), time.Second+minAcquireWait; got != want {
			t.Fatalf("AcquireWait waited %s, want %s (the hint, then the floor)", got, want)
		}
		if n := len(be.attempts()); n != 3 {
			t.Fatalf("backend acquires = %d, want 3", n)
		}
		if err := h.Release(context.Background()); err != nil {
			t.Fatalf("Release: %v", err)
		}
	})
}

func TestUnitAcquireWaitStopsWhenContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		be := &unitBackend{acquireFn: func(context.Context, int) (Grant, error) {
			return Grant{Holder: "owner-b", RetryAfter: 2 * time.Second}, nil
		}}
		c, err := New(be, "owner-a", unitConfig(func(string, error) { t.Error("abort must not fire") }))
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		h, err := c.AcquireWait(ctx, "k")
		if h != nil {
			t.Fatal("AcquireWait returned a Held it never got")
		}
		if got, want := time.Since(start), 5*time.Second; got != want {
			t.Fatalf("AcquireWait returned after %s, want %s", got, want)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("AcquireWait = %v, want it to report the context error", err)
		}
		if !errors.Is(err, ErrNotAcquired) {
			t.Fatalf("AcquireWait = %v, want it to also report the denial", err)
		}
		// Attempts at 0s, 2s and 4s: waiting, not spinning.
		if n := len(be.attempts()); n != 3 {
			t.Fatalf("backend acquires = %d, want 3", n)
		}
	})
}

func TestUnitRenewalCadenceUsesTheConfiguredTimings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		be := &unitBackend{}
		var held atomic.Pointer[Held]
		rec := &unitAbortRecorder{}
		// The wholly zero timings mean the defaults, which is what this asserts.
		c, err := New(be, "owner-a", Config{Abort: rec.fn(&held), Logger: unitLogger()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		start := time.Now()
		h, err := c.Acquire(context.Background(), "k")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		held.Store(h)

		// Four renewals fit in 22s at the default 5s cadence, and each confirmed
		// renewal pushes the abort deadline out, so the lease outlives the TTL.
		time.Sleep(22 * time.Second)
		if err := h.Err(); err != nil {
			t.Fatalf("Err() = %v, want nil while renewals are confirmed", err)
		}
		if calls := rec.snapshot(); len(calls) != 0 {
			t.Fatalf("abort fired %d times on a healthy lease: %+v", len(calls), calls)
		}

		got := be.attempts()
		want := []time.Duration{0, 5 * time.Second, 10 * time.Second, 15 * time.Second, 20 * time.Second}
		if len(got) != len(want) {
			t.Fatalf("backend acquires = %d, want %d", len(got), len(want))
		}
		for i, call := range got {
			if at := call.At.Sub(start); at != want[i] {
				t.Fatalf("attempt %d at %s, want %s", i, at, want[i])
			}
			if call.TTL != DefaultTTL {
				t.Fatalf("attempt %d asked for ttl %s, want the default %s", i, call.TTL, DefaultTTL)
			}
			if call.Key != "k" || call.Owner != "owner-a" {
				t.Fatalf("attempt %d = %+v, want the acquiring key and owner", i, call)
			}
		}
		if want := got[len(got)-1].At.Add(DefaultTTL).UnixMicro(); h.ExpiresAtMicros() != want {
			t.Fatalf("ExpiresAtMicros = %d, want %d from the last confirmed renewal", h.ExpiresAtMicros(), want)
		}
		if err := h.Release(context.Background()); err != nil {
			t.Fatalf("Release: %v", err)
		}
	})
}

func TestUnitRenewalToleratesTransientFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		be := &unitBackend{acquireFn: func(_ context.Context, attempt int) (Grant, error) {
			if attempt == 1 {
				return unitGrant(unitTTL), nil
			}
			return Grant{}, errors.New("backend unavailable")
		}}
		var held atomic.Pointer[Held]
		rec := &unitAbortRecorder{}
		c, err := New(be, "owner-a", unitConfig(rec.fn(&held)))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		h, err := c.Acquire(context.Background(), "k")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		held.Store(h)
		expiry := h.ExpiresAtMicros()

		// Two consecutive failed calls are not a lost lease: the gap between the
		// cadence and the TTL exists to absorb them.
		time.Sleep(2*unitRenewEvery + time.Second)
		if err := h.Err(); err != nil {
			t.Fatalf("Err() = %v after two failed renewals, want nil", err)
		}
		if calls := rec.snapshot(); len(calls) != 0 {
			t.Fatalf("abort fired on a failed call: %+v", calls)
		}
		if got := h.ExpiresAtMicros(); got != expiry {
			t.Fatalf("ExpiresAtMicros = %d, want it unchanged at %d by a failed renewal", got, expiry)
		}
		if n := len(be.attempts()); n != 3 {
			t.Fatalf("backend acquires = %d, want 3 (acquire plus two ticks)", n)
		}
		if err := h.Release(context.Background()); err != nil {
			t.Fatalf("Release: %v", err)
		}
	})
}

// The fail-fast trigger: time-based, not failure-counted, and fired through the
// injected seam so the test process survives to assert on it.
func TestUnitRenewalDeadlineAborts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		be := &unitBackend{acquireFn: func(_ context.Context, attempt int) (Grant, error) {
			if attempt == 1 {
				return unitGrant(unitTTL), nil
			}
			return Grant{}, errors.New("backend unavailable")
		}}
		var held atomic.Pointer[Held]
		rec := &unitAbortRecorder{}
		c, err := New(be, "owner-a", unitConfig(rec.fn(&held)))
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		start := time.Now()
		h, err := c.Acquire(context.Background(), "k")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		held.Store(h)

		<-h.Context().Done()
		if got := time.Since(start); got != unitDeadline {
			t.Fatalf("lease ended after %s, want ttl minus safety margin (%s)", got, unitDeadline)
		}
		synctest.Wait()

		if err := h.Err(); !errors.Is(err, ErrRenewalDeadline) {
			t.Fatalf("Err() = %v, want ErrRenewalDeadline", err)
		}
		calls := rec.snapshot()
		if len(calls) != 1 {
			t.Fatalf("abort fired %d times, want exactly 1: %+v", len(calls), calls)
		}
		if calls[0].Key != "k" || !errors.Is(calls[0].Cause, ErrRenewalDeadline) {
			t.Fatalf("abort call = %+v, want key \"k\" and ErrRenewalDeadline", calls[0])
		}
		// Cancelled before the abort ran, so in-process work stops even when the
		// abort is a hook that returns.
		if calls[0].CtxErr == nil {
			t.Fatal("abort ran before the lease context was cancelled")
		}
		// Ticks at 7s, 14s and 21s inside the 25s deadline.
		if n := len(be.attempts()); n != 4 {
			t.Fatalf("backend acquires = %d, want 4 (acquire plus three ticks)", n)
		}
		for _, call := range be.attempts() {
			if at := call.At.Sub(start); at > unitDeadline {
				t.Fatalf("a renewal was attempted at %s, past the abort deadline %s", at, unitDeadline)
			}
		}
	})
}

func TestUnitRenewalDenialLosesImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		be := &unitBackend{acquireFn: func(_ context.Context, attempt int) (Grant, error) {
			if attempt == 1 {
				return unitGrant(unitTTL), nil
			}
			return Grant{Holder: "owner-b", RetryAfter: 5 * time.Second}, nil
		}}
		var held atomic.Pointer[Held]
		rec := &unitAbortRecorder{}
		c, err := New(be, "owner-a", unitConfig(rec.fn(&held)))
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		start := time.Now()
		h, err := c.Acquire(context.Background(), "k")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		held.Store(h)

		<-h.Context().Done()
		// On the tick, not at the deadline: someone else demonstrably holds it.
		if got := time.Since(start); got != unitRenewEvery {
			t.Fatalf("lease ended after %s, want the first tick (%s)", got, unitRenewEvery)
		}
		synctest.Wait()

		if err := h.Err(); !errors.Is(err, ErrLost) {
			t.Fatalf("Err() = %v, want ErrLost", err)
		}
		calls := rec.snapshot()
		if len(calls) != 1 || !errors.Is(calls[0].Cause, ErrLost) || calls[0].CtxErr == nil {
			t.Fatalf("abort calls = %+v, want one ErrLost with the context already cancelled", calls)
		}
		if n := len(be.attempts()); n != 2 {
			t.Fatalf("backend acquires = %d, want 2 (no attempt after the loss)", n)
		}
	})
}

// blockingHandler parks the first record until released, so a test can hold a
// goroutine at a known point inside the code under test.
type blockingHandler struct {
	gate chan struct{}
	in   chan struct{}
	once sync.Once
}

func (h *blockingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *blockingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *blockingHandler) WithGroup(string) slog.Handler            { return h }

func (h *blockingHandler) Handle(context.Context, slog.Record) error {
	h.once.Do(func() {
		close(h.in)
		<-h.gate
	})
	return nil
}

// A Release that is ALREADY inside the once when the lease is lost must not wedge
// the renewal goroutine. Abort has to fire, or the process survives holding a
// lease it cannot prove — the one outcome this package exists to prevent.
//
// The interleaving is forced rather than raced for: lose's first statement logs,
// so a blocking handler parks the renewal goroutine inside lose with the once
// still free, and the test takes it from another goroutine before letting lose
// continue. Waiting on h.Context() instead would make this a ~few-instruction
// race that passes against a deadlocking implementation, which is exactly what an
// earlier version of this test did.
//
// It fails by hanging, so the deadline is the assertion.
func TestLossConcurrentWithReleaseDoesNotHang(t *testing.T) {
	be := &unitBackend{}
	var granting atomic.Bool
	granting.Store(true)
	be.acquireFn = func(_ context.Context, _ int) (Grant, error) {
		if granting.Load() {
			return unitGrant(2 * time.Second), nil
		}
		return Grant{Holder: "someone-else"}, nil
	}

	bh := &blockingHandler{gate: make(chan struct{}), in: make(chan struct{})}
	aborted := make(chan struct{})
	c, err := New(be, "owner-a", Config{
		TTL:          2 * time.Second,
		RenewEvery:   20 * time.Millisecond,
		RenewTimeout: 50 * time.Millisecond,
		SafetyMargin: 100 * time.Millisecond,
		Abort:        func(string, error) { close(aborted) },
		Logger:       slog.New(bh),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := c.Acquire(context.Background(), "k")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	granting.Store(false) // the next renewal is refused, so the lease is lost

	select {
	case <-bh.in: // the renewal goroutine is now parked inside lose
	case <-time.After(10 * time.Second):
		t.Fatal("the lease was never lost")
	}

	// Take the once while lose is parked: this is the caller mid-Release.
	released := make(chan error, 1)
	go func() { released <- h.Release(context.Background()) }()
	// Give that goroutine time to actually be inside Release, blocked on the
	// renewal goroutine's exit, before lose is allowed to proceed.
	time.Sleep(200 * time.Millisecond)
	close(bh.gate)

	select {
	case <-aborted:
	case <-time.After(10 * time.Second):
		t.Fatal("Abort never fired: lose is wedged against a concurrent Release")
	}
	select {
	case err := <-released:
		if err != nil {
			t.Fatalf("Release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Release never returned")
	}
	if n := be.releaseCount(); n != 0 {
		t.Fatalf("a lost lease issued %d release(s), want 0", n)
	}
}

// A lease that was LOST must never issue a release. The claim is dropped on loss
// so a caller whose Abort returns can take the key again — and because the owner
// id is per-Client and constant, the successor's row matches the dead Held's
// delete exactly. Without the guard a deferred Release on the dead Held deletes
// the successor's row, grace-free, while the successor still reports itself live.
func TestUnitLostLeaseDoesNotReleaseItsSuccessor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Grant the first acquire, refuse every renewal so the lease is lost,
		// then grant again for the successor.
		var granting atomic.Bool
		granting.Store(true)
		be := &unitBackend{}
		be.acquireFn = func(_ context.Context, _ int) (Grant, error) {
			if granting.Load() {
				return unitGrant(unitTTL), nil
			}
			return Grant{Holder: "someone-else"}, nil
		}
		aborted := make(chan struct{}, 1)
		// An Abort that RETURNS: this whole hazard is only reachable when the
		// process survives the loss, which is what a non-fatal hook does.
		c, err := New(be, "owner-a", unitConfig(func(string, error) {
			select {
			case aborted <- struct{}{}:
			default:
			}
		}))
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		lost, err := c.Acquire(context.Background(), "k")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		granting.Store(false)
		time.Sleep(unitTTL + time.Second) // let the lease be taken from under it
		select {
		case <-aborted:
		default:
			t.Fatal("the lease was never reported lost")
		}

		// The successor: same Client, same owner id, so the two are
		// indistinguishable to token_release.
		granting.Store(true)
		successor, err := c.Acquire(context.Background(), "k")
		if err != nil {
			t.Fatalf("re-Acquire after loss: %v", err)
		}
		before := be.releaseCount()

		// The deferred release a caller would realistically have registered.
		if err := lost.Release(context.Background()); err != nil {
			t.Fatalf("Release on a lost lease: %v", err)
		}
		if got := be.releaseCount(); got != before {
			t.Fatalf("a lost lease issued %d release(s), want 0 — it would delete the successor's row",
				got-before)
		}
		if err := successor.Err(); err != nil {
			t.Fatalf("successor.Err() = %v, want it still live", err)
		}
		// The successor's renewal goroutine outlives the assertions, and the
		// bubble will not go idle with it running.
		if err := successor.Release(context.Background()); err != nil {
			t.Fatalf("release the successor: %v", err)
		}
	})
}

func TestUnitReleaseStopsRenewalAndIsIdempotent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		be := &unitBackend{}
		c, err := New(be, "owner-a", unitConfig(func(string, error) { t.Error("abort must not fire on a release") }))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		h, err := c.Acquire(context.Background(), "k")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}

		time.Sleep(unitRenewEvery + time.Second) // one confirmed renewal
		if err := h.Release(context.Background()); err != nil {
			t.Fatalf("Release: %v", err)
		}
		after := len(be.attempts())

		if err := h.Err(); !errors.Is(err, context.Canceled) {
			t.Fatalf("Err() = %v, want context.Canceled after a release", err)
		}
		if err := h.Release(context.Background()); err != nil {
			t.Fatalf("second Release = %v, want nil (idempotent)", err)
		}
		if n := be.releaseCount(); n != 1 {
			t.Fatalf("backend releases = %d, want 1", n)
		}

		// The renewal goroutine is gone: no attempt can land after the row was
		// dropped, however long we wait.
		time.Sleep(5 * unitTTL)
		if n := len(be.attempts()); n != after {
			t.Fatalf("backend acquires = %d after the release, want %d", n, after)
		}
		if n := be.releaseCount(); n != 1 {
			t.Fatalf("backend releases = %d, want 1", n)
		}
	})
}

// Release must not return while the goroutine can still touch the backend: a
// renewal landing after the delete would re-create the lease just given up.
func TestUnitReleaseWaitsForAnInFlightRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		be := &unitBackend{acquireFn: func(ctx context.Context, attempt int) (Grant, error) {
			if attempt == 1 {
				return unitGrant(unitTTL), nil
			}
			select {
			case <-ctx.Done():
				return Grant{}, ctx.Err()
			case <-time.After(unitRenewTimeout / 2):
				return unitGrant(unitTTL), nil
			}
		}}
		c, err := New(be, "owner-a", unitConfig(func(string, error) { t.Error("abort must not fire") }))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		h, err := c.Acquire(context.Background(), "k")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}

		// Land inside the second attempt, which started at the first tick.
		time.Sleep(unitRenewEvery + 100*time.Millisecond)
		if err := h.Release(context.Background()); err != nil {
			t.Fatalf("Release: %v", err)
		}

		events := be.log()
		want := []string{"acquire:start:1", "acquire:end:1", "acquire:start:2", "acquire:end:2", "release:1"}
		if strings.Join(events, ",") != strings.Join(want, ",") {
			t.Fatalf("backend events = %v, want %v (the in-flight renewal completes before the delete)", events, want)
		}
		if err := h.Err(); !errors.Is(err, context.Canceled) {
			t.Fatalf("Err() = %v, want context.Canceled", err)
		}
	})
}

func TestUnitReleaseWithFailingBackendStillStopsRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		boom := errors.New("delete failed")
		be := &unitBackend{releaseFn: func(context.Context) (bool, error) { return false, boom }}
		c, err := New(be, "owner-a", unitConfig(func(string, error) { t.Error("abort must not fire") }))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		h, err := c.Acquire(context.Background(), "k")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if err := h.Release(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("Release = %v, want the backend error reported", err)
		}
		if err := h.Err(); !errors.Is(err, context.Canceled) {
			t.Fatalf("Err() = %v, want context.Canceled", err)
		}
		time.Sleep(3 * unitTTL)
		if n := len(be.attempts()); n != 1 {
			t.Fatalf("backend acquires = %d, want 1: renewal must stop even when the delete failed", n)
		}
	})
}

func TestUnitCauseDistinguishesLossFromRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		be := &unitBackend{acquireFn: func(_ context.Context, attempt int) (Grant, error) {
			if attempt == 1 {
				return unitGrant(unitTTL), nil
			}
			return Grant{Holder: "owner-b", RetryAfter: time.Second}, nil
		}}
		var held atomic.Pointer[Held]
		rec := &unitAbortRecorder{}
		c, err := New(be, "owner-a", unitConfig(rec.fn(&held)))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		h, err := c.Acquire(context.Background(), "k")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		held.Store(h)

		// The deferred-Release shape: the caller releases after the loss and must
		// still be able to tell the two apart.
		<-h.Context().Done()
		synctest.Wait()
		if err := h.Release(context.Background()); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if err := h.Err(); !errors.Is(err, ErrLost) {
			t.Fatalf("Err() = %v, want ErrLost to survive a later Release", err)
		}
		if errors.Is(h.Err(), context.Canceled) {
			t.Fatal("Release overwrote the loss cause")
		}
	})
}

func TestUnitHeldExpiryOnlyRises(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var first int64
		be := &unitBackend{acquireFn: func(_ context.Context, attempt int) (Grant, error) {
			if attempt == 1 {
				g := unitGrant(unitTTL)
				first = g.ExpiresAtMicros
				return g, nil
			}
			// A reordered push: an older expiry than the one already held.
			return Grant{Granted: true, ExpiresAtMicros: first - int64(time.Second/time.Microsecond)}, nil
		}}
		c, err := New(be, "owner-a", unitConfig(func(string, error) { t.Error("abort must not fire") }))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		h, err := c.Acquire(context.Background(), "k")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if got := h.ExpiresAtMicros(); got != first {
			t.Fatalf("ExpiresAtMicros = %d, want the granted %d", got, first)
		}

		time.Sleep(unitRenewEvery + time.Second)
		if got := h.ExpiresAtMicros(); got != first {
			t.Fatalf("ExpiresAtMicros = %d, want the fence to stay at %d rather than shorten", got, first)
		}
		if got, want := h.ExpiresAt().UnixMicro(), first; got != want {
			t.Fatalf("ExpiresAt().UnixMicro() = %d, want %d", got, want)
		}
		if err := h.Release(context.Background()); err != nil {
			t.Fatalf("Release: %v", err)
		}
	})
}

// The acquire context bounds the acquire call only. Deriving renewal from it
// would kill the lease the moment a request handler returned.
func TestUnitAcquireContextDoesNotBoundRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		be := &unitBackend{}
		c, err := New(be, "owner-a", unitConfig(func(string, error) { t.Error("abort must not fire") }))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		h, err := c.Acquire(ctx, "k")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		cancel()
		synctest.Wait()
		if err := h.Err(); err != nil {
			t.Fatalf("Err() = %v, want nil: the lease outlives the call that took it", err)
		}

		time.Sleep(3*unitRenewEvery + time.Second)
		if err := h.Err(); err != nil {
			t.Fatalf("Err() = %v after the acquire context was cancelled, want nil", err)
		}
		if n := len(be.attempts()); n != 4 {
			t.Fatalf("backend acquires = %d, want 4 (renewal kept going)", n)
		}
		if err := h.Release(context.Background()); err != nil {
			t.Fatalf("Release: %v", err)
		}
	})
}

// Two Held over one row would each believe they own the key, and either one's
// Release — which skips the grace — would free it under the other.
func TestUnitAcquireTwiceOnOneKeyIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		be := &unitBackend{}
		var held atomic.Pointer[Held]
		rec := &unitAbortRecorder{}
		c, err := New(be, "owner-a", unitConfig(rec.fn(&held)))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ctx := context.Background()

		h1, err := c.Acquire(ctx, "k")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		// The backend would grant this: to it, a re-acquire by the holder and a
		// renewal are the same write. The refusal has to come from the Client.
		h2, err := c.Acquire(ctx, "k")
		if h2 != nil || !errors.Is(err, ErrAlreadyHeld) {
			t.Fatalf("second Acquire = (%v, %v), want (nil, ErrAlreadyHeld)", h2, err)
		}
		if n := len(be.attempts()); n != 1 {
			t.Fatalf("backend acquires = %d, want 1: the second acquire must not reach the backend", n)
		}
		if err := h1.Err(); err != nil {
			t.Fatalf("first lease ended: %v", err)
		}

		// Another key is a different claim.
		h3, err := c.Acquire(ctx, "other")
		if err != nil {
			t.Fatalf("Acquire on a second key: %v", err)
		}

		// Released, so the key is this Client's to take again.
		if err := h1.Release(ctx); err != nil {
			t.Fatalf("Release: %v", err)
		}
		h4, err := c.Acquire(ctx, "k")
		if err != nil {
			t.Fatalf("Acquire after Release: %v", err)
		}
		if err := h3.Release(ctx); err != nil {
			t.Fatalf("Release other: %v", err)
		}
		if err := h4.Release(ctx); err != nil {
			t.Fatalf("Release k: %v", err)
		}
	})
}

// A lost lease also drops the claim: an Abort that returns (a test seam, or an
// opt-out) must not leave the key permanently unacquirable.
func TestUnitAcquireAfterALossIsAllowedAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		be := &unitBackend{acquireFn: func(_ context.Context, attempt int) (Grant, error) {
			if attempt == 2 {
				return Grant{Holder: "owner-b", RetryAfter: time.Second}, nil
			}
			return unitGrant(unitTTL), nil
		}}
		var held atomic.Pointer[Held]
		rec := &unitAbortRecorder{}
		c, err := New(be, "owner-a", unitConfig(rec.fn(&held)))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		h, err := c.Acquire(context.Background(), "k")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		held.Store(h)

		unitWaitEnded(t, h)
		synctest.Wait()
		if err := h.Err(); !errors.Is(err, ErrLost) {
			t.Fatalf("Err() = %v, want ErrLost", err)
		}
		again, err := c.Acquire(context.Background(), "k")
		if err != nil {
			t.Fatalf("Acquire after the loss = %v, want the claim to have been dropped", err)
		}
		if err := again.Release(context.Background()); err != nil {
			t.Fatalf("Release: %v", err)
		}
	})
}

// RenewTimeout's whole purpose: a backend that never answers must not park the
// loop inside one call past the abort deadline. Without it the attempt at the
// first tick never returns, no further attempt is made, and the process holds a
// lease it cannot prove forever.
func TestUnitRenewTimeoutBoundsAWedgedAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		be := &unitBackend{acquireFn: func(ctx context.Context, attempt int) (Grant, error) {
			if attempt == 1 {
				return unitGrant(unitTTL), nil
			}
			<-ctx.Done() // never answers on its own
			return Grant{}, ctx.Err()
		}}
		var held atomic.Pointer[Held]
		rec := &unitAbortRecorder{}
		c, err := New(be, "owner-a", unitConfig(rec.fn(&held)))
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		start := time.Now()
		h, err := c.Acquire(context.Background(), "k")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		held.Store(h)

		unitWaitEnded(t, h)
		if got := time.Since(start); got != unitDeadline {
			t.Fatalf("lease ended after %s, want the abort deadline %s", got, unitDeadline)
		}
		synctest.Wait()
		if err := h.Err(); !errors.Is(err, ErrRenewalDeadline) {
			t.Fatalf("Err() = %v, want ErrRenewalDeadline", err)
		}
		// One wedged call per tick, each abandoned after RenewTimeout, so the
		// cadence is unchanged: 0s, 7s, 14s, 21s.
		got := be.attempts()
		want := []time.Duration{0, unitRenewEvery, 2 * unitRenewEvery, 3 * unitRenewEvery}
		if len(got) != len(want) {
			t.Fatalf("backend acquires = %d, want %d: the loop parked inside a wedged call", len(got), len(want))
		}
		for i, call := range got {
			if at := call.At.Sub(start); at != want[i] {
				t.Fatalf("attempt %d at %s, want %s", i, at, want[i])
			}
		}
		if calls := rec.snapshot(); len(calls) != 1 {
			t.Fatalf("abort fired %d times, want 1: %+v", len(calls), calls)
		}
	})
}

// When a tick and the abort deadline come ready in the same select, Go picks
// between them at random. The deadline has to win either way: a renewal
// confirmed after it proves ownership at a moment when another owner may
// already have taken over, written and released. Repeated because one run only
// samples one side of that coin flip.
func TestUnitDeadlineWinsATieWithATick(t *testing.T) {
	for range 24 {
		unitDeadlineTieRun(t)
	}
}

func unitDeadlineTieRun(t *testing.T) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		be := &unitBackend{acquireFn: func(_ context.Context, attempt int) (Grant, error) {
			if attempt == 1 {
				return unitGrant(unitTTL), nil
			}
			// Everything between fails, so the deadline is never pushed out and
			// the tick at 25s lands exactly on it.
			if attempt <= 5 {
				return Grant{}, errors.New("backend unavailable")
			}
			return unitGrant(unitTTL), nil
		}}
		var held atomic.Pointer[Held]
		rec := &unitAbortRecorder{}
		cfg := unitConfig(rec.fn(&held))
		cfg.RenewEvery = 5 * time.Second // divides the 25s deadline exactly
		if err := cfg.Validate(); err != nil {
			t.Fatalf("the tie config must be a legal one: %v", err)
		}
		c, err := New(be, "owner-a", cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		start := time.Now()
		h, err := c.Acquire(context.Background(), "k")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		held.Store(h)

		unitWaitEnded(t, h)
		if got := time.Since(start); got != unitDeadline {
			t.Fatalf("lease ended after %s, want the abort deadline %s", got, unitDeadline)
		}
		synctest.Wait()
		if err := h.Err(); !errors.Is(err, ErrRenewalDeadline) {
			t.Fatalf("Err() = %v, want ErrRenewalDeadline whichever case the select picked", err)
		}
		// The grant waiting at the coinciding tick is never even asked for.
		if n := len(be.attempts()); n != 5 {
			t.Fatalf("backend acquires = %d, want 5: a renewal was attempted at the deadline", n)
		}
		calls := rec.snapshot()
		if len(calls) != 1 || !errors.Is(calls[0].Cause, ErrRenewalDeadline) {
			t.Fatalf("abort calls = %+v, want one ErrRenewalDeadline", calls)
		}
	})
}

// The abort must land at or before the expiry the caller published downstream.
// The database stamps that expiry before the round trip completes, so the grant's
// latency has to come off the deadline: charged to SafetyMargin instead, an
// attempt begun just inside the deadline pushes the abort a whole RenewTimeout
// later — past the fence, with SafetyMargin == RenewTimeout.
func TestUnitAbortLandsBeforeThePublishedExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			ttl          = 30 * time.Second
			renewEvery   = 12 * time.Second
			renewTimeout = 5 * time.Second
			margin       = 5 * time.Second // equal to renewTimeout: no slack to hide in
			grantLatency = 2 * time.Second
		)
		be := &unitBackend{acquireFn: func(ctx context.Context, attempt int) (Grant, error) {
			if attempt == 1 {
				// Stamped when the statement runs, returned a round trip later.
				g := Grant{Granted: true, ExpiresAtMicros: time.Now().Add(ttl).UnixMicro()}
				time.Sleep(grantLatency)
				return g, nil
			}
			<-ctx.Done() // the node stopped answering
			return Grant{}, ctx.Err()
		}}
		var held atomic.Pointer[Held]
		rec := &unitAbortRecorder{}
		cfg := Config{
			TTL: ttl, RenewEvery: renewEvery, RenewTimeout: renewTimeout, SafetyMargin: margin,
			Abort: rec.fn(&held), Logger: unitLogger(),
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("config: %v", err)
		}
		c, err := New(be, "owner-a", cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		start := time.Now()
		h, err := c.Acquire(context.Background(), "k")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		held.Store(h)
		expiry := h.ExpiresAtMicros()

		unitWaitEnded(t, h)
		abortedAt := time.Now()
		synctest.Wait()
		if err := h.Err(); !errors.Is(err, ErrRenewalDeadline) {
			t.Fatalf("Err() = %v, want ErrRenewalDeadline", err)
		}
		if over := abortedAt.UnixMicro() - expiry; over > 0 {
			t.Fatalf("abort landed %s past the published expiry; the process was still writing after its own fence",
				time.Duration(over)*time.Microsecond)
		}
		// Measured from when the grant was stamped, not from when it arrived.
		if got, want := abortedAt.Sub(start), ttl-margin; got != want {
			t.Fatalf("aborted %s after the grant was stamped, want %s", got, want)
		}
	})
}

// A grant the database stamped at or after the expiry we already held is a fresh
// grant, not a renewal: the lease had lapsed, so a rival could have taken over,
// written and released in the gap, leaving no row for our acquire to conflict
// with. It looks exactly like a healthy renewal and must not be treated as one.
func TestUnitRegrantAfterAGapIsALoss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var firstExpiry int64
		be := &unitBackend{acquireFn: func(_ context.Context, attempt int) (Grant, error) {
			if attempt == 1 {
				g := unitGrant(unitTTL)
				firstExpiry = g.ExpiresAtMicros
				return g, nil
			}
			return Grant{Granted: true, ExpiresAtMicros: firstExpiry + unitTTL.Microseconds()}, nil
		}}
		var held atomic.Pointer[Held]
		rec := &unitAbortRecorder{}
		c, err := New(be, "owner-a", unitConfig(rec.fn(&held)))
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		start := time.Now()
		h, err := c.Acquire(context.Background(), "k")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		held.Store(h)

		unitWaitEnded(t, h)
		if got := time.Since(start); got != unitRenewEvery {
			t.Fatalf("lease ended after %s, want the first tick (%s)", got, unitRenewEvery)
		}
		synctest.Wait()
		if err := h.Err(); !errors.Is(err, ErrLeaseGap) {
			t.Fatalf("Err() = %v, want ErrLeaseGap", err)
		}
		calls := rec.snapshot()
		if len(calls) != 1 || !errors.Is(calls[0].Cause, ErrLeaseGap) || calls[0].CtxErr == nil {
			t.Fatalf("abort calls = %+v, want one ErrLeaseGap with the context already cancelled", calls)
		}
		if got := h.ExpiresAtMicros(); got != firstExpiry {
			t.Fatalf("ExpiresAtMicros = %d, want the fence left at %d rather than pushed out by a regrant", got, firstExpiry)
		}
		if n := len(be.attempts()); n != 2 {
			t.Fatalf("backend acquires = %d, want 2 (no attempt after the loss)", n)
		}
	})
}

// A backend error is not a refusal: there is nobody to wait for, so AcquireWait
// must report it instead of sleeping and trying again.
func TestUnitAcquireWaitPropagatesBackendErrors(t *testing.T) {
	boom := errors.New("backend unavailable")
	be := &unitBackend{acquireFn: func(context.Context, int) (Grant, error) { return Grant{}, boom }}
	c, err := New(be, "owner-a", unitConfig(func(string, error) { t.Error("abort must not fire") }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := c.AcquireWait(context.Background(), "k")
	if h != nil || !errors.Is(err, boom) {
		t.Fatalf("AcquireWait = (%v, %v), want (nil, the backend error)", h, err)
	}
	if n := len(be.attempts()); n != 1 {
		t.Fatalf("backend acquires = %d, want 1: a failed call must not be retried as a denial", n)
	}
}

// 40001 rolls the whole statement back, and a replayed acquire by the same owner
// is a renewal, so the replay is safe and invisible to the caller. Nothing else
// is replayed.
func TestUnitStoreRetryReplaysSerializationFailures(t *testing.T) {
	s, err := NewStore(unitPool(t), StoreConfig{})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	serialization := &pgconn.PgError{Code: "40001", Message: "conflict"}
	unique := &pgconn.PgError{Code: "23505", Message: "duplicate key"}
	plain := errors.New("dial tcp: connection refused")

	cases := []struct {
		name        string
		fail        error
		failFirstN  int
		wantCalls   int
		wantErr     error
		wantAtLeast time.Duration
	}{
		{"serialization failure is replayed to the attempt limit",
			serialization, stmtAttempts, stmtAttempts, serialization, 2 * stmtBackoff},
		{"one serialization failure then success", serialization, 1, 2, nil, stmtBackoff},
		{"a unique violation is not replayed", unique, stmtAttempts, 1, unique, 0},
		{"a dial error is not replayed", plain, stmtAttempts, 1, plain, 0},
		{"success is not retried", nil, 0, 1, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			start := time.Now()
			err := s.retry(context.Background(), func(context.Context) error {
				calls++
				if calls <= tc.failFirstN {
					return tc.fail
				}
				return nil
			})
			if calls != tc.wantCalls {
				t.Errorf("fn called %d times, want %d", calls, tc.wantCalls)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("retry = %v, want %v", err, tc.wantErr)
			}
			if elapsed := time.Since(start); elapsed < tc.wantAtLeast {
				t.Errorf("retry took %s, want at least %s of backoff between attempts", elapsed, tc.wantAtLeast)
			}
		})
	}

	t.Run("a cancelled context stops the replay", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		err := s.retry(ctx, func(context.Context) error {
			calls++
			return serialization
		})
		if calls != 1 || !errors.Is(err, context.Canceled) {
			t.Fatalf("retry = (%d calls, %v), want (1, context.Canceled)", calls, err)
		}
	})
}

func TestUnitOpenRejectsBadURLAndDeadAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if s, err := Open(ctx, "postgres://user@host:notaport/db", StoreConfig{}); err == nil {
		s.Close()
		t.Error("Open with an unparseable url = nil error, want a parse failure")
	}
	// Nothing listens here, so the ping fails and Open must not hand back a
	// Store over a pool it opened.
	s, err := Open(ctx, "postgres://pglease:pglease@127.0.0.1:1/pglease?sslmode=disable", StoreConfig{})
	if err == nil {
		s.Close()
		t.Fatal("Open against a dead address = nil error, want the ping failure")
	}
	if s != nil {
		t.Errorf("Open = %v on error, want a nil Store", s)
	}
}
