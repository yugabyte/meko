package pglease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Backend is the durable side a Client drives. *Store implements it; tests
// substitute an in-memory fake.
//
// There is no Renew method: with the conditional upsert, a renewal IS an acquire
// by the current holder, which the rule grants unconditionally. Splitting them
// would create a second write path that has to agree with the first.
type Backend interface {
	Acquire(ctx context.Context, key, owner string, ttl time.Duration) (Grant, error)
	Release(ctx context.Context, key, owner string) (bool, error)
}

// Defaults for Config. The TTL is 15s against a 5s renewal interval, sized by
// what it has to survive rather than by what a healthy renewal needs: two
// renewals may fail outright before anything stops writing.
const (
	DefaultTTL          = 15 * time.Second
	DefaultRenewEvery   = 5 * time.Second
	DefaultRenewTimeout = 2 * time.Second
	DefaultSafetyMargin = 2 * time.Second
)

// minAcquireWait floors AcquireWait's sleep so a denial reporting "retry now"
// cannot turn the wait into a spin.
const minAcquireWait = 250 * time.Millisecond

// Config configures a Client. The zero value is valid and uses the defaults.
type Config struct {
	// TTL is the lease each acquire and renewal asks for. Must be in
	// [MinTTL, MaxTTL].
	TTL time.Duration

	// RenewEvery is how often a renewal is attempted.
	RenewEvery time.Duration

	// RenewTimeout bounds ONE renewal attempt, so a backend that never answers
	// cannot park the loop past its own abort deadline inside a call.
	RenewTimeout time.Duration

	// SafetyMargin is how far before the granted expiry the Client gives up. It
	// must cover one in-flight attempt, hence SafetyMargin >= RenewTimeout.
	SafetyMargin time.Duration

	// Abort ends the process on lease loss. nil means AbortProcess, so
	// forgetting to set it yields the safe behaviour and only an explicit
	// opt-out disables it.
	Abort AbortFunc

	// Logger receives renewal warnings and the loss error. nil discards them:
	// see normalize for why this is not slog.Default().
	Logger *slog.Logger
}

func (c Config) timingsUnset() bool {
	return c.TTL == 0 && c.RenewEvery == 0 && c.RenewTimeout == 0 && c.SafetyMargin == 0
}

func (c Config) normalize() Config {
	if c.timingsUnset() {
		c.TTL = DefaultTTL
		c.RenewEvery = DefaultRenewEvery
		c.RenewTimeout = DefaultRenewTimeout
		c.SafetyMargin = DefaultSafetyMargin
	}
	if c.Abort == nil {
		c.Abort = AbortProcess
	}
	if c.Logger == nil {
		// Never slog.Default(): a library must not pick up whatever global a
		// process happened to install, and pkg/ cannot reach internal/log. A
		// caller that wants these records supplies the logger; the default is
		// silence, which is honest rather than accidentally-correct.
		c.Logger = slog.New(slog.DiscardHandler)
	}
	return c
}

// Validate reports whether the timings can hold a lease. The wholly zero value
// is valid and means "the defaults"; once any timing is set the rest are checked
// as given, because a partially configured Client is one whose timings the
// caller is reasoning about.
func (c Config) Validate() error {
	if c.timingsUnset() {
		c = c.normalize()
	}
	switch {
	case c.TTL < MinTTL || c.TTL > MaxTTL:
		// MinTTL because the ttl reaches the database in whole milliseconds: a
		// sub-millisecond lease would be granted already expired.
		return fmt.Errorf("%w: ttl %s must be in [%s, %s]", ErrConfig, c.TTL, MinTTL, MaxTTL)
	case c.RenewEvery <= 0:
		return fmt.Errorf("%w: renew interval %s must be positive", ErrConfig, c.RenewEvery)
	case c.RenewTimeout <= 0:
		return fmt.Errorf("%w: renew timeout %s must be positive", ErrConfig, c.RenewTimeout)
	case c.SafetyMargin <= 0:
		return fmt.Errorf("%w: safety margin %s must be positive", ErrConfig, c.SafetyMargin)
	case c.RenewEvery >= c.TTL-c.SafetyMargin:
		// Strictly less, so at least one attempt fits before the abort deadline.
		// This does NOT guarantee a failed renewal is survivable: at the boundary
		// the next attempt is due 1ms before the deadline. Surviving two failures
		// is a property of the defaults (TestUnitDefaultCadenceIsCoherent), not of
		// every configuration this accepts.
		return fmt.Errorf("%w: renew interval %s must be less than ttl minus safety margin (%s)",
			ErrConfig, c.RenewEvery, c.TTL-c.SafetyMargin)
	case c.SafetyMargin < c.RenewTimeout:
		// Otherwise the deadline can only be reached from inside an attempt
		// that is still running.
		return fmt.Errorf("%w: safety margin %s must be at least the renew timeout %s",
			ErrConfig, c.SafetyMargin, c.RenewTimeout)
	}
	return nil
}

