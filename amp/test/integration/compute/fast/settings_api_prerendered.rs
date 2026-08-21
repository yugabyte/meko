//! `POST /v1/settings/reload` against a launcher-rendered endpoint.
//!
//! The supervisor renders config in a pod and does not here: local config comes
//! from an environment it cannot see — ports from a free-port search, a Postgres
//! install outside any image, per-endpoint settings held in the control plane —
//! so `yb_amp_local` renders and sets `PRERENDERED_CONFIG`. The handler refuses
//! on that flag.
//!
//! Which makes the refusal the contract worth pinning, because losing it is not
//! a no-op: the handler would re-render from resource-derived defaults, write
//! that over three working config files, and restart the endpoint onto ports and
//! paths that are not the ones the launcher allocated. This asserts the refusal
//! and that it costs nothing — the settings stay unapplied and Postgres is not
//! bounced.
//!
//! The end-to-end apply (reload for a `sighup` setting, restart for a
//! `postmaster` one) is only reachable where the supervisor renders, so it
//! belongs to a deployed cluster rather than this tier.

use anyhow::{Context, bail};

use crate::cluster::{Cluster, step};
use crate::db;

/// A setting no local endpoint would otherwise be running, so seeing it in force
/// means the refusal did not hold.
const PROBE: &str = "123MB";

/// The port the launcher allocated for this endpoint's settings API, out of the
/// opaque `extended` config it stored on the row.
async fn settings_port(yb: &str, branch: &str) -> anyhow::Result<u16> {
    let raw = db::query_text_opt(
        yb,
        &format!(
            "SELECT config #>> '{{extended,settings_http_port}}' \
             FROM amp_registry.endpoints \
             WHERE branch_id = '{branch}' AND role = 'read_write'"
        ),
    )
    .await?
    .context("no read_write endpoint for the branch, or no settings port on it")?;
    raw.parse()
        .with_context(|| format!("settings_http_port {raw:?} is not a port"))
}

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());
    let compute = db::compute_conn(c.compute_port());
    let branch = c.branch();

    let port = settings_port(&yb, branch).await?;
    let started_at = db::query_text(&compute, "SELECT pg_postmaster_start_time()::text").await?;

    step("Store a setting the running compute is not using");
    db::batch(
        &yb,
        &format!(
            "UPDATE amp_registry.endpoints SET pg_settings = '{{\"work_mem\": \"{PROBE}\"}}'::jsonb \
             WHERE branch_id = '{branch}' AND role = 'read_write'"
        ),
    )
    .await?;

    step("The settings API refuses to re-render a prerendered endpoint");
    let resp = reqwest::Client::new()
        .post(format!("http://127.0.0.1:{port}/v1/settings/reload"))
        .send()
        .await
        .with_context(|| format!("POST /v1/settings/reload on {port}"))?;
    let status = resp.status();
    let body = resp.text().await.unwrap_or_default();
    if status != reqwest::StatusCode::PRECONDITION_FAILED {
        bail!("reload returned {status}, want 412 Precondition Failed: {body}");
    }
    // The reason has to be in the reply: a controller that gets a bare 412 has
    // no way to tell "this endpoint renders its own config" from a transient
    // failure it should retry.
    if !body.contains("prerendered") {
        bail!("412 body does not say why: {body}");
    }

    step("The refusal applied nothing and bounced nothing");
    let in_force = db::query_text(&compute, "SHOW work_mem").await?;
    if in_force == PROBE {
        bail!("the refused reload applied work_mem anyway (now {in_force})");
    }
    let still_started_at =
        db::query_text(&compute, "SELECT pg_postmaster_start_time()::text").await?;
    if still_started_at != started_at {
        bail!("the refused reload restarted Postgres ({started_at} -> {still_started_at})");
    }

    // The launcher renders this row's pg_settings on the next `endpoint start`,
    // so leaving the probe behind would hand it to a later scenario.
    db::batch(
        &yb,
        &format!(
            "UPDATE amp_registry.endpoints SET pg_settings = '{{}}'::jsonb \
             WHERE branch_id = '{branch}' AND role = 'read_write'"
        ),
    )
    .await?;
    Ok(())
}
