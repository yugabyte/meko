package pglease

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Bounds on the lease values, enforced by Store before any clock read or write,
// so a bad argument never mutates a row.
const (
	// MinTTL is the floor because durations reach the database in whole
	// milliseconds: a sub-millisecond ttl would truncate to zero and grant a
	// lease that has already expired.
	MinTTL      = time.Millisecond
	MaxTTL      = 300 * time.Second
	MaxSkew     = 3 * time.Second
	DefaultSkew = 500 * time.Millisecond
)

// DisableSkew turns the takeover grace off. It has to be asked for by name
// because StoreConfig's zero value means DefaultSkew: running with no grace is a
// claim that every node's clock agrees, and that is not something a caller should
// be able to assert by leaving a field unset.
const DisableSkew = time.Duration(-1)

const (
	connectTimeout = 3 * time.Second

	// Acquire and Release are single, key-scoped, idempotent statements, so a
	// racing loser that gets a serialization failure instead of a clean denial
	// must not surface it to a caller.
	stmtAttempts = 3
	stmtBackoff  = 50 * time.Millisecond
)

// StoreConfig configures a Store. The zero value is valid and means DefaultSkew.
type StoreConfig struct {
	// Skew is the grace a lapsed lease keeps against takeover by a DIFFERENT
	// owner. It gates takeover and probe visibility only — never a renewal by
	// the holder, and never a release.
	//
	// Zero means DefaultSkew (500ms), matching YugabyteDB's own
	// max_clock_skew_usec and the DEFAULT 500 that migration 0006's
	// token_acquire declares, so the lease inherits a bound the database already
	// enforces rather than adding an assumption of its own. Otherwise it must be
	// in [MinTTL, MaxSkew], or DisableSkew to turn the grace off.
	Skew time.Duration
}

// normalize resolves the zero value. Kept separate from Validate so both the
// constructor and the bounds check see the same effective skew.
func (c StoreConfig) normalize() StoreConfig {
	if c.Skew == 0 {
		c.Skew = DefaultSkew
	}
	return c
}

// effectiveSkew is what goes on the wire: DisableSkew is the caller's opt-out and
// reaches the database as a literal 0.
func (c StoreConfig) effectiveSkew() time.Duration {
	if c.Skew == DisableSkew {
		return 0
	}
	return c.Skew
}

// Validate reports whether Skew is in bounds.
func (c StoreConfig) Validate() error {
	if c.Skew == DisableSkew {
		return nil
	}
	c = c.normalize()
	// A sub-millisecond skew would truncate to zero on the wire and silently
	// mean "no grace" — which is DisableSkew's job to say out loud.
	if c.Skew < MinTTL || c.Skew > MaxSkew {
		return fmt.Errorf("%w: skew %s must be in [%s, %s], or DisableSkew",
			ErrConfig, c.Skew, MinTTL, MaxSkew)
	}
	return nil
}

// Grant is the answer to one Acquire. A denial is a Grant with Granted false,
// not an error: a denial is an answer, not a failure.
type Grant struct {
	Granted bool

	// ExpiresAtMicros is the lease value in epoch microseconds, read from the
	// database clock; see the package doc. 0 on a denial.
	ExpiresAtMicros int64

	// Holder is the owner that holds the lease: the caller on a grant, and on a
	// denial the incumbent as of this statement's snapshot, which a takeover
	// committed after it can already have superseded. "" when no row was
	// visible.
	Holder string

	// RetryAfter is how long until this owner may take over: the rest of the
	// lease plus the remaining grace, at least 1ms. Zero on a grant, and on a
	// denial with no visible row. Advisory, and for the same reason as Holder it
	// can undershoot badly — a rival's fresh takeover reads as the incumbent's
	// spent lease — so a caller must treat it as a hint and floor its own wait.
	// AcquireWait does.
	RetryAfter time.Duration
}

// ExpiresAt is ExpiresAtMicros as a time.Time, or the zero time on a denial.
func (g Grant) ExpiresAt() time.Time { return microsToTime(g.ExpiresAtMicros) }

// Lease is what the holder probe reports.
type Lease struct {
	Owner           string
	ExpiresAtMicros int64
	ExpiresIn       time.Duration // 0 once lapsed; truncated, never negative
	TakeoverIn      time.Duration // ExpiresIn plus the remaining grace
}

// ExpiresAt is ExpiresAtMicros as a time.Time.
func (l Lease) ExpiresAt() time.Time { return microsToTime(l.ExpiresAtMicros) }

func microsToTime(us int64) time.Time {
	if us == 0 {
		return time.Time{}
	}
	return time.UnixMicro(us)
}

// Store is the durable side of a lease: one row per key in
// amp_registry.resource_token, which migration 0006 owns and the compute write
// fence shares. The decision is 0006's conditional upsert, not ours.
type Store struct {
	pool     *pgxpool.Pool
	ownsPool bool
	cfg      StoreConfig
}

// NewStore wraps a pool the caller owns. Close does not close it.
func NewStore(pool *pgxpool.Pool, cfg StoreConfig) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("%w: nil pool", ErrConfig)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Store{pool: pool, cfg: cfg.normalize()}, nil
}

