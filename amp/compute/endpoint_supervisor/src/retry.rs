//! Bounded retry with exponential backoff, for the supervisor's own external
//! calls.
//!
//! This process is PID 1: an error it returns is a pod restart, so anything that
//! can fail for a reason that will pass on its own — a YB leader election, a
//! rolling restart, a spawn that hits EAGAIN under a tight cgroup — is retried
//! here rather than escalated. Bounded, because a retry that never gives up is an
//! endpoint that hangs instead of reporting, which is harder to diagnose.
//!
//! Only transient failures belong in a retry. Deterministic ones — a malformed
//! id, unparseable output — must be checked outside the loop, since repeating
//! them just multiplies the latency before the same error surfaces.

use std::time::Duration;

use anyhow::Result;

/// An attempt count and the delay curve between attempts.
#[derive(Debug, Clone, Copy)]
pub struct Backoff {
    /// Total attempts, including the first. Clamped to at least 1.
    pub attempts: u32,
    pub initial: Duration,
    /// Ceiling for a single delay, so the curve plateaus instead of exploding.
    pub max: Duration,
}

impl Backoff {
    /// Reading the registry overrides. Sized so the whole read stays inside a
    /// handful of seconds even in the worst case: an endpoint waiting on config
    /// has not started Postgres yet, so this delay is startup latency.
    pub const fn registry() -> Self {
        Self {
            attempts: 4,
            initial: Duration::from_millis(250),
            max: Duration::from_secs(2),
        }
    }

    /// Spawning a child. A failure here is `fork`/`exec` refusing, which is
    /// either transient memory pressure or a permanent bad argv; a few quick
    /// attempts separate the two without delaying a real failure much.
    pub const fn spawn() -> Self {
        Self {
            attempts: 3,
            initial: Duration::from_millis(200),
            max: Duration::from_secs(1),
        }
    }

    /// The delay before the attempt following `attempt` (1-based): `initial`
    /// doubled each time, capped at `max`.
    pub fn delay_after(&self, attempt: u32) -> Duration {
        let shift = attempt.saturating_sub(1).min(16);
        self.initial
            .checked_mul(1u32 << shift)
            .unwrap_or(self.max)
            .min(self.max)
    }
}

/// Runs `op` until it succeeds or the attempts run out, sleeping between tries.
///
/// `what` names the operation in the logs; every failed attempt is logged at
/// warn, so a retry that eventually succeeds still leaves evidence that the
/// dependency was flapping. The returned error is the **last** one, with the
/// attempt count attached.
pub fn retry<T>(what: &str, policy: &Backoff, mut op: impl FnMut() -> Result<T>) -> Result<T> {
    let attempts = policy.attempts.max(1);
    let mut last: Option<anyhow::Error> = None;
    for attempt in 1..=attempts {
        match op() {
            Ok(v) => {
                if attempt > 1 {
                    tracing::info!(what, attempt, "succeeded after retrying");
                }
                return Ok(v);
            }
            Err(e) => {
                if attempt < attempts {
                    let delay = policy.delay_after(attempt);
                    tracing::warn!(
                        what,
                        attempt,
                        of = attempts,
                        delay_ms = delay.as_millis(),
                        error = %e,
                        "attempt failed; retrying"
                    );
                    std::thread::sleep(delay);
                }
                last = Some(e);
            }
        }
    }
    Err(last
        .unwrap_or_else(|| anyhow::anyhow!("no attempt was made"))
        .context(format!("{what} failed after {attempts} attempts")))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::Cell;

    /// A policy with no real sleeping, so the tests stay fast.
    fn instant(attempts: u32) -> Backoff {
        Backoff {
            attempts,
            initial: Duration::from_millis(0),
            max: Duration::from_millis(0),
        }
    }

    #[test]
    fn a_first_attempt_success_does_not_retry() {
        let calls = Cell::new(0);
        let got = retry("x", &instant(4), || {
            calls.set(calls.get() + 1);
            Ok(7)
        })
        .unwrap();
        assert_eq!(got, 7);
        assert_eq!(calls.get(), 1);
    }

    /// The point of the module: a call that fails once and then works must be a
    /// success, not a pod restart.
    #[test]
    fn a_transient_failure_is_absorbed() {
        let calls = Cell::new(0);
        let got = retry("x", &instant(4), || {
            calls.set(calls.get() + 1);
            if calls.get() < 3 {
                anyhow::bail!("flap");
            }
            Ok("ok")
        })
        .unwrap();
        assert_eq!(got, "ok");
        assert_eq!(calls.get(), 3);
    }

    /// Bounded, not forever: the budget is spent and the last error surfaces.
    #[test]
    fn attempts_are_bounded_and_the_last_error_is_reported() {
        let calls = Cell::new(0);
        let err = retry("reading the thing", &instant(3), || -> Result<()> {
            calls.set(calls.get() + 1);
            anyhow::bail!("boom {}", calls.get())
        })
        .unwrap_err();
        assert_eq!(calls.get(), 3);
        let msg = format!("{err:#}");
        assert!(
            msg.contains("reading the thing failed after 3 attempts"),
            "{msg}"
        );
        assert!(msg.contains("boom 3"), "{msg}");
    }

    /// A zero-attempt policy must still call the operation once rather than
    /// silently skipping it and reporting a failure that never happened.
    #[test]
    fn a_zero_attempt_policy_still_runs_once() {
        let calls = Cell::new(0);
        let got = retry("x", &instant(0), || {
            calls.set(calls.get() + 1);
            Ok(1)
        })
        .unwrap();
        assert_eq!((got, calls.get()), (1, 1));
    }

    #[test]
    fn the_delay_doubles_then_plateaus_at_max() {
        let b = Backoff {
            attempts: 8,
            initial: Duration::from_millis(100),
            max: Duration::from_millis(400),
        };
        assert_eq!(b.delay_after(1), Duration::from_millis(100));
        assert_eq!(b.delay_after(2), Duration::from_millis(200));
        assert_eq!(b.delay_after(3), Duration::from_millis(400));
        assert_eq!(b.delay_after(4), Duration::from_millis(400));
        // A large attempt number must saturate, not overflow the shift.
        assert_eq!(b.delay_after(u32::MAX), Duration::from_millis(400));
    }

    /// The real policies must stay inside a startup budget a human would accept.
    #[test]
    fn the_shipped_policies_are_bounded_in_wall_clock() {
        for p in [Backoff::registry(), Backoff::spawn()] {
            let total: Duration = (1..p.attempts).map(|a| p.delay_after(a)).sum();
            assert!(total <= Duration::from_secs(5), "{p:?} sleeps {total:?}");
        }
    }
}
