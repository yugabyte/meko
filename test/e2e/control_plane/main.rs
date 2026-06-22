//! The amp-on-Docker e2e suite (`e2e_control_plane`) — deliberately thin.
//!
//! Provisioning lives in **amp**, across two REST calls: `POST .../timelines`
//! records the tenant + its main timeline, then `POST .../endpoints` deploys a
//! compute endpoint for it (synchronously, via amp's Docker provisioner) and
//! returns its connection address. So this suite only:
//!   1. calls amp's REST API (user -> storage pool -> timeline -> endpoint), and
//!   2. runs PG against the endpoint address amp hands back.
//!
//! There is no container/Docker orchestration here — that is amp's job.
//!
//! amp itself is started by `test/e2e/control_plane/run-controller.sh`, which builds +
//! runs this binary with amp's base URL in `AMP_BASE_URL`.

use std::future::Future;
use std::pin::Pin;
use std::process::ExitCode;
use std::time::{Duration, Instant};

use anyhow::{Context, Result, bail};
use serde_json::{Value, json};

use e2e_common::db;

/// Print a step banner. (The storage suite's `step` also tees to a run
/// log; this suite just prints to stdout.)
fn step(msg: &str) {
    println!();
    println!("=== {msg} ===");
}

/// `e2e_control_plane [-n <count>]` — run the amp suite once, or `<count>`
/// times with `-n` (run-all + tally, which also disables fail-fast).
#[tokio::main]
async fn main() -> ExitCode {
    let mut repeat: u32 = 1;
    let mut fail_fast = true;
    let mut it = std::env::args().skip(1);
    while let Some(arg) = it.next() {
        let count = if arg == "-n" {
            it.next()
        } else {
            arg.strip_prefix("-n=").or_else(|| arg.strip_prefix("-n")).map(str::to_string)
        };
        match count {
            Some(c) => match c.parse::<u32>().ok().filter(|&n| n >= 1) {
                Some(n) => {
                    repeat = n;
                    fail_fast = false; // -n opts into run-all + tally
                }
                None => {
                    eprintln!("invalid -n count: {c:?}");
                    return ExitCode::from(2);
                }
            },
            None => {
                eprintln!("unexpected argument: {arg:?}\nusage: e2e_control_plane [-n <count>]");
                return ExitCode::from(2);
            }
        }
    }

    match run_suite(repeat, fail_fast).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("\nAMP E2E FAILED: {e:#}");
            ExitCode::FAILURE
        }
    }
}

// --- amp REST client --------------------------------------------------------

struct AmpClient {
    root: String,
    base: String,
    http: reqwest::Client,
}

/// What amp returns from create-timeline: the storage-layer tenant id (hex) and
/// the timeline id (used to address the endpoint sub-resource).
struct TimelineResult {
    tenant_hex: String,
    timeline_id: String,
}

impl AmpClient {
    fn new(base_url: &str) -> Self {
        let root = base_url.trim_end_matches('/').to_string();
        Self {
            base: format!("{root}/api/v1"),
            root,
            // No client-wide timeout: the create-timeline call blocks while amp
            // provisions the endpoint (tens of seconds).
            http: reqwest::Client::new(),
        }
    }

    async fn wait_ready(&self, timeout: Duration) -> Result<()> {
        let url = format!("{}/readyz", self.root);
        let deadline = Instant::now() + timeout;
        let mut last = String::from("(no response yet)");
        while Instant::now() < deadline {
            match self.http.get(&url).timeout(Duration::from_secs(3)).send().await {
                Ok(r) if r.status().is_success() => return Ok(()),
                Ok(r) => last = format!("status {}", r.status()),
                Err(e) => last = e.to_string(),
            }
            tokio::time::sleep(Duration::from_millis(500)).await;
        }
        bail!("amp {url} not ready within {timeout:?}: {last}");
    }