// abortIn is how long from now the fail-fast deadline must fire, given how long
// the grant it starts from took to answer. The latency is charged to us rather
// than assumed away: the database stamped the expiry before the round trip
// completed, and without subtracting it an attempt begun just before the
// deadline pushes the abort up to a whole RenewTimeout past the expiry the caller
// published downstream — which SafetyMargin == RenewTimeout leaves no room for.
func (c Config) abortIn(latency time.Duration) time.Duration {
	return c.TTL - c.SafetyMargin - latency
}

// Client acquires leases on behalf of one owner.
type Client struct {
	backend Backend
	owner   string
	cfg     Config

	// mu guards held: one entry per key with an acquire in flight or a live
	// Held. See reserve.
	mu   sync.Mutex
	held map[string]*Held
}

// New validates cfg and returns a Client. owner must identify one process: see
// NewOwner.
func New(b Backend, owner string, cfg Config) (*Client, error) {
	if b == nil {
		return nil, fmt.Errorf("%w: nil backend", ErrConfig)
	}
	if owner == "" {
		return nil, fmt.Errorf("%w: empty owner", ErrConfig)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Client{backend: b, owner: owner, cfg: cfg.normalize(), held: map[string]*Held{}}, nil
}

// Owner reports this Client's owner id.
func (c *Client) Owner() string { return c.owner }

// reserve claims key for this Client until the Held it produces ends, and
// reports ErrAlreadyHeld if it is claimed already.
//
// LOAD-BEARING: the backend grants unconditionally to the current holder, so a
// second acquire of a key this Client already holds is indistinguishable from a
// renewal and would return a second, independent Held over the one row. Either
// one's Release then deletes that row — deliberately skipping the grace — while
// the other still reports a live lease, so a third process is granted the key
// while the first caller is still writing. One holder per key per Client is what
// keeps "the row is mine" and "this Held is live" the same fact.
func (c *Client) reserve(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.held[key]; ok {
		return fmt.Errorf("%w: %q", ErrAlreadyHeld, key)
	}
	c.held[key] = nil // in flight; publish replaces it with the Held
	return nil
}

func (c *Client) publish(key string, h *Held) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held[key] = h
}

// release drops the claim. h is nil for an acquire that never produced a Held,
// and otherwise must match, so a Held ending after the key was acquired again
// cannot evict its successor.
func (c *Client) release(key string, h *Held) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.held[key]; ok && cur == h {
		delete(c.held, key)
	}
}

// Acquire takes the lease on key and starts renewing it. A refusal returns a
// *Denial, which unwraps to ErrNotAcquired: a Held cannot be built without the
// lease, so here the denial is an error even though Store.Acquire returns it as
// an answer. Acquiring a key this Client already holds returns ErrAlreadyHeld.
//
// ctx bounds only the acquire call. The returned lease's lifetime is Release,
// not ctx — deriving renewal from a request context would kill renewal the
// moment a handler returned and silently turn a held lease into an expiring one.
func (c *Client) Acquire(ctx context.Context, key string) (*Held, error) {
	if err := c.reserve(key); err != nil {
		return nil, err
	}
	start := time.Now()
	g, err := c.backend.Acquire(ctx, key, c.owner, c.cfg.TTL)
	latency := time.Since(start)
	if err != nil {
		c.release(key, nil)
		return nil, err
	}
	if !g.Granted {
		c.release(key, nil)
		return nil, &Denial{Key: key, Holder: g.Holder, RetryAfter: g.RetryAfter}
	}
	return c.hold(ctx, key, g, latency), nil
}