// Open dials url and returns a Store that owns the pool. It sets a 3s connect
// timeout and pings before returning. It sets no statement_timeout: every call
// is bounded by its context, which is the one mechanism that also aborts a query
// against a node that has stopped answering.
func Open(ctx context.Context, url string, cfg StoreConfig) (*Store, error) {
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("pglease: parse database url: %w", err)
	}
	poolCfg.ConnConfig.ConnectTimeout = connectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("pglease: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pglease: ping: %w", err)
	}
	s, err := NewStore(pool, cfg)
	if err != nil {
		pool.Close()
		return nil, err
	}
	s.ownsPool = true
	return s, nil
}

// Close closes the pool if this Store opened it.
func (s *Store) Close() {
	if s.ownsPool {
		s.pool.Close()
	}
}

// Skew reports the takeover grace in force — the resolved value, not the
// sentinel: DisableSkew reads back as 0, which is what reaches the database.
func (s *Store) Skew() time.Duration { return s.cfg.effectiveSkew() }

// Acquire grants, renews or refuses the lease on key in one statement.
//
// The holder renews with no grace; a different owner is granted only once
// expiry+skew has passed. ttl must be in [MinTTL, MaxTTL]; a rejected ttl never
// reaches the database.
func (s *Store) Acquire(ctx context.Context, key, owner string, ttl time.Duration) (Grant, error) {
	switch {
	case key == "":
		return Grant{}, fmt.Errorf("%w: empty key", ErrConfig)
	case owner == "":
		return Grant{}, fmt.Errorf("%w: empty owner", ErrConfig)
	case ttl < MinTTL || ttl > MaxTTL:
		return Grant{}, fmt.Errorf("%w: ttl %s must be in [%s, %s]", ErrConfig, ttl, MinTTL, MaxTTL)
	}

	var g Grant
	err := s.retry(ctx, func(ctx context.Context) error {
		var granted bool
		var token, retryMS *int64
		var holder *string
		row := s.pool.QueryRow(ctx, acquireStmt, key, owner, ttl.Milliseconds(), s.cfg.effectiveSkew().Milliseconds())
		if err := row.Scan(&granted, &token, &holder, &retryMS); err != nil {
			return err
		}
		g = grantFromRow(granted, token, holder, retryMS)
		if g.Granted {
			g.Holder = owner
		}
		return nil
	})
	if err != nil {
		return Grant{}, fmt.Errorf("pglease: acquire %q: %w", key, err)
	}
	return g, nil
}

// grantFromRow normalizes one token_acquire row. `granted` is the authority, not
// the presence of a token: the function always returns exactly one row, and only
// the granted one carries a token.
//
// A denial names a holder and a wait, but token_acquire re-reads to report them
// and tolerates the row vanishing in between, so both can be absent — reported as
// RetryAfter 0, meaning "retry now". A caller that sleeps on the hint must floor
// it, which is what AcquireWait does.
func grantFromRow(granted bool, token *int64, holder *string, retryMS *int64) Grant {
	if granted {
		g := Grant{Granted: true}
		if token != nil {
			g.ExpiresAtMicros = *token
		}
		return g
	}
	g := Grant{}
	if holder != nil {
		g.Holder = *holder
	}
	if retryMS != nil && *retryMS > 0 {
		g.RetryAfter = time.Duration(*retryMS) * time.Millisecond
	}
	return g
}

// Release drops the lease if owner still holds it, and reports whether a row was
// removed. Releasing a lease held by someone else, or one already released, is a
// no-op reporting false, not an error.
func (s *Store) Release(ctx context.Context, key, owner string) (bool, error) {
	var released bool
	err := s.retry(ctx, func(ctx context.Context) error {
		// The boolean the function returns is the answer; a row count would only
		// say that one row of output came back, which it always does.
		return s.pool.QueryRow(ctx, releaseStmt, key, owner).Scan(&released)
	})
	if err != nil {
		return false, fmt.Errorf("pglease: release %q: %w", key, err)
	}
	return released, nil
}

// Holder reports the current lease, or nil if key is free to a NEW owner: see
// holderStmt.
func (s *Store) Holder(ctx context.Context, key string) (*Lease, error) {
	var lease *Lease
	err := s.retry(ctx, func(ctx context.Context) error {
		var l Lease
		var expiresInMS, takeoverInMS int64
		row := s.pool.QueryRow(ctx, holderStmt, key, s.cfg.effectiveSkew().Milliseconds())
		switch err := row.Scan(&l.Owner, &l.ExpiresAtMicros, &expiresInMS, &takeoverInMS); {
		case errors.Is(err, pgx.ErrNoRows):
			lease = nil
			return nil
		case err != nil:
			return err
		}
		l.ExpiresIn = time.Duration(expiresInMS) * time.Millisecond
		l.TakeoverIn = time.Duration(takeoverInMS) * time.Millisecond
		lease = &l
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("pglease: holder %q: %w", key, err)
	}
	return lease, nil
}

// retry re-runs fn while the database reports SQLSTATE 40001, which rolls the
// statement back in full. A replayed acquire by the same owner is a renewal, so
// a replay cannot change the outcome the caller is told about.
func (s *Store) retry(ctx context.Context, fn func(context.Context) error) error {
	var err error
	for attempt := 0; attempt < stmtAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(jittered(stmtBackoff)):
			}
		}
		if err = fn(ctx); err == nil || sqlState(err) != "40001" {
			return err
		}
	}
	return err
}

// jittered spreads a fixed backoff over [d, 1.5d): the processes that collide
// here are the ones that raced for the same key or the same DDL, so retrying at
// the same offset would just reproduce the collision.
func jittered(d time.Duration) time.Duration {
	return d + time.Duration(rand.Int64N(int64(d)/2))
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
