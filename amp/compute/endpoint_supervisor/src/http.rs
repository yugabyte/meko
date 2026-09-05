//! The settings API: how a change in `amp_registry` reaches a running endpoint.
//!
//! Every setting an operator can change — the branch's `pg_settings` and
//! `write_token_settings`, the endpoint's `pg_settings`, `pageserver_settings`
//! and `safekeeper_settings` — is read by this process at startup and rendered
//! into the files its children start from. Without a way in, the only way to
//! apply an edit is to restart the endpoint and hope the boot read picks it up.
//!
//! So the controller calls `POST /v1/settings/reload` after it writes the row.
//! This process re-reads the registry, re-renders the configs, and applies each
//! change the cheapest way that actually applies it.
//!
//! For the compute that is usually a reload: Postgres says which settings can
//! change while it is running, and when the changed ones all can, `compute_ctl`
//! takes a SIGHUP, re-reads the spec and calls `pg_reload_conf()` — connections
//! stay up. A setting fixed at server start restarts the compute, and the reply
//! names it, so the interruption is explained rather than arbitrary.
//!
//! The pageserver and safekeeper restart on any change to their toml: they
//! read it once at start and have no reload path.
//!
//! **Only what moved.** Each rendered file is compared with what is on disk
//! before it is written, so a reload that renders identical bytes does nothing.
//! Most of a config comes from the environment and the resources, not from the
//! registry row someone just edited.
//!
//! **Before the row is written** there is `POST /v1/settings/check`. The
//! controller stores settings first and applies them second, which is right for
//! a value Postgres will take and wrong for one it will not: that one is stored,
//! never rendered, and only ever reported. The check lets the controller refuse
//! it up front instead — it asks the same question of the same binary, writes
//! nothing, and changes nothing, so it is safe to call on any endpoint at any
//! time.
//!
//! Separate from [`crate::control`] rather than folded into it: that socket is
//! a Unix socket in the run directory, deliberately unreachable from outside the
//! pod, and the caller here is the controller.

use std::sync::Arc;

use anyhow::{Context, Result};
use http_utils::endpoint;
use http_utils::error::ApiError;
use http_utils::json::json_response;
use hyper0::{Body, Request, Response, StatusCode};
use routerify::RouterBuilder;
use routerify::ext::RequestExt;

use crate::Env;

/// What a reload did.
#[derive(Debug, serde::Serialize)]
struct ReloadResponse {
    /// Services restarted onto the new config, because it could not be applied
    /// any other way. Empty means nothing changed, or everything that did was
    /// applied without an interruption.
    restarted: Vec<String>,
    /// Services that took the change in place. For the compute this means
    /// Postgres reloaded its config and kept its connections.
    reloaded: Vec<String>,
    /// The settings that forced a restart, when one was needed — Postgres fixes
    /// these at server start, so naming them explains a cost the caller
    /// otherwise has to guess at.
    restart_forced_by: Vec<String>,
    /// Settings that were stored but are not in force. Either Postgres refuses
    /// them outright — a name it does not recognise, a value it cannot parse —
    /// in which case they were deliberately not written, because a config file
    /// containing one does not load and the compute would not start; or nothing
    /// applies them, like an `internal` setting compiled into the binary.
    /// Naming them is the only way the caller learns the edit had no effect —
    /// the alternative is a success reply for a setting that never took.
    not_applied: Vec<crate::guc::NotApplied>,
}

/// The supervisor currently serving a lease, for the handler to restart through.
///
/// A slot rather than a field on [`State`]: the API is bound once for the
/// process, while the supervisor is rebuilt for each lease, so the one the
/// handler must talk to is whichever is serving when the request arrives. Empty
/// between leases, which is the honest answer — there is nothing running to
/// restart.
static SUPERVISOR: std::sync::Mutex<Option<crate::control::Shared>> = std::sync::Mutex::new(None);

/// Publish (or, with `None`, retire) the supervisor serving this lease.
pub fn set_supervisor(sup: Option<crate::control::Shared>) {
    *SUPERVISOR.lock().unwrap_or_else(|e| e.into_inner()) = sup;
}

