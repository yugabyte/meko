//! `POST /v1/settings/reload` against a locally-launched endpoint.
//!
//! `yb_amp_local` runs with `PRERENDERED_CONFIG=daemons`: it brings the two
//! daemon tomls, whose content comes from an environment the supervisor cannot
//! see — per-endpoint data directories and ports from a free-port search — and
//! the supervisor renders the compute spec, as it does on every other deploy
//! path. So a reload here re-renders that spec and applies it, which it could not
//! do while the launcher rendered its own copy.
//!
//! Two halves of the contract, and the second is the one that used to need the
//! refusal to hold: the settings change actually reaches Postgres, **and** no
//! daemon is restarted. The supervisor restarts a daemon only when its toml
//! changed, so an empty `restarted` list is the assertion that it left the
//! launcher's two files alone — re-rendering those from container defaults would
//! point the endpoint at ports and paths the launcher never allocated.

use anyhow::{Context, bail};

use crate::cluster::{Cluster, step};
use crate::db;

/// A setting no local endpoint would otherwise be running, so seeing it in force
/// means the reload really applied it. `work_mem` is `user`-context, so it
/// applies on a reload and must not cost a restart.
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

    step("The settings API re-renders the compute spec and applies it");
    let resp = reqwest::Client::new()
        .post(format!("http://127.0.0.1:{port}/v1/settings/reload"))
        .send()
        .await
        .with_context(|| format!("POST /v1/settings/reload on {port}"))?;
    let status = resp.status();
    let body = resp.text().await.unwrap_or_default();
    if status != reqwest::StatusCode::OK {
        bail!("reload returned {status}, want 200 OK: {body}");
    }

    step("The daemons the launcher configured were left alone");
    // Their tomls are the launcher's, and the supervisor restarts a daemon only
    // when its toml changed — so anything named here means it re-rendered one
    // from defaults, over ports and paths it did not allocate.
    let restarted = body
        .split("\"restarted\":")
        .nth(1)
        .and_then(|t| t.split(']').next())
        .unwrap_or_default();
    if restarted.contains("pageserver") || restarted.contains("safekeeper") {
        bail!("reload restarted a daemon whose config the launcher owns: {body}");
    }

    step("The stored setting is in force, without a restart");
    // Bounded, because the apply is asynchronous on purpose: the supervisor
    // signals `compute_ctl`, which logs "reconfigure queued" and applies it on
    // its configurator loop, so the 200 above can land before Postgres has
    // re-read the file. The wait is what makes this assert the apply rather than
    // the timing, and it still fails rather than passing on a value that never
    // arrives.
    let mut in_force = String::new();
    for _ in 0..40 {
        in_force = db::query_text(&compute, "SHOW work_mem").await?;
        if in_force == PROBE {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(250)).await;
    }
    if in_force != PROBE {
        bail!("work_mem is {in_force} after 10s, want {PROBE} — the reload never applied the stored setting");
    }
    // `work_mem` is not `postmaster`, so applying it must not have bounced
    // Postgres: that is the difference between a reload and a restart.
    let still_started_at =
        db::query_text(&compute, "SELECT pg_postmaster_start_time()::text").await?;
    if still_started_at != started_at {
        bail!("applying a reloadable setting restarted Postgres ({started_at} -> {still_started_at})");
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
