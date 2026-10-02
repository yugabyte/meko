//! The handshake between the pageserver and the safekeeper, run after both are
//! up and before `compute_ctl` starts.
//!
//! The safekeeper does not learn about a branch from its config. It has to be
//! told, over its HTTP API, at a starting LSN — and the only source of that LSN is
//! the pageserver, once its branch has finished coming up. So the sequence is
//! fixed: wait for the pageserver's branch to reach `Active`, read its
//! `last_record_lsn`, then create the branch on the safekeeper at that LSN.
//!
//! Skipping this leaves the safekeeper with no timeline for the branch, and the
//! walproposer opens a stream against nothing. Nothing earlier in startup catches
//! it: both daemons are running and healthy, so the failure surfaces as a compute
//! that cannot start, one step removed from its cause.

use std::time::{Duration, Instant};

use anyhow::{Context, Result, bail};
use utils::write_fence::{WriteToken, WriteTokenRequest, WriteTokenResponse};

/// How long the pageserver's branch may take to reach `Active`. Generous: on a
/// Bootstrap recipe this runs `initdb`, and on a Branch recipe it forks the
/// ancestor, neither of which is instant.
pub const BRANCH_ACTIVE_TIMEOUT: Duration = Duration::from_secs(180);
pub const BRANCH_ACTIVE_INTERVAL: Duration = Duration::from_millis(500);

/// Timeout for the branch control calls, which block on the far side's startup
/// rather than answering promptly. `GlobalTimelines::create` waits out the
/// safekeeper's load behind a deadline of its own, so the shared client's 5 s
/// killed the call whenever that load needed the wait — fatally, since a dropped
/// request is not the retryable `StillLoading` the server would have returned
/// (#831). Well past the server's own deadline, so the server gets to answer.
pub const SLOW_CONTROL_TIMEOUT: Duration = Duration::from_secs(15 * 60);

/// A shared blocking client. Built once: `reqwest::blocking` starts a runtime per
/// client, and the readiness probes call this hundreds of times.
///
/// Its 5 s is a *probe* budget. Calls that can legitimately block longer override
/// it per request with [`SLOW_CONTROL_TIMEOUT`] rather than taking a second
/// client, which would mean a second runtime.
pub fn client() -> &'static reqwest::blocking::Client {
    static C: std::sync::OnceLock<reqwest::blocking::Client> = std::sync::OnceLock::new();
    C.get_or_init(|| {
        reqwest::blocking::Client::builder()
            .timeout(Duration::from_secs(5))
            .build()
            .expect("a blocking client with only a timeout set cannot fail to build")
    })
}

/// A GET returning 2xx, which is what "up" means for the pod's HTTP services.
///
/// A TCP connect is not enough: the port binds before the server can serve, so a
/// connect-only probe would let the next service start against a dependency that
/// is not ready.
pub fn http_ok(host: &str, port: u16, path: &str) -> bool {
    let url = format!("http://{host}:{port}{path}");
    client()
        .get(&url)
        .send()
        .map(|r| r.status().is_success())
        .unwrap_or(false)
}

/// Blocks until the pageserver reports the branch `Active`, then returns its
/// `last_record_lsn` — the LSN the safekeeper's branch must start at.
pub fn wait_branch_active(
    host: &str,
    pageserver_http_port: u16,
    branch_id: &str,
    timeout: Duration,
    interval: Duration,
) -> Result<String> {
    let url = format!("http://{host}:{pageserver_http_port}/v1/branch/{branch_id}");
    let deadline = Instant::now() + timeout;
    let mut logged = false;
    loop {
        if crate::supervise::shutdown_requested() {
            bail!("shutdown requested while waiting for the branch to become Active");
        }
        // Bounded by what is left of `timeout`: the 5 s probe budget is wrong here
        // too, but a request outliving the loop's own deadline would make that
        // deadline unenforceable.
        let budget = deadline
            .saturating_duration_since(Instant::now())
            .min(SLOW_CONTROL_TIMEOUT);
        match branch_state(&url, budget) {
            Ok(Some((state, lsn))) if state == "Active" => {
                tracing::info!(branch_id, %lsn, "pageserver branch is Active");
                return Ok(lsn);
            }
            Ok(Some((state, _))) => {
                if !logged {
                    tracing::info!(branch_id, %state, "waiting for the pageserver branch");
                    logged = true;
                }
            }
            Ok(None) => {
                if !logged {
                    tracing::info!(branch_id, "waiting for the pageserver branch to appear");
                    logged = true;
                }
            }
            // Transient while the pageserver is still opening its HTTP listener.
            Err(e) => tracing::debug!("branch state not readable yet: {e:#}"),
        }
        if Instant::now() >= deadline {
            bail!("pageserver branch {branch_id} was not Active within {timeout:?}");
        }
        std::thread::sleep(interval);
    }
}

