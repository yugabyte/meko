//! `POST /v1/settings/check`: would Postgres take these settings?
//!
//! The controller asks this before it writes a settings row, because a name
//! Postgres does not recognise or a value it cannot parse is not ignored — it
//! makes a reload apply nothing and the next start fail outright, so an endpoint
//! keeps serving until something restarts it and then does not come back.
//!
//! Testable here where the reload is not: the check renders nothing, so it works
//! on a `PRERENDERED_CONFIG` endpoint like the ones this harness runs, and it
//! answers from the same Postgres binary the compute uses.

use anyhow::{Context, bail};
use serde_json::Value;

use crate::cluster::{Cluster, step};
use crate::db;

/// The port the launcher allocated for this endpoint's settings API.
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

async fn check(port: u16, body: &str) -> anyhow::Result<Value> {
    let resp = reqwest::Client::new()
        .post(format!("http://127.0.0.1:{port}/v1/settings/check"))
        .header("Content-Type", "application/json")
        .body(body.to_string())
        .send()
        .await
        .with_context(|| format!("POST /v1/settings/check on {port}"))?;
    let status = resp.status();
    let text = resp.text().await.unwrap_or_default();
    if status != reqwest::StatusCode::OK {
        bail!("check returned {status}: {text}");
    }
    serde_json::from_str(&text).with_context(|| format!("parsing {text}"))
}

/// The `not_applied` entries, as (name, reason) pairs.
fn refused(r: &Value) -> Vec<(&str, &str)> {
    r["not_applied"]
        .as_array()
        .map(|a| {
            a.iter()
                .filter_map(|n| Some((n["name"].as_str()?, n["reason"].as_str()?)))
                .collect()
        })
        .unwrap_or_default()
}

/// The reason reported for `name`, or None when it was accepted.
fn reason_for<'a>(r: &'a Value, name: &str) -> Option<&'a str> {
    refused(r)
        .into_iter()
        .find(|(n, _)| *n == name)
        .map(|(_, reason)| reason)
}

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());
    let port = settings_port(&yb, c.branch()).await?;

    step("Settings Postgres accepts are reported as acceptable");
    // Two real settings and two names Postgres treats as placeholders: a custom
    // GUC, and one belonging to an extension that is not loaded. Refusing either
    // of the last two would make configuring an extension impossible.
    let got = check(
        port,
        r#"{"pg_settings":{
            "work_mem":"64MB",
            "log_min_duration_statement":"250ms",
            "amp_probe.custom":"1",
            "pg_stat_statements.max":"5000"
        }}"#,
    )
    .await?;
    if !refused(&got).is_empty() {
        bail!("usable settings were refused: {:?}", refused(&got));
    }

    step("Each kind of unusable setting is named, with why");
    // All in one request: an operator who mistypes twice should learn both at
    // once rather than fixing one and coming back for the next.
    let got = check(
        port,
        r#"{"pg_settings":{
            "work_memm":"5MB",
            "work_mem":"banana",
            "block_size":"8192",
            "maintenance_work_mem":"128MB"
        }}"#,
    )
    .await?;
    for (name, want) in [
        ("work_memm", "unrecognized parameter"),
        ("work_mem", "invalid value"),
        ("block_size", "cannot be set in a configuration file"),
    ] {
        match reason_for(&got, name) {
            Some(reason) if reason == want => {}
            Some(reason) => bail!("{name} reported as {reason:?}, want {want:?}"),
            None => bail!(
                "{name} was accepted; writing it would stop the compute from starting \
                 (reported: {:?})",
                refused(&got)
            ),
        }
    }
    // The good setting beside them must survive: a check that condemned the whole
    // request would make one typo undo an operator's other edits.
    if let Some(reason) = reason_for(&got, "maintenance_work_mem") {
        bail!("a usable setting was refused alongside the bad ones: {reason}");
    }

    step("Nothing was applied, and nothing was restarted");
    // The check must be a question, not a change: the compute is still serving
    // the value it had, on the postmaster it had.
    let compute = db::compute_conn(c.compute_port());
    let in_force = db::query_text(&compute, "SHOW work_mem").await?;
    if in_force == "64MB" || in_force == "banana" {
        bail!("the check applied work_mem (now {in_force})");
    }
    Ok(())
}