fn supervisor() -> Option<crate::control::Shared> {
    SUPERVISOR.lock().unwrap_or_else(|e| e.into_inner()).clone()
}

/// Restart the services whose config moved, in the order they depend on each
/// other, and report what was restarted.
///
/// Only the ones that moved: a reload that renders byte-identical config
/// restarts nothing, which is what makes this safe to call after any edit
/// rather than only after one known to matter.
fn restart_stale(stale: &[&'static str]) -> Result<Vec<String>> {
    if stale.is_empty() {
        return Ok(Vec::new());
    }
    let sup = supervisor().context("no services are running to restart")?;
    let mut sup = sup
        .lock()
        .map_err(|_| anyhow::anyhow!("the supervisor lock is poisoned"))?;
    let mut restarted = Vec::with_capacity(stale.len());
    for name in stale {
        // Drained, not killed: this is a config change, not a fence loss, so the
        // service gets the same shutdown `pageserver restart` would give it.
        sup.stop_one(name, true)
            .and_then(|()| sup.start_one(name, None))
            .with_context(|| format!("restarting {name}"))?;
        restarted.push((*name).to_string());
    }
    Ok(restarted)
}

/// Apply a re-render: reload what can be reloaded, restart what cannot.
///
/// The pageserver and safekeeper read their toml once, at start, and have no
/// reload path — so a change to theirs is still a restart. The compute is the
/// one that had a cheaper option all along, and the expensive one: restarting
/// it takes Postgres and every connection with it.
fn apply(env: &Env, rendered: crate::Rendered) -> Result<ReloadResponse> {
    let mut restarted = Vec::new();
    let mut reloaded = Vec::new();
    let mut restart_forced_by = Vec::new();
    let mut not_applied = Vec::new();

    restarted.extend(restart_stale(&daemons_to_restart(&rendered))?);

    // From the render, not the classification: these were never written, so the
    // running server has nothing to say about them, and the caller needs to hear
    // it even when nothing else moved.
    not_applied.extend(rendered.refused_settings.iter().cloned());

    if rendered.compute {
        let l = &env.layout;
        let decision = match crate::reconfigure::classify_settings(
            &l.pg_bin().join("psql"),
            l.compute_port,
            &rendered.changed_settings,
        ) {
            Ok(sorted) => {
                // Settings nothing can apply are named back to the caller rather
                // than restarted for: a restart would not put them in force
                // either, so it would be an interruption that changes nothing.
                // Skipping any the render already refused. An endpoint
                // upgrading with a typo already in its spec drops it from the
                // render — which puts the name in `changed_settings`, where
                // the classifier finds no `pg_settings` row for it and would
                // call it inapplicable too. One name under two reasons reads
                // as two problems, and the render's reason is the specific
                // one.
                let fresh: Vec<_> = sorted
                    .inapplicable
                    .iter()
                    .filter(|name| !not_applied.iter().any(|n| &n.name == *name))
                    .map(|name| crate::guc::NotApplied {
                        name: name.clone(),
                        reason: crate::guc::NOTHING_APPLIES.to_string(),
                    })
                    .collect();
                not_applied.extend(fresh);
                crate::reconfigure::decide(&rendered.changed_settings, &sorted)
            }
            // Postgres could not be asked — it may be down, or mid-restart.
            // Restarting is the answer that is always correct, so take it and
            // say why rather than reloading on an assumption.
            //
            // "Could not be classified" is a different fact from "these are
            // postmaster settings", and only the second has a field. Filling
            // `restart_forced_by` here would tell the operator that the setting
            // they edited can only change at server start, which is exactly what
            // is not known.
            Err(e) => {
                tracing::warn!("could not classify the changed settings, restarting: {e:#}");
                crate::reconfigure::Apply::Restart {
                    because: Vec::new(),
                }
            }
        };
        match decision {
            // Nothing here: the spec changed, but not in its settings — a
            // password, an endpoint id, the storage block, the prewarm knobs.
            // A reload does NOT apply those on an amp spec: every spec this
            // process renders sets `skip_pg_catalog_updates`, so `reconfigure`
            // skips the catalogue work, and the prewarm fields are only read on
            // the start path. Reporting a reload here would report a change that
            // is on disk and not in force.
            crate::reconfigure::Apply::Nothing => {
                restarted.extend(restart_stale(&["compute_ctl"])?);
            }
            crate::reconfigure::Apply::Reload => {
                match reload_compute() {
                    Ok(()) => reloaded.push("compute_ctl".to_string()),
                    // Falling back to the restart keeps the change applied,
                    // which is what the caller asked for; it just costs what
                    // this function exists to avoid.
                    Err(e) => {
                        tracing::warn!("in-place reconfigure failed, restarting: {e:#}");
                        restarted.extend(restart_stale(&["compute_ctl"])?);
                    }
                }
            }
            crate::reconfigure::Apply::Restart { because } => {
                restart_forced_by = because;
                restarted.extend(restart_stale(&["compute_ctl"])?);
            }
        }
    }

    Ok(ReloadResponse {
        restarted,
        reloaded,
        restart_forced_by,
        not_applied,
    })
}

/// The daemons whose config moved, in the order they are restarted.
///
/// The safekeeper first: it is what the pageserver and the compute read from,
/// so restarting it after them would make them reconnect twice.
fn daemons_to_restart(rendered: &crate::Rendered) -> Vec<&'static str> {
    let mut out = Vec::new();
    if rendered.safekeeper {
        out.push("safekeeper");
    }
    if rendered.pageserver {
        out.push("pageserver");
    }
    out
}