/// `(state, last_record_lsn)`, or `None` when the pageserver has no such branch
/// yet. `last_record_lsn` defaults to `0/0`, which is what a branch with no
/// records reports.
fn branch_state(url: &str, timeout: Duration) -> Result<Option<(String, String)>> {
    let resp = client()
        .get(url)
        .timeout(timeout)
        .send()
        .context("GET branch")?;
    if resp.status() == reqwest::StatusCode::NOT_FOUND {
        return Ok(None);
    }
    if !resp.status().is_success() {
        bail!("GET branch returned {}", resp.status());
    }
    let body: serde_json::Value = resp.json().context("branch response is not JSON")?;
    let Some(state) = body.get("state").and_then(|v| v.as_str()) else {
        return Ok(None);
    };
    let lsn = body
        .get("last_record_lsn")
        .and_then(|v| v.as_str())
        .unwrap_or("0/0")
        .to_string();
    Ok(Some((state.to_string(), lsn)))
}

/// How long to keep re-asking the safekeeper while it answers "still loading",
/// and the gap between asks. The budget is the same [`SLOW_CONTROL_TIMEOUT`] a
/// single call gets — it is spent on the same thing, and split across attempts
/// rather than sunk into one.
///
/// Bounded rather than forever — this runs on PID 1's startup path, and an
/// endpoint that never reports is harder to diagnose than one that says what it
/// gave up on.
pub const BRANCH_CREATE_TIMEOUT: Duration = SLOW_CONTROL_TIMEOUT;
pub const BRANCH_CREATE_INTERVAL: Duration = Duration::from_secs(2);
/// How often the wait says so. Every ask would be hundreds of identical lines
/// across the full budget; silence would leave a slow start looking like a hang.
const LOAD_LOG_EVERY: Duration = Duration::from_secs(30);