    async fn post(&self, path: &str, body: Value, want: u16) -> Result<Value> {
        let url = format!("{}{path}", self.base);
        let resp = self.http.post(&url).json(&body).send().await.with_context(|| format!("POST {url}"))?;
        let status = resp.status().as_u16();
        let text = resp.text().await.unwrap_or_default();
        if status != want {
            bail!("POST {url} -> {status} (want {want}): {text}");
        }
        serde_json::from_str(&text).with_context(|| format!("decoding {url} response: {text}"))
    }

    async fn create_user(&self, is_admin: bool) -> Result<String> {
        let v = self.post("/users", json!({ "is_admin": is_admin }), 201).await?;
        field(&v, "id")
    }

    async fn create_storage_pool(&self, user: &str, name: &str) -> Result<String> {
        // Storage pools are top-level; the owner is named in the body.
        let v = self
            .post("/storage-pools", json!({ "name": name, "owner_user_id": user }), 201)
            .await?;
        field(&v, "id")
    }

    /// Create a tenant + its main timeline. Returns the storage-layer tenant id
    /// (hex) and the timeline id. Compute is a separate resource (create_endpoint).
    async fn create_timeline(&self, pool: &str, name: &str, pg_version: u32) -> Result<TimelineResult> {
        let v = self
            .post(
                &format!("/storage-pools/{pool}/timelines"),
                json!({ "name": name, "pg_version": pg_version }),
                201,
            )
            .await?;
        let tenant_hex = field(&v, "tenant_id")?.replace('-', "");
        let timeline_id = field(&v, "timeline_id")?;
        Ok(TimelineResult { tenant_hex, timeline_id })
    }

    /// Deploy a read_write compute endpoint on the timeline and return its
    /// connection address. amp deploys synchronously, so the `201` response
    /// already carries the address. Endpoints are top-level under the timeline.
    async fn create_endpoint(&self, pool: &str, timeline: &str) -> Result<String> {
        let v = self
            .post(
                &format!("/storage-pools/{pool}/timelines/{timeline}/endpoints"),
                json!({ "role": "read_write" }),
                201,
            )
            .await?;
        field(&v, "address")
    }
}

fn field(v: &Value, key: &str) -> Result<String> {
    v.get(key)
        .and_then(Value::as_str)
        .map(str::to_string)
        .with_context(|| format!("response missing string field {key:?}: {v}"))
}

// --- the endpoint under test ------------------------------------------------

/// The compute endpoint amp provisioned: how to connect, and which tenant it
/// should be serving.
struct Endpoint {
    compute_conn: String,
    tenant_hex: String,
}

impl Endpoint {
    /// Connection string for the address amp returned (`host:port`), with the
    /// endpoint's md5 password (set by the endpoint supervisor).
    fn from_address(addr: &str, tenant_hex: String) -> Result<Self> {
        let (host, port) = addr
            .rsplit_once(':')
            .with_context(|| format!("endpoint address {addr:?} is not host:port"))?;
        let conn = format!(
            "host={host} port={port} user=cloud_admin password=cloud_admin dbname=postgres"
        );
        Ok(Self { compute_conn: conn, tenant_hex })
    }

    async fn wait_ready(&self, timeout: Duration) -> Result<()> {
        let deadline = Instant::now() + timeout;
        let mut last = String::from("(no attempt yet)");
        while Instant::now() < deadline {
            match db::query_i64(&self.compute_conn, "SELECT 1::bigint").await {
                Ok(1) => return Ok(()),
                Ok(other) => last = format!("readiness probe returned {other}"),
                Err(e) => last = format!("{e:#}"),
            }
            tokio::time::sleep(Duration::from_secs(1)).await;
        }
        bail!("compute not ready within {timeout:?}: {last}");
    }
}

// --- scenarios --------------------------------------------------------------

type ScenarioFut<'a> = Pin<Box<dyn Future<Output = Result<()>> + 'a>>;

fn scenarios(e: &Endpoint) -> Vec<(&'static str, ScenarioFut<'_>)> {
    vec![
        ("smoke", Box::pin(smoke(e))),
        ("resource_ladder", Box::pin(resource_ladder(e))),
    ]
}