// AcquireWait retries until the lease is granted or ctx ends, sleeping the
// denial's RetryAfter floored at 250ms. A process that cannot get the lease is a
// process waiting its turn, not a failed one.
func (c *Client) AcquireWait(ctx context.Context, key string) (*Held, error) {
	for {
		h, err := c.Acquire(ctx, key)
		if err == nil {
			return h, nil
		}
		var d *Denial
		if !errors.As(err, &d) {
			return nil, err
		}
		wait := max(d.RetryAfter, minAcquireWait)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			// Both causes are reported: the caller may care that it was still
			// being denied, or that its own deadline ran out.
			return nil, fmt.Errorf("pglease: waiting for lease %q: %w: %w", key, ctx.Err(), err)
		case <-timer.C:
		}
	}
}

// hold starts the renewal goroutine for a granted lease. Both contexts drop the
// acquire ctx's cancellation and keep its values: the lease's lifetime is
// Release, not the call that took it.
func (c *Client) hold(ctx context.Context, key string, g Grant, latency time.Duration) *Held {
	base := context.WithoutCancel(ctx)
	holdCtx, cancelCause := context.WithCancelCause(base)
	renewCtx, cancelRenew := context.WithCancel(base)

	h := &Held{
		Key:         key,
		Owner:       c.owner,
		ctx:         holdCtx,
		cancelCause: cancelCause,
		cancelRenew: cancelRenew,
		stopped:     make(chan struct{}),
		client:      c,
	}
	h.expiry.Store(g.ExpiresAtMicros)
	c.publish(key, h)
	go h.renew(renewCtx, latency)
	return h
}

// Held is a lease this process holds, and the renewal goroutine keeping it.
type Held struct {
	Key   string
	Owner string

	expiry atomic.Int64

	// lost is set by lose before the renewal goroutine returns, so a Release
	// that is waiting on stopped observes it through that channel's close.
	lost atomic.Bool

	// cancelCause will cancel `ctx` and store the reason. This is exposed from Err() or Context()
	ctx         context.Context
	cancelCause context.CancelCauseFunc

	// Internal fields to manage the renewal goroutine and the Client that owns this Held.
	cancelRenew context.CancelFunc
	stopped     chan struct{}
	once        sync.Once

	// client is the Client that produced this Held, so it can drop the claim on
	// this key when the lease ends.
	client *Client
}

// Context is cancelled when the lease ends, whether by loss or by Release. Do
// the work under it and the work stops when the ownership does.
func (h *Held) Context() context.Context { return h.ctx }

// Err reports why the lease ended: nil while held, context.Canceled after
// Release, ErrLost after a takeover, ErrRenewalDeadline when no renewal could be
// confirmed in time, and ErrLeaseGap when a grant came back stamped at or after
// the expiry we held, which proves nothing about the interval in between.
func (h *Held) Err() error {
	if h.ctx.Err() == nil {
		return nil
	}
	return context.Cause(h.ctx)
}

// ExpiresAtMicros is the currently held lease value in epoch microseconds — the
// fence value to hand downstream.
func (h *Held) ExpiresAtMicros() int64 { return h.expiry.Load() }

// ExpiresAt is ExpiresAtMicros as a time.Time.
func (h *Held) ExpiresAt() time.Time { return microsToTime(h.expiry.Load()) }

// Release stops renewing and drops the row. It is idempotent and safe to defer.
// It does not return while the renewal goroutine can still touch the backend.
// The backend call is best effort: the row is owner-scoped, so a failure here
// only means the next holder waits out the TTL.
func (h *Held) Release(ctx context.Context) error {
	var err error
	h.once.Do(func() {
		// Stop the renew and wait for the goroutine to exit
		h.cancelRenew()
		<-h.stopped
		// Set the cause to nil as this is an explicit release
		h.cancelCause(nil)
		h.client.release(h.Key, h)
		if h.lost.Load() {
			// LOAD-BEARING: a lost lease must not delete the row. lose drops the
			// claim, so the caller may already hold a NEW lease on this key under
			// the same owner id — which is all token_release matches on — and the
			// delete would take the successor's row, grace-free, while its Held
			// still reported live.
			return
		}
		_, err = h.client.backend.Release(ctx, h.Key, h.Owner)
	})
	return err
}