/// Tells the safekeeper about the branch, at the LSN the pageserver reported.
///
/// An already-existing branch is success, not failure: this runs again on every
/// pod restart, and the safekeeper keeps the branch across one.
///
/// A safekeeper that has not finished loading the timeline answers 503 (see
/// `GlobalTimelines::create`). That is "ask again shortly", not a failure: the
/// load is in flight and each ask re-enters it, so this waits it out. Treating it
/// as fatal instead takes down both daemons, and since the restart begins the load
/// again from nothing, every attempt hits the same wall — the branch can never be
/// given an endpoint again (#874).
pub fn create_safekeeper_branch(
    host: &str,
    safekeeper_http_port: u16,
    branch_id: &str,
    pg_version_major: u32,
    start_lsn: &str,
    timeout: Duration,
    interval: Duration,
) -> Result<()> {
    let url = format!("http://{host}:{safekeeper_http_port}/v1/branch");
    let body = serde_json::json!({
        "branch_id": branch_id,
        // The safekeeper holds this in memory for the greeting check, which
        // compares major versions; the durable pg_version is the
        // controller-seeded one it reads back on restart.
        "pg_version": pg_version_major * 10_000,
        "start_lsn": start_lsn,
    });
    let deadline = Instant::now() + timeout;
    let mut last_log: Option<Instant> = None;
    loop {
        if crate::supervise::shutdown_requested() {
            bail!("shutdown requested while creating the branch on the safekeeper");
        }
        // Nothing else reads this until the handshake returns — `poll_once` does
        // not run yet — while the renewals keep going in the background. Without
        // this check a pod that lost the branch's lease mid-wait would keep asking
        // for the full budget before anyone noticed.
        if crate::fence::lost() {
            bail!("the write lease was lost while creating the branch on the safekeeper");
        }
        // Per attempt, not the whole budget: a request that hangs must not push the
        // handshake past `timeout` (the same clamp `wait_branch_active` uses).
        let attempt_budget = deadline
            .saturating_duration_since(Instant::now())
            .min(SLOW_CONTROL_TIMEOUT)
            // A budget that ran out during the sleep would fail the request as a
            // transport error and lose the "still loading" diagnosis. Leave room
            // for one last ask, whose reply the deadline check below turns into
            // that message.
            .max(Duration::from_secs(1));
        let resp = client()
            .post(&url)
            .timeout(attempt_budget)
            .json(&body)
            .send()
            .context("POST safekeeper branch")?;
        let status = resp.status();
        if status.is_success() {
            tracing::info!(branch_id, start_lsn, "created the branch on the safekeeper");
            return Ok(());
        }
        if status == reqwest::StatusCode::CONFLICT {
            tracing::info!(branch_id, "branch already exists on the safekeeper");
            return Ok(());
        }
        let detail = resp.text().unwrap_or_default();
        let detail: String = detail.chars().take(200).collect();
        if status != reqwest::StatusCode::SERVICE_UNAVAILABLE {
            bail!("safekeeper branch create returned {status}: {detail}");
        }
        if Instant::now() >= deadline {
            bail!(
                "the safekeeper was still loading branch {branch_id} after {timeout:?}: {detail}"
            );
        }
        let now = Instant::now();
        if last_log.is_none_or(|at| now.duration_since(at) >= LOAD_LOG_EVERY) {
            tracing::info!(
                branch_id,
                "the safekeeper is still loading the timeline; asking again"
            );
            last_log = Some(now);
        }
        std::thread::sleep(interval);
    }
}

