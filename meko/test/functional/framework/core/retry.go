package core

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"log"
	"time"
)

// One retry absorbs a momentary capacity squeeze; 60s ≈ a GKE node add.
const (
	DefaultEnvRetries      = 1
	DefaultEnvRetryBackoff = 60 * time.Second
)

// AttemptInfo identifies one execution: same Group = same chain, highest N = current.
type AttemptInfo struct {
	Group  string // UUID minted at the FIRST attempt, reused by later attempts
	N      int    // 1-based attempt number
	Reason string // why the PREVIOUS attempt was retried; empty on N == 1
}

// RunAttempt is one full execution: RunOne, or RunOneOnEnv bound to a shared env.
type RunAttempt func(context.Context, Test, RunOptions) *Result

// ResolveEnvRetries resolves sched.env_retries, EXTRA attempts, 0 disables.
func ResolveEnvRetries(cfg *Config) int {
	if cfg == nil {
		return DefaultEnvRetries
	}
	n := cfg.GetInt("sched.env_retries", DefaultEnvRetries)
	if n < 0 {
		return 0
	}
	return n
}

// ResolveEnvRetryBackoff resolves sched.env_retry_backoff, the pause before a retry.
func ResolveEnvRetryBackoff(cfg *Config) time.Duration {
	if cfg == nil {
		return DefaultEnvRetryBackoff
	}
	d := cfg.GetDuration("sched.env_retry_backoff", DefaultEnvRetryBackoff)
	if d < 0 {
		return 0
	}
	return d
}

// RunWithEnvRetry retries a test that ended env_failed for a scheduling-class
// reason. Every attempt reports its own Result; only the FINAL one is returned.
func RunWithEnvRetry(ctx context.Context, test Test, opts RunOptions, attempt RunAttempt) *Result {
	group := newRetryGroup()
	opts.Attempt = AttemptInfo{Group: group, N: 1}
	res := attempt(ctx, test, opts)

	retries := ResolveEnvRetries(opts.Config)
	for n := 2; n <= retries+1; n++ {
		reason, ok := retryableReason(res)
		if !ok {
			return res
		}
		backoff := ResolveEnvRetryBackoff(opts.Config)
		log.Printf("RETRY %s: attempt %d ended env_failed (%s), %q; retrying in %s (attempt %d of %d)",
			test.Name, n-1, reason, res.ErrorSummary, backoff, n, retries+1)
		if !sleepCtx(ctx, backoff) {
			return res
		}
		next := opts
		next.Attempt = AttemptInfo{Group: group, N: n, Reason: reason}
		res = attempt(ctx, test, next)
	}
	return res
}

// retryableReason: only env_failed retries, `timed_out` spent its whole budget.
func retryableReason(r *Result) (string, bool) {
	if r == nil || r.Status != StatusEnvFailed {
		return "", false
	}
	reason, ok := SchedulingReason(r.ErrorSummary)
	if !ok {
		return "", false
	}
	return "env: " + reason, true
}

// FinalAttempts keeps the highest Attempt per RetryGroup, in the chain's original
// position. Every sink producing run-level TOTALS must filter through this.
func FinalAttempts(results []*Result) []*Result {
	out := make([]*Result, 0, len(results))
	at := make(map[string]int, len(results))
	for _, r := range results {
		if r == nil {
			continue
		}
		if r.RetryGroup == "" {
			out = append(out, r)
			continue
		}
		if i, seen := at[r.RetryGroup]; seen {
			if r.Attempt >= out[i].Attempt {
				out[i] = r
			}
			continue
		}
		at[r.RetryGroup] = len(out)
		out = append(out, r)
	}
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// newRetryGroup: crypto/rand, not the run seed, a replayed --seed must not
// collide two runs' chains.
func newRetryGroup() string {
	var b [16]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
