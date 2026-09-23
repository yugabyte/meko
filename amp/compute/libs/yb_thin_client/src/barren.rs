//! The stuck-continuation guard for a paging scan.
//!
//! Every loop that pages a scan to exhaustion needs it, and there are three:
//! [`crate::TsClient::read_scan`] here, `run_batched_block_scan` in `pageserver`,
//! and `fetch_chunks_thin` in `safekeeper`. #1055 keeps the two batched loops
//! separate — they accumulate different things, and one generic driver over both
//! costs more in parameters than the duplication does — but not this check: both
//! defects found in that code so far were in its checks, and both had to be
//! fixed twice (#1043 in the pageserver copy, #1063 in the safekeeper one).
//!
//! The batch's other invariant — that results pair positionally with the ops
//! sent — belongs to the read itself and is enforced in [`crate::TsClient::read`].
//! This one cannot be: the counter is per-op state spanning rounds, which only
//! the paging loop holds.
//!
//! It returns [`StuckContinuation`] rather than [`crate::YbError`] so a caller
//! can wrap it with the context naming the op it was scanning. `read_scan`, whose
//! signature is generic over `E: From<YbError>`, converts instead.

/// Consecutive empty-but-continuing pages tolerated per op before a scan fails
/// loudly.
///
/// Deliberately NOT a cap on total rounds. Paging to exhaustion is the ordinary
/// case for both drivers, so a round cap would false-fail a healthy scan; and an
/// op crossing tablets that hold no matching row returns empty pages with a
/// continuation, which is normal for a handful of rounds. Only a token that
/// never advances does it indefinitely (an earlier shim did exactly that and
/// spun at ~35% CPU forever).
pub const THIN_SCAN_BARREN_ROUNDS: usize = 64;

/// An op's continuation stopped advancing.
#[derive(Debug, Clone, thiserror::Error)]
#[error(
    "returned {barren_rounds} empty pages in a row while still continuing — the \
     continuation token is not advancing"
)]
pub struct StuckContinuation {
    pub barren_rounds: usize,
}

impl From<StuckContinuation> for crate::YbError {
    fn from(e: StuckContinuation) -> Self {
        crate::YbError::Other(e.to_string())
    }
}

/// Fold one paging round into an op's barren counter, failing once the op has
/// gone unproductive for too long to be believed.
///
/// `got` is the row count that round returned. Any row resets the counter, so
/// the number of PRODUCTIVE rounds is unbounded however slow the scan is. Only
/// an unbroken run of empty rounds under a live continuation is evidence a token
/// is not advancing.
pub fn check_barren_round(barren: &mut usize, got: usize) -> Result<(), StuckContinuation> {
    *barren = if got == 0 { *barren + 1 } else { 0 };
    if *barren > THIN_SCAN_BARREN_ROUNDS {
        return Err(StuckContinuation {
            barren_rounds: *barren,
        });
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A productive round must never count against the cap, no matter how many
    /// rounds the scan takes. Regression: the cap used to be on TOTAL rounds, so
    /// a fetch that returned rows on every round still died once it crossed the
    /// limit — which is what paging to exhaustion under load looks like, and it
    /// wedged replay behind a retry loop that could not heal because every
    /// reconnect reissued the identical fetch.
    #[test]
    fn productive_paging_rounds_are_unbounded() {
        let mut barren = 0usize;
        for round in 1..=(20 * THIN_SCAN_BARREN_ROUNDS) {
            assert!(
                check_barren_round(&mut barren, 1).is_ok(),
                "round {round} returned a row and must count as progress"
            );
        }
        assert_eq!(barren, 0, "a productive round leaves nothing to charge");
    }

    /// Any row resets the run, so empty rounds only accumulate while they are
    /// UNBROKEN. A scan crossing empty tablets between hits pages arbitrarily
    /// long without tripping the cap.
    #[test]
    fn a_single_row_resets_the_barren_run() {
        let mut barren = 0usize;
        for _ in 0..(4 * THIN_SCAN_BARREN_ROUNDS) {
            // Walk right up to the cap, then land one row.
            for _ in 0..THIN_SCAN_BARREN_ROUNDS {
                assert!(check_barren_round(&mut barren, 0).is_ok());
            }
            assert_eq!(barren, THIN_SCAN_BARREN_ROUNDS);
            assert!(check_barren_round(&mut barren, 1).is_ok());
            assert_eq!(barren, 0);
        }
    }

    /// The cap still fires on a genuinely stuck token: an unbroken run of empty
    /// rounds under a live continuation.
    #[test]
    fn an_unbroken_barren_run_trips_the_cap() {
        let mut barren = 0usize;
        for round in 1..=THIN_SCAN_BARREN_ROUNDS {
            assert!(
                check_barren_round(&mut barren, 0).is_ok(),
                "empty round {round} is still within the budget"
            );
        }
        assert!(
            check_barren_round(&mut barren, 0).is_err(),
            "one empty round past the budget must fail the scan"
        );
    }

    /// The bail reports the count that tripped it, not the bound. They differ by
    /// one, and naming the bound was #1043's second defect.
    #[test]
    fn the_bail_reports_the_count_that_tripped_it() {
        let mut barren = 0usize;
        let err = (0..=THIN_SCAN_BARREN_ROUNDS + 1)
            .find_map(|_| check_barren_round(&mut barren, 0).err())
            .expect("an unbroken empty run must trip the cap within the budget");
        assert_eq!(
            err.to_string(),
            format!(
                "returned {} empty pages in a row while still continuing — the \
                 continuation token is not advancing",
                THIN_SCAN_BARREN_ROUNDS + 1
            )
        );
    }
}