/// Tell `compute_ctl` to re-read the spec this process just wrote.
///
/// SIGHUP, not an HTTP call. The external `/configure` is behind a JWT this
/// process cannot present — it renders the compute's own key set empty — and the
/// internal server binds a wildcard address with no auth layer, so exposing
/// `/configure` there would let anything able to reach the pod IP replace the
/// spec, including where this compute reads its data from. A signal carries no
/// payload and needs the pod's PID namespace, so the only thing it can apply is
/// the file already on disk.
fn reload_compute() -> Result<()> {
    let sup = supervisor().context("no services are running to reload")?;
    let mut sup = sup
        .lock()
        .map_err(|_| anyhow::anyhow!("the supervisor lock is poisoned"))?;
    sup.signal_one("compute_ctl", libc::SIGHUP)
}

/// Everything a request needs./// Everything a request needs. `Arc` because routerify hands it to each request
/// by reference and the server outlives any one of them.
pub struct State {
    pub env: Arc<Env>,
    pub fence_required: bool,
    /// One reload at a time. Two of them interleaving would have each writing
    /// the other's files, and both reporting a "changed" that reflects the
    /// race rather than the registry.
    reloading: tokio::sync::Mutex<()>,
}

impl State {
    pub fn new(env: Arc<Env>, fence_required: bool) -> Self {
        Self {
            env,
            fence_required,
            reloading: tokio::sync::Mutex::new(()),
        }
    }
}

async fn reload_handler(request: Request<Body>) -> Result<Response<Body>, ApiError> {
    let state = request
        .data::<Arc<State>>()
        .expect("state is attached to the router");

    // The caller brought every file, so the config on disk is theirs, not ours,
    // and re-rendering would replace a working config with one built from
    // defaults. A caller that brought only the daemon tomls is not refused: the
    // compute spec is this process's to render, so a reload re-renders that and
    // leaves the tomls alone.
    if state.env.prerender == crate::Prerender::Everything {
        return Err(ApiError::PreconditionFailed(
            "this endpoint runs on prerendered config; its settings are the caller's to re-render"
                .into(),
        ));
    }

    let _one_at_a_time = state.reloading.lock().await;

    // On a blocking thread, not this one. The registry read underneath builds
    // its own current-thread runtime and blocks on it, and `block_on` from
    // inside a runtime panics — this path is async where the boot path was a
    // plain synchronous stack, which is the whole of the difference.
    let (env, fence_required) = (state.env.clone(), state.fence_required);
    let rendered = tokio::task::spawn_blocking(move || crate::reload_config(&env, fence_required))
        .await
        .map_err(|e| ApiError::InternalServerError(anyhow::anyhow!("the reload task: {e}")))?
        // A registry that cannot be read is a 503, not a render: see
        // `reload_config` for why re-rendering anyway is the dangerous option.
        .map_err(|e| ApiError::ResourceUnavailable(format!("{e:#}").into()))?;

    let env = state.env.clone();
    let outcome = tokio::task::spawn_blocking(move || apply(&env, rendered))
        .await
        .map_err(|e| ApiError::InternalServerError(anyhow::anyhow!("the apply task: {e}")))?
        // The settings are stored and rendered by now, so a failure here leaves
        // a service running the previous config — worth an error rather than a
        // 200 that implies it took.
        .map_err(ApiError::InternalServerError)?;

    tracing::info!(
        restarted = ?outcome.restarted,
        reloaded = ?outcome.reloaded,
        "settings reloaded from the registry"
    );
    json_response(StatusCode::OK, outcome)
}