/// Hands a daemon the endpoint's write token.
///
/// Returns whether that daemon's fence is actually armed, which is how the
/// supervisor discovers it is pushing tokens at a service that will not act on
/// them. See `compute/architecture/compute-fencing.md`.
pub fn post_write_token(url: &str, token: WriteToken) -> Result<bool> {
    let resp = client()
        .post(url)
        .json(&WriteTokenRequest { token })
        .send()
        .context("POST write token")?;
    let status = resp.status();
    if !status.is_success() {
        let detail = resp.text().unwrap_or_default();
        bail!(
            "write token push returned {status}: {}",
            detail.chars().take(200).collect::<String>()
        );
    }
    let body: WriteTokenResponse = resp.json().context("decoding the write-token response")?;
    Ok(body.armed)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::net::TcpListener;
    use std::sync::mpsc;

    /// A one-shot HTTP server that replies with `status` and `body` to every
    /// request, and reports back what it received.
    fn serve(replies: Vec<(u16, String)>) -> (u16, mpsc::Receiver<String>) {
        serve_after(Duration::ZERO, replies)
    }

    /// `serve`, but the reply is withheld for `delay` — a server that is up and
    /// working, just slow, which is what a safekeeper waiting out its startup
    /// load looks like from the client side.
    fn serve_after(delay: Duration, replies: Vec<(u16, String)>) -> (u16, mpsc::Receiver<String>) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let (tx, rx) = mpsc::channel();
        std::thread::spawn(move || {
            for (code, body) in replies {
                let Ok((mut sock, _)) = listener.accept() else {
                    return;
                };
                let mut buf = [0u8; 4096];
                let n = sock.read(&mut buf).unwrap_or(0);
                let _ = tx.send(String::from_utf8_lossy(&buf[..n]).to_string());
                std::thread::sleep(delay);
                let reason = if code == 200 { "OK" } else { "X" };
                let resp = format!(
                    "HTTP/1.1 {code} {reason}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                    body.len()
                );
                let _ = sock.write_all(resp.as_bytes());
            }
        });
        (port, rx)
    }

    #[test]
    fn a_status_probe_needs_a_200_not_just_an_open_port() {
        let (port, _rx) = serve(vec![(200, "{}".into())]);
        assert!(http_ok("127.0.0.1", port, "/v1/status"));
        // Listening but erroring is *not* ready — the case a TCP probe misses.
        let (port, _rx) = serve(vec![(503, "{}".into())]);
        assert!(!http_ok("127.0.0.1", port, "/v1/status"));
    }

    /// The path is per-service, so it has to reach the request rather than being
    /// hardcoded, since a service could serve `/status` rather than `/v1/status`.
    #[test]
    fn the_probe_requests_the_path_it_is_given() {
        let (port, rx) = serve(vec![(200, "{}".into())]);
        assert!(http_ok("127.0.0.1", port, "/status"));
        let req = rx.recv_timeout(Duration::from_secs(5)).unwrap();
        assert!(req.starts_with("GET /status "), "{req}");
    }

    /// Regression for #831: the branch create shares the client whose 5 s exists
    /// for readiness probes, and the safekeeper deliberately blocks this call
    /// while it loads (up to 60 s server-side). A reply slower than the probe
    /// budget must still be waited for — before the fix this failed at exactly
    /// 5.000 s with `error sending request`, which the supervisor treats as fatal,
    /// so the endpoint crashlooped and the branch could not be given an endpoint
    /// at all.
    #[test]
    fn a_slow_branch_create_outlives_the_probe_budget() {
        // `create_safekeeper_branch` reads `fence::lost()`, which other tests set.
        let _fence = crate::fence::lost_lock();
        let (port, _rx) = serve_after(Duration::from_secs(6), vec![(200, "{}".into())]);
        let started = Instant::now();
        create_safekeeper_branch(
            "127.0.0.1",
            port,
            "abc123",
            18,
            "0/16B3F48",
            Duration::from_secs(30),
            Duration::from_millis(10),
        )
        .expect("a slow-but-successful create must not be cut off at 5s");
        assert!(
            started.elapsed() >= Duration::from_secs(5),
            "the server replied too fast to prove anything: {:?}",
            started.elapsed()
        );
    }

    #[test]
    fn a_closed_port_is_not_ready() {
        assert!(!http_ok("127.0.0.1", 1, "/v1/status"));
    }

    /// The LSN is the whole point of the wait: the safekeeper's branch has to
    /// start where the pageserver actually is.
    #[test]
    fn waiting_returns_the_lsn_once_the_branch_is_active() {
        let (port, _rx) = serve(vec![
            (200, r#"{"state":"Loading","last_record_lsn":"0/0"}"#.into()),
            (
                200,
                r#"{"state":"Active","last_record_lsn":"0/16B3F48"}"#.into(),
            ),
        ]);
        let got = wait_branch_active(
            "127.0.0.1",
            port,
            "9a054903-242f-46bc-a0c9-cc6b91a37e4c",
            Duration::from_secs(10),
            Duration::from_millis(10),
        )
        .unwrap();
        assert_eq!(got, "0/16B3F48");
    }

    /// A branch that never becomes Active must time out rather than block
    /// startup forever.
    #[test]
    fn waiting_times_out_if_the_branch_never_activates() {
        let (port, _rx) = serve(vec![
            (200, r#"{"state":"Loading"}"#.into()),
            (200, r#"{"state":"Loading"}"#.into()),
            (200, r#"{"state":"Loading"}"#.into()),
        ]);
        let err = wait_branch_active(
            "127.0.0.1",
            port,
            "b",
            Duration::from_millis(60),
            Duration::from_millis(10),
        )
        .expect_err("must not report Active");
        assert!(format!("{err:#}").contains("was not Active"), "{err:#}");
    }

    /// The request the safekeeper receives is the contract: the branch id, the
    /// LSN the pageserver reported, and the major version scaled the way the
    /// greeting check expects.
    #[test]
    fn the_branch_create_sends_the_lsn_and_scaled_version() {
        let _fence = crate::fence::lost_lock();
        let (port, rx) = serve(vec![(200, "{}".into())]);
        create_safekeeper_branch(
            "127.0.0.1",
            port,
            "abc123",
            18,
            "0/16B3F48",
            Duration::from_secs(30),
            Duration::from_millis(10),
        )
        .unwrap();
        let req = rx.recv_timeout(Duration::from_secs(5)).unwrap();
        assert!(req.starts_with("POST /v1/branch"), "{req}");
        assert!(req.contains(r#""branch_id":"abc123""#), "{req}");
        assert!(req.contains(r#""start_lsn":"0/16B3F48""#), "{req}");
        assert!(req.contains(r#""pg_version":180000"#), "{req}");
    }

    /// Every pod restart re-runs this, and the safekeeper keeps the branch, so
    /// an existing branch must not fail startup.
    #[test]
    fn an_existing_branch_is_success() {
        let _fence = crate::fence::lost_lock();
        let (port, _rx) = serve(vec![(409, "already exists".into())]);
        create_safekeeper_branch(
            "127.0.0.1",
            port,
            "abc123",
            18,
            "0/0",
            Duration::from_secs(30),
            Duration::from_millis(10),
        )
        .unwrap();
    }

    /// Regression for #874: a safekeeper still loading the timeline answers 503,
    /// which means "ask again shortly" — the load is in flight and each ask
    /// re-enters it. Treating it as fatal takes down both daemons, and the restart
    /// starts the load over, so the branch can never be given an endpoint again.
    #[test]
    fn a_still_loading_safekeeper_is_waited_out() {
        let _fence = crate::fence::lost_lock();
        let loading = r#"{"msg":"timeline abc123 has not been loaded yet"}"#.to_string();
        let (port, _rx) = serve(vec![
            (503, loading.clone()),
            (503, loading),
            (200, "{}".into()),
        ]);
        create_safekeeper_branch(
            "127.0.0.1",
            port,
            "abc123",
            18,
            "0/16B3F48",
            Duration::from_secs(30),
            Duration::from_millis(10),
        )
        .expect("a load still in progress must be waited out, not fatal");
    }

    /// Bounded, though: a load that never finishes has to report what it gave up
    /// on rather than hang startup silently.
    #[test]
    fn waiting_out_the_load_is_bounded() {
        let _fence = crate::fence::lost_lock();
        let loading = r#"{"msg":"timeline abc123 has not been loaded yet"}"#.to_string();
        // More replies than the budget allows attempts: a stub that runs out
        // would hand the loop a connection error instead of another 503, which is
        // a different path than the one under test.
        let (port, _rx) = serve(vec![(503, loading); 12]);
        let err = create_safekeeper_branch(
            "127.0.0.1",
            port,
            "abc123",
            18,
            "0/0",
            Duration::from_millis(30),
            Duration::from_millis(10),
        )
        .expect_err("an endless load must not block startup forever");
        assert!(format!("{err:#}").contains("still loading"), "{err:#}");
    }

    /// Any other failure must surface: a safekeeper without the branch cannot
    /// serve the walproposer, and starting the compute anyway hides the cause.
    #[test]
    fn a_real_failure_is_an_error() {
        let _fence = crate::fence::lost_lock();
        let (port, _rx) = serve(vec![(500, "boom".into())]);
        let err = create_safekeeper_branch(
            "127.0.0.1",
            port,
            "abc123",
            18,
            "0/0",
            Duration::from_secs(30),
            Duration::from_millis(10),
        )
        .expect_err("500 must not be swallowed");
        assert!(format!("{err:#}").contains("500"), "{err:#}");
    }
}