// renew keeps the lease alive until Release, a takeover, or the abort deadline.
// latency is how long the grant it starts from took to answer.
func (h *Held) renew(renewCtx context.Context, latency time.Duration) {
	defer close(h.stopped)

	cfg := h.client.cfg
	ticker := time.NewTicker(cfg.RenewEvery)
	defer ticker.Stop()

	abortIn := cfg.abortIn(latency)
	// Fail fast timer to end the loop early if our ticker doesn't cause a loop first.
	deadline := time.NewTimer(abortIn)
	defer deadline.Stop()
	// If deadline and ticker fire at the same time, the select picks one at random.
	// Abort at is an extra check to give priority to the abort deadline; this guarantees ownership
	abortAt := time.Now().Add(abortIn)

	for {
		select {
		case <-renewCtx.Done():
			return
		case <-deadline.C:
			h.lose(ErrRenewalDeadline)
			return
		case <-ticker.C:
			if !time.Now().Before(abortAt) {
				h.lose(ErrRenewalDeadline)
				return
			}
		}

		start := time.Now()
		attemptCtx, cancelAttempt := context.WithTimeout(renewCtx, cfg.RenewTimeout)
		g, err := h.client.backend.Acquire(attemptCtx, h.Key, h.Owner, cfg.TTL)
		cancelAttempt()
		latency = time.Since(start)

		switch {
		case renewCtx.Err() != nil:
			return
		case err != nil:
			// A failed call is not a lost lease: the gap between RenewEvery and
			// TTL exists to absorb a blip, and the deadline timer bounds it.
			cfg.Logger.Warn("pglease: lease renewal failed",
				"key", h.Key, "owner", h.Owner, "error", err)
		case !g.Granted:
			h.lose(ErrLost)
			return
		case g.ExpiresAtMicros-cfg.TTL.Microseconds() >= h.expiry.Load():
			// The database stamped this grant at or after the expiry we already
			// held, so the lease had lapsed when it was written: in the gap
			// another owner could legally have taken over, written, and released
			// — leaving no row, which makes this acquire a plain insert that
			// succeeds and tells us nothing. A regrant is not a renewal. Both
			// values are database clock readings, so this holds however far the
			// local clock has drifted or this goroutine has been descheduled.
			h.lose(ErrLeaseGap)
			return
		default:
			h.pushExpiry(g.ExpiresAtMicros)
			if !deadline.Stop() {
				select {
				case <-deadline.C:
				default:
				}
			}
			abortIn = cfg.abortIn(latency)
			deadline.Reset(abortIn)
			abortAt = time.Now().Add(abortIn)
		}
	}
}

// pushExpiry raises the held expiry, never lowers it: pushes and retries can
// reorder, and taking the older of two would shorten the fence for no reason.
//
// Keeping the larger can outlive what the row says, when a renewal is served by
// a node whose clock lags the granting one. That is safe only because the lag is
// bounded by the database's own max_clock_skew_usec, which StoreConfig's default
// Skew mirrors: the stale fence then lapses no later than the instant takeover
// becomes legal. A Store configured with a smaller Skew gives up that margin.
func (h *Held) pushExpiry(us int64) {
	for {
		cur := h.expiry.Load()
		if us <= cur || h.expiry.CompareAndSwap(cur, us) {
			return
		}
	}
}

func (h *Held) lose(cause error) {
	h.client.cfg.Logger.Error("pglease: lease lost",
		"key", h.Key, "owner", h.Owner, "error", cause)
	// Mark BEFORE cancelling: the cancel is what wakes a caller written as
	// `defer h.Release(ctx)`, so the flag has to be visible to the Release that
	// wakes with it. Release reads it after <-h.stopped, which the renewal
	// goroutine closes on its way out, so the ordering holds regardless.
	//
	// LOAD-BEARING: this is a flag rather than consuming h.once. lose runs ON the
	// renewal goroutine, and Release holds the once while waiting for that
	// goroutine to exit — so taking the once here is a cycle: Release waits on
	// stopped, lose waits on the once, and stopped cannot close until lose
	// returns. Abort would never run, leaving the process alive holding a lease
	// it cannot prove, which is the one outcome this package exists to prevent.
	h.lost.Store(true)
	// Cancel BEFORE aborting so in-process work stops even when Abort is a hook
	// that returns, and drop the claim so such a caller can acquire the key
	// again rather than being told it still holds it.
	h.cancelCause(cause)
	h.client.release(h.Key, h)
	h.client.cfg.Abort(h.Key, cause)
}