/// What a caller wants checked. Only Postgres settings: the daemons' keys are
/// validated against a generated key set in the controller, which needs no pod.
#[derive(Debug, serde::Deserialize)]
struct CheckRequest {
    #[serde(default)]
    pg_settings: std::collections::BTreeMap<String, String>,
}

/// How much body the check will read, and how many settings it will probe.
///
/// This is the only route on this port that takes a body and does work
/// proportional to it — a refused set costs one `postgres` per setting — and the
/// port is unauthenticated. Both bounds are far above any real settings map:
/// Postgres has a few hundred GUCs, and 64 KiB holds thousands of them.
const MAX_CHECK_BODY: usize = 64 * 1024;
const MAX_CHECK_SETTINGS: usize = 512;

/// The answer: what would not be applied, and why. Empty means all of them are
/// usable.
#[derive(Debug, serde::Serialize)]
struct CheckResponse {
    not_applied: Vec<crate::guc::NotApplied>,
}

/// Would Postgres take these settings?
///
/// Applies nothing: it puts them to the binary in a directory of its own and
/// reports. Safe to call on an endpoint running prerendered config, and
/// concurrently with a reload or with another check — each probe gets its own
/// directory ([`crate::guc`]), which is what makes that true rather than merely
/// claimed.
///
/// It does write, to that directory, and it spawns a `postgres` per setting when
/// a set is refused. Hence the bounds on the input above: this port is
/// unauthenticated.
async fn check_handler(mut request: Request<Body>) -> Result<Response<Body>, ApiError> {
    let state = request
        .data::<Arc<State>>()
        .expect("state is attached to the router")
        .clone();

    // Read to the cap rather than to the end: `to_bytes` on an unauthenticated
    // route would buffer whatever a caller cared to send.
    let mut buf: Vec<u8> = Vec::new();
    {
        use hyper0::body::HttpBody;
        let body = request.body_mut();
        while let Some(chunk) = body.data().await {
            let chunk = chunk
                .map_err(|e| ApiError::BadRequest(anyhow::anyhow!("reading the body: {e}")))?;
            if buf.len() + chunk.len() > MAX_CHECK_BODY {
                return Err(ApiError::BadRequest(anyhow::anyhow!(
                    "a settings check body may not exceed {MAX_CHECK_BODY} bytes"
                )));
            }
            buf.extend_from_slice(&chunk);
        }
    }
    let req: CheckRequest = serde_json::from_slice(&buf)
        .map_err(|e| ApiError::BadRequest(anyhow::anyhow!("not a settings check request: {e}")))?;
    if req.pg_settings.len() > MAX_CHECK_SETTINGS {
        return Err(ApiError::BadRequest(anyhow::anyhow!(
            "a settings check may name at most {MAX_CHECK_SETTINGS} settings, got {}",
            req.pg_settings.len()
        )));
    }

    // Spawns processes, so not on the reactor thread.
    let not_applied = tokio::task::spawn_blocking(move || {
        let l = &state.env.layout;
        crate::guc::screen(&l.pg_bin(), &l.run_dir, &req.pg_settings).refused
    })
    .await
    .map_err(|e| ApiError::InternalServerError(anyhow::anyhow!("the check task: {e}")))?;

    json_response(StatusCode::OK, CheckResponse { not_applied })
}

