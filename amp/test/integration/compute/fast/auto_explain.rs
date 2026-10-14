//! auto_explain, which every compute preloads and leaves off. An operator turns
//! it on with the endpoint's `pg_settings`, which the supervisor applies with a
//! reload. Because the library is already loaded, the change reaches a session
//! that was open before it, and the reply reports every setting as applied.
//! `auto_explain.log_level = notice` sends each plan to the client, so this
//! asserts the plan itself.

use std::time::{Duration, Instant};

use anyhow::{Context, bail};
use serde_json::Value;
use tokio::sync::mpsc::UnboundedReceiver;
use tokio_postgres::Client;
use tokio_postgres::error::DbError;

use crate::cluster::{Cluster, step};
use crate::db;

const PROBE: &str = "SELECT 1 AS auto_explain_probe";

/// Plans go to the client, but `log_min_duration` keeps its default of -1.
const NOTICE_ONLY: &str = r#"{"auto_explain.log_level": "notice"}"#;

const ON: &str = r#"{"auto_explain.log_level": "notice", "auto_explain.log_min_duration": "0"}"#;

/// How long a reload may take to reach Postgres: compute_ctl applies it on its
/// configurator loop, after the supervisor has replied.
const APPLY_TIMEOUT: Duration = Duration::from_secs(60);

async fn store_settings(yb: &str, branch: &str, settings: &str) -> anyhow::Result<()> {
    db::batch(
        yb,
        &format!(
            "UPDATE amp_registry.endpoints SET pg_settings = '{settings}'::jsonb \
             WHERE branch_id = '{branch}' AND role = 'read_write'"
        ),
    )
    .await
}

/// Reloads the endpoint's settings, and fails unless they all applied without a
/// restart.
async fn reload(port: u16) -> anyhow::Result<()> {
    let resp = reqwest::Client::new()
        .post(format!("http://127.0.0.1:{port}/v1/settings/reload"))
        .send()
        .await
        .with_context(|| format!("POST /v1/settings/reload on {port}"))?;
    let status = resp.status();
    let text = resp.text().await.unwrap_or_default();
    if status != reqwest::StatusCode::OK {
        bail!("reload returned {status}, want 200 OK: {text}");
    }
    let reply: Value = serde_json::from_str(&text).with_context(|| format!("parsing {text}"))?;
    let list = |key: &str| {
        reply[key]
            .as_array()
            .with_context(|| format!("no {key} list in the reload's reply: {reply}"))
    };
    // The supervisor names what it restarted: compute_ctl, or a daemon whose
    // toml changed.
    let restarted = list("restarted")?;
    if !restarted.is_empty() {
        bail!("the edit restarted {restarted:?}, but auto_explain's settings change on a reload: {reply}");
    }
    let not_applied = list("not_applied")?;
    if !not_applied.is_empty() {
        bail!(
            "the reply lists {not_applied:?} as not applied, though the preloaded auto_explain defines them: {reply}"
        );
    }
    Ok(())
}

/// Waits until `session` reads `name` as `want`. A backend takes a reload at its
/// next statement.
async fn await_setting(session: &Client, name: &str, want: &str) -> anyhow::Result<()> {
    let deadline = Instant::now() + APPLY_TIMEOUT;
    loop {
        let got: String = session
            .query_one("SELECT setting FROM pg_settings WHERE name = $1", &[&name])
            .await
            .with_context(|| format!("reading {name} from pg_settings"))?
            .get(0);
        if got == want {
            return Ok(());
        }
        if Instant::now() > deadline {
            bail!("{name} is still {got:?} in the open session {APPLY_TIMEOUT:?} after the reload, want {want:?}");
        }
        tokio::time::sleep(Duration::from_millis(250)).await;
    }
}

/// Runs the probe on `session` and returns whether it sent the probe's plan.
async fn explains_probe(
    session: &Client,
    notices: &mut UnboundedReceiver<DbError>,
) -> anyhow::Result<bool> {
    while notices.try_recv().is_ok() {}
    session
        .simple_query(PROBE)
        .await
        .context("running the probe query")?;
    // The driver queues a notice as soon as it reads it, before the response
    // that follows, so the probe's plan is queued by now if it was sent.
    let mut explained = false;
    while let Ok(n) = notices.try_recv() {
        let m = n.message();
        explained |= m.contains("plan:") && m.contains(&format!("Query Text: {PROBE}"));
    }
    Ok(explained)
}

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let yb = db::yb_conn(c.yb_host(), c.yb_port(), c.db_name());
    let branch = c.branch();
    let port = c.settings_port().await?;

    let result = explain_plans(c, &yb, branch, port).await;

    // Later scenarios' goldens would otherwise get a NOTICE per statement. The
    // compute is recycled before each one and renders the stored row, so
    // clearing the row is enough. A failed check's error wins over this one.
    let cleared = store_settings(&yb, branch, "{}").await;
    result.and(cleared)
}

async fn explain_plans(c: &Cluster, yb: &str, branch: &str, port: u16) -> anyhow::Result<()> {
    let compute = db::compute_conn(c.compute_port());
    let started_at = db::query_text(&compute, "SELECT pg_postmaster_start_time()::text").await?;

    step("auto_explain is preloaded and off");
    let (session, mut notices) = db::connect_with_notices(&compute).await?;
    // pg_settings lists a library's settings only once it is loaded.
    let min = session
        .query_opt(
            "SELECT setting FROM pg_settings WHERE name = 'auto_explain.log_min_duration'",
            &[],
        )
        .await
        .context("reading auto_explain.log_min_duration")?;
    match min.map(|r| r.get::<_, String>(0)) {
        None => bail!("pg_settings has no auto_explain.log_min_duration: auto_explain is not loaded"),
        Some(v) if v != "-1" => bail!("auto_explain.log_min_duration is {v}, want its default -1"),
        Some(_) => {}
    }

    step("With log_level = notice alone, the open session gets no plan");
    store_settings(yb, branch, NOTICE_ONLY).await?;
    reload(port).await?;
    await_setting(&session, "auto_explain.log_level", "notice").await?;
    if explains_probe(&session, &mut notices).await? {
        bail!("auto_explain sent a plan for {PROBE:?} with log_min_duration at its default -1");
    }

    step("Adding log_min_duration = 0 sends the open session its plan");
    store_settings(yb, branch, ON).await?;
    reload(port).await?;
    await_setting(&session, "auto_explain.log_min_duration", "0").await?;
    if !explains_probe(&session, &mut notices).await? {
        bail!("no auto_explain plan for {PROBE:?} in a session opened before the reload");
    }

    let still_started_at =
        db::query_text(&compute, "SELECT pg_postmaster_start_time()::text").await?;
    if still_started_at != started_at {
        bail!("turning auto_explain on restarted Postgres ({started_at} -> {still_started_at})");
    }
    Ok(())
}