/// The endpoint is a working Postgres: DDL + DML, read back.
async fn smoke(e: &Endpoint) -> Result<()> {
    let conn = &e.compute_conn;
    let version = db::query_text(conn, "SELECT version()").await?;
    if !version.contains("PostgreSQL") {
        bail!("unexpected version banner: {version}");
    }
    db::batch(
        conn,
        "CREATE TABLE amp_smoke (id int PRIMARY KEY, note text); \
         INSERT INTO amp_smoke VALUES (1, 'hello'), (2, 'world');",
    )
    .await?;
    let n = db::query_i64(conn, "SELECT count(*) FROM amp_smoke").await?;
    if n != 2 {
        bail!("amp_smoke row count = {n}, want 2");
    }
    let note = db::query_text(conn, "SELECT note FROM amp_smoke WHERE id = 1").await?;
    if note != "hello" {
        bail!("amp_smoke id=1 note = {note:?}, want \"hello\"");
    }
    Ok(())
}

/// The compute serves exactly the tenant amp created, and writes are YB-backed
/// (durable across a CHECKPOINT).
async fn resource_ladder(e: &Endpoint) -> Result<()> {
    let conn = &e.compute_conn;
    let served = db::query_text(conn, "SELECT current_setting('yb_amp.tenant_id')").await?;
    if served != e.tenant_hex {
        bail!("compute serves tenant {served}, but amp created {}", e.tenant_hex);
    }
    db::batch(
        conn,
        "CREATE TABLE amp_ladder (k int); INSERT INTO amp_ladder SELECT generate_series(1, 100); CHECKPOINT;",
    )
    .await?;
    let sum = db::query_i64(conn, "SELECT sum(k) FROM amp_ladder").await?;
    if sum != 5050 {
        bail!("amp_ladder sum = {sum}, want 5050");
    }
    Ok(())
}

// --- run loop ---------------------------------------------------------------

/// Drive amp's API to provision an endpoint, then run the scenarios against it.
/// amp must already be running; its base URL comes from `AMP_BASE_URL`.
pub async fn run_suite(repeat: u32, fail_fast: bool) -> Result<()> {
    let base = std::env::var("AMP_BASE_URL")
        .context("AMP_BASE_URL is not set — run via test/e2e/control_plane/run-controller.sh, which starts amp")?;
    step(&format!("amp e2e: driving amp at {base}"));
    let client = AmpClient::new(&base);
    client.wait_ready(Duration::from_secs(60)).await.context("amp readiness")?;

    step("amp e2e: create user -> storage pool -> timeline -> endpoint (amp deploys the compute)");
    let user = client.create_user(true).await?;
    let pool = client.create_storage_pool(&user, "e2e-pool").await?;
    let tl = client.create_timeline(&pool, "main", 17).await.context("create timeline")?;
    let addr = client
        .create_endpoint(&pool, &tl.timeline_id)
        .await
        .context("create endpoint — start amp with --deploy (AMP_DEPLOY=true)")?;
    step(&format!("amp e2e: amp deployed endpoint at {addr} (tenant {})", tl.tenant_hex));

    let endpoint = Endpoint::from_address(&addr, tl.tenant_hex)?;
    endpoint.wait_ready(Duration::from_secs(60)).await.context("endpoint readiness")?;

    let mut passed = 0u64;
    let mut failed = 0u64;
    let mut fatal: Option<anyhow::Error> = None;
    'outer: for iter in 1..=repeat {
        if repeat > 1 {
            step(&format!("amp iteration {iter}/{repeat}"));
        }
        for (name, fut) in scenarios(&endpoint) {
            match fut.await {
                Ok(()) => {
                    passed += 1;
                    println!("PASS — amp/{name}");
                }
                Err(e) => {
                    failed += 1;
                    eprintln!("FAIL — amp/{name}: {e:#}");
                    if fail_fast {
                        fatal = Some(e);
                        break 'outer;
                    }
                }
            }
        }
    }

    step(&format!("amp e2e: {passed} passed, {failed} failed"));
    if let Some(e) = fatal {
        return Err(e);
    }
    if failed > 0 {
        bail!("{failed} amp test(s) failed");
    }
    Ok(())
}