pub fn make_router(state: Arc<State>) -> RouterBuilder<Body, ApiError> {
    endpoint::make_router()
        .data(state)
        .post("/v1/settings/reload", |r| {
            endpoint::request_span(r, reload_handler)
        })
        .post("/v1/settings/check", |r| {
            endpoint::request_span(r, check_handler)
        })
}

/// Serve the settings API on `addr`, on a thread of its own.
///
/// Its own thread and its own runtime: this binary is synchronous, and the one
/// runtime it already has belongs to the write fence, whose calls are on a
/// deadline. An HTTP request must not queue behind a renewal, and a renewal must
/// never queue behind an HTTP request.
pub fn spawn(addr: std::net::SocketAddr, state: Arc<State>) -> Result<std::net::SocketAddr> {
    let listener = std::net::TcpListener::bind(addr).with_context(|| format!("bind {addr}"))?;
    let addr = listener.local_addr().unwrap_or(addr);
    // routerify's errors are boxed `dyn Error`, which `?` cannot lift into
    // anyhow directly.
    let router = make_router(state)
        .build()
        .map_err(|e| anyhow::anyhow!("building the settings router: {e}"))?;
    let service = routerify::RequestServiceBuilder::new(router)
        .map_err(|e| anyhow::anyhow!("building the settings request service: {e}"))?;

    std::thread::Builder::new()
        .name("settings-api".into())
        .spawn(move || {
            let rt = match tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
            {
                Ok(rt) => rt,
                Err(e) => {
                    tracing::error!("settings API: building its runtime: {e:#}");
                    return;
                }
            };
            // Inside `block_on`, not beside it: `Server::new` registers the
            // listener with the reactor (`TcpListener::from_std`), which panics
            // outside a runtime context. Built out here it would take the API
            // down on the first request, with nothing but a panic on stderr to
            // say why.
            let served = rt.block_on(async move {
                let server = http_utils::server::Server::new(Arc::new(service), listener, None)?;
                tracing::info!(%addr, "settings API listening");
                server
                    .serve(tokio_util::sync::CancellationToken::new())
                    .await
            });
            if let Err(e) = served {
                tracing::error!("settings API stopped: {e:#}");
            }
        })
        .context("spawning the settings API thread")?;
    Ok(addr)
}

#[cfg(test)]
mod tests {
    use super::daemons_to_restart;

    /// The API has to *come up*, which is not something compiling proves.
    ///
    /// `Server::new` registers the listener with the reactor, so building it
    /// outside a runtime context panics on the API thread — the process carries
    /// on, the port never answers, and the only evidence is a panic on stderr.
    /// Nothing short of connecting to it catches that.
    #[test]
    fn the_api_answers_once_spawned() {
        use std::io::{Read, Write};

        let state = std::sync::Arc::new(super::State::new(
            std::sync::Arc::new(crate::tests::test_env()),
            true,
        ));
        // Port 0: the kernel picks one, and `spawn` reports which.
        let addr = super::spawn(([127, 0, 0, 1], 0).into(), state).expect("spawn the API");

        // The thread binds before it serves; give it a moment to be listening.
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
        let mut last = String::new();
        while std::time::Instant::now() < deadline {
            if let Ok(mut sock) = std::net::TcpStream::connect(addr) {
                sock.set_read_timeout(Some(std::time::Duration::from_secs(5)))
                    .unwrap();
                // Any route will do: this is asking whether anything is serving,
                // not what it says.
                write!(
                    sock,
                    "GET /v1/nothing HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"
                )
                .unwrap();
                let mut buf = String::new();
                let _ = sock.read_to_string(&mut buf);
                if buf.starts_with("HTTP/") {
                    return;
                }
                last = buf;
            }
            std::thread::sleep(std::time::Duration::from_millis(100));
        }
        panic!("the settings API never answered on {addr}: {last:?}");
    }

