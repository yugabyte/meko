//! The settings API: how a change in `amp_registry` reaches a running endpoint.
//!
//! Every setting an operator can change — the branch's `pg_settings` and
//! `write_token_settings`, the endpoint's `pg_settings`, `pageserver_settings`
//! and `safekeeper_settings` — is read by this process at startup and rendered
//! into the files its children start from. Without a way in, the only way to
//! apply an edit is to restart the endpoint and hope the boot read picks it up.
//!
//! So the controller calls `POST /v1/settings/reload` after it writes the row.
//! This process re-reads the registry, re-renders the configs, applies what it
//! can in place — the lease timings are its own — and restarts the services
//! whose config actually moved.
//!
//! **Only the ones that moved.** Each rendered file is compared with what is on
//! disk before it is written, so a reload that renders identical bytes restarts
//! nothing. That is what makes this safe to call after any edit rather than only
//! after one known to matter: most of a config comes from the environment and
//! the resources, not from the registry row someone just edited.
//!
//! Restarting is heavier than most changes need — Postgres classifies most GUCs
//! as reloadable and the daemons have no reload path at all — so the plan is to
//! restart only what genuinely requires it and reload the rest (#847). Until
//! then the cost is paid visibly at the moment of the edit rather than left
//! unapplied.
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
    /// Services whose rendered config changed, and which were therefore
    /// restarted onto it. Empty means nothing changed — a reload that renders
    /// identical config is a no-op, not a restart.
    restarted: Vec<String>,
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

/// Everything a request needs. `Arc` because routerify hands it to each request
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

    // PRERENDERED_CONFIG means the config on disk is the caller's, not ours —
    // `yb_amp_local` writes it from an environment this process cannot see, so
    // re-rendering would replace a working config with one built from defaults.
    if state.env.prerendered {
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

    let stale = rendered.stale();
    let restarted = tokio::task::spawn_blocking(move || restart_stale(&stale))
        .await
        .map_err(|e| ApiError::InternalServerError(anyhow::anyhow!("the restart task: {e}")))?
        // The settings are stored and rendered by now, so a restart that failed
        // leaves a service running the previous config — worth an error rather
        // than a 200 that implies it took.
        .map_err(ApiError::InternalServerError)?;

    tracing::info!(?restarted, "settings reloaded from the registry");
    json_response(StatusCode::OK, ReloadResponse { restarted })
}

pub fn make_router(state: Arc<State>) -> RouterBuilder<Body, ApiError> {
    endpoint::make_router()
        .data(state)
        .post("/v1/settings/reload", |r| {
            endpoint::request_span(r, reload_handler)
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
    use crate::Rendered;

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

    /// Which services a change touches, in the order they are restarted.
    #[test]
    fn stale_names_the_services_whose_config_moved() {
        let none = Rendered::default();
        assert!(none.stale().is_empty());

        let all = Rendered {
            pageserver: true,
            safekeeper: true,
            compute: true,
        };
        // Restart order, not declaration order: the safekeeper is what the
        // pageserver and the compute read from.
        assert_eq!(all.stale(), vec!["safekeeper", "pageserver", "compute_ctl"]);

        let one = Rendered {
            pageserver: true,
            ..Default::default()
        };
        assert_eq!(one.stale(), vec!["pageserver"]);
    }
}