    /// The check's HTTP contract, which the controller's pre-check depends on: a
    /// map in, a `{name, reason}` list out, naming only what Postgres refuses.
    ///
    /// Needs the compute's Postgres, so it skips where `pg_install` is not built
    /// — the same rule as the probe's own tests.
    #[test]
    fn the_check_names_what_postgres_refuses() {
        use std::io::{Read, Write};

        let pg_distrib =
            camino::Utf8PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../pg_install");
        if !pg_distrib.join("v18/bin/postgres").exists() {
            eprintln!("SKIP: no postgres at {pg_distrib}");
            return;
        }
        let run_dir = camino::Utf8PathBuf::try_from(std::env::temp_dir())
            .expect("temp dir is utf8")
            .join("endpoint-supervisor-check-http");
        std::fs::create_dir_all(&run_dir).expect("run dir");

        let mut env = crate::tests::test_env();
        env.layout.pg_distrib_dir = pg_distrib;
        env.layout.run_dir = run_dir;
        let state = std::sync::Arc::new(super::State::new(std::sync::Arc::new(env), true));
        let addr = super::spawn(([127, 0, 0, 1], 0).into(), state).expect("spawn the API");

        let body = r#"{"pg_settings":{"work_memm":"5MB","work_mem":"64MB"}}"#;
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(20);
        let mut last = String::new();
        while std::time::Instant::now() < deadline {
            if let Ok(mut sock) = std::net::TcpStream::connect(addr) {
                sock.set_read_timeout(Some(std::time::Duration::from_secs(10)))
                    .unwrap();
                write!(
                    sock,
                    "POST /v1/settings/check HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
                    body.len(),
                    body
                )
                .unwrap();
                let mut buf = String::new();
                let _ = sock.read_to_string(&mut buf);
                if buf.contains("200 OK") {
                    assert!(
                        buf.contains("work_memm") && buf.contains("unrecognized parameter"),
                        "the typo is not named with its reason: {buf}"
                    );
                    // The valid setting must not be reported: a check that
                    // refused everything would block every write.
                    assert!(
                        !buf.contains("\"work_mem\","),
                        "a usable setting was reported as refused: {buf}"
                    );
                    return;
                }
                last = buf;
            }
            std::thread::sleep(std::time::Duration::from_millis(100));
        }
        panic!("the check never answered on {addr}: {last:?}");
    }

    /// Nothing changed, nothing restarts — including when there is no supervisor
    /// to restart through. This is what makes a reload safe to call after any
    /// edit rather than only after one known to matter: most of a config comes
    /// from the environment and the resources, so most reloads render identical
    /// bytes and must cost nothing.
    #[test]
    fn an_unchanged_config_restarts_nothing() {
        super::set_supervisor(None);
        let restarted = super::restart_stale(&[]).expect("an empty set is not an error");
        assert!(restarted.is_empty());
    }

    /// With services to restart and no supervisor, it says so rather than
    /// reporting a restart that did not happen. Between leases there is nothing
    /// running, and a caller told "restarted: [pageserver]" would believe its
    /// change had taken.
    #[test]
    fn a_restart_without_a_supervisor_is_an_error() {
        super::set_supervisor(None);
        let err = super::restart_stale(&["pageserver"]).expect_err("must not claim success");
        assert!(
            format!("{err:#}").contains("no services are running"),
            "{err:#}"
        );
    }

    /// The daemons are restarted in dependency order — the safekeeper is what
    /// the pageserver and the compute read from, so it goes first.
    #[test]
    fn daemons_restart_in_dependency_order() {
        let order = daemons_to_restart(&crate::Rendered {
            pageserver: true,
            safekeeper: true,
            compute: true,
            changed_settings: Vec::new(),
            refused_settings: Vec::new(),
        });
        assert_eq!(order, vec!["safekeeper", "pageserver"]);

        // Only what moved: an unchanged daemon is not bounced for a change to
        // its neighbour.
        let order = daemons_to_restart(&crate::Rendered {
            pageserver: true,
            ..Default::default()
        });
        assert_eq!(order, vec!["pageserver"]);
        assert!(daemons_to_restart(&crate::Rendered::default()).is_empty());
    }
}
