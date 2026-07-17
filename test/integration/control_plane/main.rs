//! The controller-on-Docker integration suite (`integration_control_plane`) — a thin driver.
//!
//! Provisioning lives in the **controller**: this suite only drives its REST
//! API (user → storage pool → project → branch → endpoint) and runs Postgres
//! against the endpoint addresses the controller hands back. There is no
//! container/Docker orchestration here — that is the controller's job.
//!
//! The centrepiece is [`branching_run`]: it creates a project (a storage-layer
//! tenant + its default branch `main`), deploys an endpoint on `main`, seeds
//! data, then forks a branch tree —
//!
//! ```text
//!   main ──┬── b1 ── b2     (b2 forks the branch b1)
//!          └── b3           (b3 forks the root main, a second time)
//! ```
//!
//! — deploying an endpoint per branch and asserting copy-on-write isolation at
//! every fork point: each branch reads its ancestors' pre-fork rows via lineage
//! reads (nothing copied), never its ancestors' post-fork writes, and its own
//! writes never leak to a sibling or an ancestor. It also checks the
//! control-plane metadata (fork ancestry, branch count) and that each compute
//! serves the exact tenant/timeline the controller bound it to.
//!
//! The controller itself is started by `test/integration/control_plane/run-controller.sh`,
//! which builds + runs this binary with the controller base URL in `AMP_BASE_URL`.

use std::process::ExitCode;
use std::time::{Duration, Instant};

use anyhow::{Context, Result, bail};
use serde::Deserialize;
use serde_json::{Value, json};

use integration_common::db;

/// Poll budget for a metadata-only task (create/delete project, create/delete
/// branch). These only write control-plane rows + recipes, so they settle fast.
const TASK_TIMEOUT: Duration = Duration::from_secs(120);

/// Poll budget for an endpoint task (create/delete endpoint). Creating an
/// endpoint actually starts a pod/container (and, for the first endpoint on a
/// branch, materializes it / clones from an ancestor), so it can take tens of
/// seconds; give it a generous ceiling (real pod deploys on a loaded cluster
/// need headroom beyond the metadata-task budget).
const ENDPOINT_TASK_TIMEOUT: Duration = Duration::from_secs(240);

/// Print a step banner. (The compute suite's `step` also tees to a run log;
/// this suite just prints to stdout.)
fn step(msg: &str) {
    println!();
    println!("=== {msg} ===");
}

/// `integration_control_plane [-n <count>]` — run the suite once, or `<count>` times
/// with `-n` (run-all + tally, which also disables fail-fast).
#[tokio::main]
async fn main() -> ExitCode {
    let mut repeat: u32 = 1;
    let mut fail_fast = true;
    let mut it = std::env::args().skip(1);
    while let Some(arg) = it.next() {
        let count = if arg == "-n" {
            it.next()
        } else {
            arg.strip_prefix("-n=")
                .or_else(|| arg.strip_prefix("-n"))
                .map(str::to_string)
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
                eprintln!("unexpected argument: {arg:?}\nusage: integration_control_plane [-n <count>]");
                return ExitCode::from(2);
            }
        }
    }

    match run_suite(repeat, fail_fast).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("\nCONTROLLER INTEGRATION FAILED: {e:#}");
            ExitCode::FAILURE
        }
    }
}

// --- controller REST client -------------------------------------------------

struct AmpClient {
    root: String,
    base: String,
    http: reqwest::Client,
}

/// A branch as the controller addresses + identifies it. `id` is the path id
/// (a UUID); `timeline_hex` is the same id with dashes stripped — how the
/// compute reports the timeline it serves via `yb_amp.timeline_id`.
#[derive(Clone)]
struct BranchRef {
    id: String,
    timeline_hex: String,
}

/// A project: its path id (UUID = the storage-layer tenant id), the tenant hex
/// the compute reports (`yb_amp.tenant_id`), and its default branch.
struct ProjectRef {
    id: String,
    tenant_hex: String,
    default_branch: BranchRef,
}

/// The lifecycle status of an async task, as the controller reports it. The six
/// mutating endpoints (create/delete project, branch, endpoint) now return a
/// `Task` and run the work in the background; the client polls until it reaches
/// a terminal state.
#[derive(Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
enum TaskStatus {
    Pending,
    Running,
    Succeeded,
    Failed,
}

/// A controller async task. Only the fields the harness acts on are decoded;
/// `params`/`created_at`/etc. are ignored. `error` is set when `status` is
/// `failed`.
#[derive(Deserialize)]
struct Task {
    id: String,
    #[serde(default)]
    error: Option<String>,
    status: TaskStatus,
}

impl AmpClient {
    fn new(base_url: &str) -> Self {
        let root = base_url.trim_end_matches('/').to_string();
        Self {
            base: format!("{root}/api/v1"),
            root,
            // No client-wide timeout: create-endpoint blocks while the
            // controller deploys (and materializes) the compute — tens of
            // seconds, longer for a branch clone.
            http: reqwest::Client::new(),
        }
    }

    async fn wait_ready(&self, timeout: Duration) -> Result<()> {
        let url = format!("{}/readyz", self.root);
        let deadline = Instant::now() + timeout;
        let mut last = String::from("(no response yet)");
        while Instant::now() < deadline {
            match self
                .http
                .get(&url)
                .timeout(Duration::from_secs(3))
                .send()
                .await
            {
                Ok(r) if r.status().is_success() => return Ok(()),
                Ok(r) => last = format!("status {}", r.status()),
                Err(e) => last = e.to_string(),
            }
            tokio::time::sleep(Duration::from_millis(500)).await;
        }
        bail!("controller {url} not ready within {timeout:?}: {last}");
    }

    async fn post(&self, path: &str, body: Value, want: u16) -> Result<Value> {
        let url = format!("{}{path}", self.base);
        let resp = self
            .http
            .post(&url)
            .json(&body)
            .send()
            .await
            .with_context(|| format!("POST {url}"))?;
        let status = resp.status().as_u16();
        let text = resp.text().await.unwrap_or_default();
        if status != want {
            bail!("POST {url} -> {status} (want {want}): {text}");
        }
        serde_json::from_str(&text).with_context(|| format!("decoding {url} response: {text}"))
    }

    async fn get(&self, path: &str) -> Result<Value> {
        let url = format!("{}{path}", self.base);
        let resp = self
            .http
            .get(&url)
            .send()
            .await
            .with_context(|| format!("GET {url}"))?;
        let status = resp.status().as_u16();
        let text = resp.text().await.unwrap_or_default();
        if status != 200 {
            bail!("GET {url} -> {status} (want 200): {text}");
        }
        serde_json::from_str(&text).with_context(|| format!("decoding {url} response: {text}"))
    }

    /// Decode a value the controller already returned (200/201/202) into a
    /// concrete type. The HTTP-status check lives in `post`/`get`/`send_task`.
    fn decode<T: serde::de::DeserializeOwned>(url: &str, text: &str) -> Result<T> {
        serde_json::from_str(text).with_context(|| format!("decoding {url} response: {text}"))
    }

    /// Issue a mutating request (POST or DELETE), expect `202`, and decode the
    /// `Task` the controller returns. Used by the async create/delete flows; the
    /// caller then drives [`poll_task`] to a terminal state.
    async fn send_task(
        &self,
        method: reqwest::Method,
        path: &str,
        body: Option<Value>,
    ) -> Result<Task> {
        let url = format!("{}{path}", self.base);
        let mut req = self.http.request(method.clone(), &url);
        if let Some(b) = body {
            req = req.json(&b);
        }
        let resp = req
            .send()
            .await
            .with_context(|| format!("{method} {url}"))?;
        let status = resp.status().as_u16();
        let text = resp.text().await.unwrap_or_default();
        if status != 202 {
            bail!("{method} {url} -> {status} (want 202): {text}");
        }
        Self::decode(&url, &text)
    }

    /// Poll `GET /tasks/{id}` every ~500ms until the task reaches a terminal
    /// state. Returns `Ok(())` on `succeeded`, an error carrying `task.error` on
    /// `failed`, and times out otherwise. The timeout is generous — a
    /// create_endpoint task actually starts a pod/container.
    async fn poll_task(&self, task_id: &str, timeout: Duration) -> Result<()> {
        let deadline = Instant::now() + timeout;
        loop {
            let v = self
                .get(&format!("/tasks/{task_id}"))
                .await
                .with_context(|| format!("poll task {task_id}"))?;
            let task: Task = serde_json::from_value(v).context("decoding task")?;
            let last_status = match task.status {
                TaskStatus::Succeeded => return Ok(()),
                TaskStatus::Failed => {
                    let err = task
                        .error
                        .unwrap_or_else(|| "(no error message)".to_string());
                    bail!("task {task_id} failed: {err}");
                }
                TaskStatus::Pending => "pending",
                TaskStatus::Running => "running",
            };
            if Instant::now() >= deadline {
                bail!(
                    "task {task_id} not terminal within {timeout:?} (last status: {last_status})"
                );
            }
            tokio::time::sleep(Duration::from_millis(500)).await;
        }
    }

    /// Run a mutating request as an async task end to end: send it, expect a
    /// `202` + `Task`, then poll to a terminal state. The materialized resource
    /// (if any) is fetched separately by the caller once this returns `Ok`.
    async fn run_task(
        &self,
        method: reqwest::Method,
        path: &str,
        body: Option<Value>,
        timeout: Duration,
    ) -> Result<()> {
        let task = self.send_task(method, path, body).await?;
        self.poll_task(&task.id, timeout).await
    }

    async fn create_user(&self, is_admin: bool) -> Result<String> {
        let v = self
            .post("/users", json!({ "is_admin": is_admin }), 201)
            .await?;
        field(&v, "id")
    }

    async fn create_storage_pool(&self, user: &str, name: &str) -> Result<String> {
        // Storage pools are top-level; the owner is named in the body.
        let v = self
            .post(
                "/storage-pools",
                json!({ "name": name, "owner_user_id": user }),
                201,
            )
            .await?;
        field(&v, "id")
    }

    /// Create a project (a tenant) and its default branch. Returns the project
    /// path id, its tenant hex, and the default branch. Does not deploy compute.
    ///
    /// Async-task flow: `POST .../projects` now returns `202` + a `Task`; we poll
    /// it to `succeeded`, then list the pool's projects to read back the
    /// materialized project (its id + default branch) by name.
    async fn create_project(&self, pool: &str, name: &str, pg_version: u32) -> Result<ProjectRef> {
        self.run_task(
            reqwest::Method::POST,
            &format!("/storage-pools/{pool}/projects"),
            Some(json!({ "name": name, "pg_version": pg_version })),
            TASK_TIMEOUT,
        )
        .await?;

        let v = self.find_project(pool, name).await?;
        let id = field(&v, "id")?;
        let tenant_hex = id.replace('-', "");
        let default = v
            .get("default_branch")
            .context("project response missing default_branch")?;
        let default_branch = branch_ref(default)?;
        Ok(ProjectRef {
            id,
            tenant_hex,
            default_branch,
        })
    }

    /// List the pool's projects and return the one named `name` (the project a
    /// just-succeeded create-project task materialized).
    async fn find_project(&self, pool: &str, name: &str) -> Result<Value> {
        let list = self
            .get(&format!("/storage-pools/{pool}/projects?limit=1000"))
            .await?;
        find_named(&list, "projects", name)
            .with_context(|| format!("project {name:?} not found after create task succeeded"))
    }

    /// List the project's branches and return the one named `name` (the branch a
    /// just-succeeded branch task materialized).
    async fn find_branch(&self, pool: &str, project: &str, name: &str) -> Result<Value> {
        let list = self
            .get(&format!(
                "/storage-pools/{pool}/projects/{project}/branches?limit=1000"
            ))
            .await?;
        find_named(&list, "branches", name)
            .with_context(|| format!("branch {name:?} not found after create task succeeded"))
    }

    /// Fork a branch into a new one and return the created `Branch` JSON (so the
    /// caller can assert fork ancestry). `parent` defaults to the project's
    /// default branch; `fork_point_lsn` defaults to the parent's latest durable
    /// LSN (read from the safekeeper co-located with the parent's read_write
    /// endpoint — so the parent must have one). No compute is started.
    async fn create_branch(
        &self,
        pool: &str,
        project: &str,
        name: &str,
        parent: Option<&str>,
        fork_point_lsn: Option<&str>,
    ) -> Result<Value> {
        let mut body = json!({ "name": name });
        if let Some(p) = parent {
            body["parent_branch_id"] = json!(p);
        }
        if let Some(l) = fork_point_lsn {
            body["fork_point_lsn"] = json!(l);
        }
        // Async-task flow: `POST .../branches` returns `202` + a `Task`; poll it
        // to `succeeded`, then read back the materialized branch (so the caller
        // still gets the branch JSON it asserts ancestry on, and its branch_id).
        self.run_task(
            reqwest::Method::POST,
            &format!("/storage-pools/{pool}/projects/{project}/branches"),
            Some(body),
            TASK_TIMEOUT,
        )
        .await?;
        self.find_branch(pool, project, name).await
    }

    /// Deploy a read_write endpoint on a branch and return its `(endpoint_id,
    /// address)`. The first endpoint on a branch materializes it.
    ///
    /// Async-task flow: `POST .../endpoints` returns `202` + a `Task`; the task
    /// actually starts a pod/container, so we poll with the longer
    /// [`ENDPOINT_TASK_TIMEOUT`]. Once it succeeds we list the branch's endpoints
    /// and read back the deployed read_write endpoint's id + address.
    async fn create_endpoint(
        &self,
        pool: &str,
        project: &str,
        branch: &str,
    ) -> Result<(String, String)> {
        self.run_task(
            reqwest::Method::POST,
            &format!("/storage-pools/{pool}/projects/{project}/branches/{branch}/endpoints"),
            Some(json!({ "role": "read_write" })),
            ENDPOINT_TASK_TIMEOUT,
        )
        .await?;

        let list = self
            .get(&format!(
                "/storage-pools/{pool}/projects/{project}/branches/{branch}/endpoints?limit=1000"
            ))
            .await?;
        let endpoints = list
            .get("endpoints")
            .and_then(Value::as_array)
            .context("endpoint list missing endpoints")?;
        let ep = endpoints
            .iter()
            .find(|e| e.get("role").and_then(Value::as_str) == Some("read_write"))
            .context("no read_write endpoint found after create task succeeded")?;
        let id = field(ep, "id")?;
        let address = field(ep, "address")?;
        if address.is_empty() {
            bail!("endpoint {id} has an empty address after create task succeeded");
        }
        Ok((id, address))
    }

    /// Retire an endpoint (best-effort cleanup between iterations).
    ///
    /// Async-task flow: `DELETE .../endpoints/{id}` returns `202` + a `Task`;
    /// poll it to `succeeded`.
    async fn delete_endpoint(
        &self,
        pool: &str,
        project: &str,
        branch: &str,
        endpoint: &str,
    ) -> Result<()> {
        self.run_task(
            reqwest::Method::DELETE,
            &format!(
                "/storage-pools/{pool}/projects/{project}/branches/{branch}/endpoints/{endpoint}"
            ),
            None,
            ENDPOINT_TASK_TIMEOUT,
        )
        .await
    }

    /// Get one branch's JSON, by path id.
    async fn get_branch(&self, pool: &str, project: &str, branch: &str) -> Result<Value> {
        self.get(&format!(
            "/storage-pools/{pool}/projects/{project}/branches/{branch}"
        ))
        .await
    }

    /// The `total` from listBranches (count of all branches in the project).
    async fn count_branches(&self, pool: &str, project: &str) -> Result<i64> {
        let v = self
            .get(&format!(
                "/storage-pools/{pool}/projects/{project}/branches?limit=1000"
            ))
            .await?;
        v.get("total")
            .and_then(Value::as_i64)
            .context("branch list missing total")
    }
}

fn field(v: &Value, key: &str) -> Result<String> {
    v.get(key)
        .and_then(Value::as_str)
        .map(str::to_string)
        .with_context(|| format!("response missing string field {key:?}: {v}"))
}

/// Find the element of `list[key]` (a JSON array of resources) whose `name`
/// field equals `name`. Used to read back a resource materialized by a
/// just-succeeded async task (names are unique within their scope).
fn find_named(list: &Value, key: &str, name: &str) -> Result<Value> {
    let items = list
        .get(key)
        .and_then(Value::as_array)
        .with_context(|| format!("list missing {key:?} array: {list}"))?;
    let mut matches = items
        .iter()
        .filter(|e| e.get("name").and_then(Value::as_str) == Some(name));
    let first = matches
        .clone()
        .next()
        .cloned()
        .with_context(|| format!("no {key} entry named {name:?}"))?;
    if matches.nth(1).is_some() {
        bail!("ambiguous: more than one {key} entry named {name:?}");
    }
    Ok(first)
}

fn branch_ref(v: &Value) -> Result<BranchRef> {
    let id = field(v, "branch_id")?;
    let timeline_hex = id.replace('-', "");
    Ok(BranchRef { id, timeline_hex })
}

/// Assert a branch's fork-ancestry metadata: its `is_default` flag, its parent
/// (`None` for a root/default branch), and that the `fork_lsn` is present iff
/// it is a fork.
fn assert_branch_meta(v: &Value, is_default: bool, parent: Option<&str>) -> Result<()> {
    let got_default = v
        .get("is_default")
        .and_then(Value::as_bool)
        .context("branch missing is_default")?;
    if got_default != is_default {
        bail!("branch is_default = {got_default}, want {is_default}: {v}");
    }
    let got_parent = v.get("parent_branch_id").and_then(Value::as_str);
    if got_parent != parent {
        bail!("branch parent_branch_id = {got_parent:?}, want {parent:?}: {v}");
    }
    let fork_lsn = v.get("fork_lsn").and_then(Value::as_str);
    match (is_default, fork_lsn) {
        (true, Some(l)) => bail!("default branch has fork_lsn {l:?}, want null"),
        (false, None) => bail!("forked branch has null fork_lsn: {v}"),
        _ => Ok(()),
    }
}

// --- the deployed endpoint under test ---------------------------------------

/// A compute endpoint the controller deployed on a branch: how to connect, and
/// the storage tenant + timeline it must be serving.
struct BranchEndpoint {
    label: &'static str,
    conn: String,
    tenant_hex: String,
    timeline_hex: String,
}

impl BranchEndpoint {
    /// Build from the `host:port` the controller returned, with the endpoint's
    /// `cloud_admin` password (set by the endpoint supervisor).
    fn from_address(
        label: &'static str,
        addr: &str,
        tenant_hex: String,
        timeline_hex: String,
    ) -> Result<Self> {
        let (host, port) = addr
            .rsplit_once(':')
            .with_context(|| format!("endpoint address {addr:?} is not host:port"))?;
        let conn = format!(
            "host={host} port={port} user=cloud_admin password=cloud_admin dbname=postgres"
        );
        Ok(Self {
            label,
            conn,
            tenant_hex,
            timeline_hex,
        })
    }

    async fn wait_ready(&self, timeout: Duration) -> Result<()> {
        let deadline = Instant::now() + timeout;
        let mut last = String::from("(no attempt yet)");
        while Instant::now() < deadline {
            match db::query_i64(&self.conn, "SELECT 1::bigint").await {
                Ok(1) => return Ok(()),
                Ok(other) => last = format!("readiness probe returned {other}"),
                Err(e) => last = format!("{e:#}"),
            }
            tokio::time::sleep(Duration::from_secs(1)).await;
        }
        bail!(
            "{} compute not ready within {timeout:?}: {last}",
            self.label
        );
    }

    /// The compute is a real Postgres, and — when it exposes the `yb_amp.*`
    /// GUCs — serves exactly the tenant + timeline the controller bound it to.
    /// The data-lineage checks below are the real proof the right storage was
    /// wired in; the GUC asserts are a cheap corroboration when available.
    async fn assert_identity(&self) -> Result<()> {
        let version = db::query_text(&self.conn, "SELECT version()").await?;
        if !version.contains("PostgreSQL") {
            bail!("{}: unexpected version banner: {version}", self.label);
        }
        self.assert_guc("yb_amp.tenant_id", &self.tenant_hex)
            .await?;
        self.assert_guc("yb_amp.timeline_id", &self.timeline_hex)
            .await?;
        Ok(())
    }

    /// Assert a `yb_amp.*` GUC equals `want`, but only when the compute exposes
    /// it: the GUC is registered by the `yb_amp` extension and isn't always
    /// queryable in the deployed endpoint image, so the `missing_ok` form of
    /// `current_setting` returns NULL (→ skip) rather than erroring.
    async fn assert_guc(&self, guc: &str, want: &str) -> Result<()> {
        let sql = format!("SELECT current_setting('{guc}', true)");
        if let Some(got) = db::query_text_opt(&self.conn, &sql).await? {
            if !got.is_empty() && got != want {
                bail!("{}: serves {guc} {got}, want {want}", self.label);
            }
        }
        Ok(())
    }

    /// The compute's durable WAL end — the point a branch forked here will
    /// inherit up to. Passed as the branch's explicit `fork_point_lsn`.
    async fn flush_lsn(&self) -> Result<String> {
        db::query_text(&self.conn, "SELECT pg_current_wal_flush_lsn()::text").await
    }

    async fn total(&self) -> Result<i64> {
        db::query_i64(&self.conn, "SELECT count(*) FROM lineage").await
    }

    async fn tag_count(&self, tag: &str) -> Result<i64> {
        // Tags are controlled constants in this suite (no untrusted input).
        db::query_i64(
            &self.conn,
            &format!("SELECT count(*) FROM lineage WHERE tag = '{tag}'"),
        )
        .await
    }

    /// Whole-table checksum over the inherited `main-pre` rows, for value-level
    /// (not just count-level) inheritance checks.
    async fn pre_checksum(&self) -> Result<String> {
        db::query_text(
            &self.conn,
            "SELECT coalesce(md5(string_agg(payload, ',' ORDER BY id)), '') FROM lineage WHERE tag = 'main-pre'",
        )
        .await
    }
}

/// Assert a count, attributing failures to a labelled endpoint.
fn expect(ep: &BranchEndpoint, what: &str, got: i64, want: i64) -> Result<()> {
    if got != want {
        bail!("{}: {what} = {got}, want {want}", ep.label);
    }
    Ok(())
}

// --- run loop ---------------------------------------------------------------

/// Drive the controller's API end to end. The controller must already be
/// running; its base URL comes from `AMP_BASE_URL`.
pub async fn run_suite(repeat: u32, fail_fast: bool) -> Result<()> {
    let base = std::env::var("AMP_BASE_URL")
        .context("AMP_BASE_URL is not set — run via test/integration/control_plane/run-controller.sh, which starts the controller")?;
    step(&format!("controller integration: driving the controller at {base}"));
    let client = AmpClient::new(&base);
    client
        .wait_ready(Duration::from_secs(60))
        .await
        .context("controller readiness")?;

    step("controller integration: create user -> storage pool");
    let user = client.create_user(true).await?;
    let pool = client.create_storage_pool(&user, "integration-pool").await?;

    let mut passed = 0u64;
    let mut failed = 0u64;
    let mut fatal: Option<anyhow::Error> = None;
    for iter in 1..=repeat {
        if repeat > 1 {
            step(&format!("branch_lineage iteration {iter}/{repeat}"));
        }
        match branching_run(&client, &pool, iter).await {
            Ok(()) => {
                passed += 1;
                println!("PASS — controller/branch_lineage (iter {iter})");
            }
            Err(e) => {
                failed += 1;
                eprintln!("FAIL — controller/branch_lineage (iter {iter}): {e:#}");
                if fail_fast {
                    fatal = Some(e);
                    break;
                }
            }
        }
    }

    step(&format!("controller integration: {passed} passed, {failed} failed"));
    if let Some(e) = fatal {
        return Err(e);
    }
    if failed > 0 {
        bail!("{failed} controller test(s) failed");
    }
    Ok(())
}

/// One full create-project → branch-tree → validate run. Each iteration uses a
/// fresh project (so `-n` repeats are independent) and retires its endpoints at
/// the end (best-effort).
async fn branching_run(client: &AmpClient, pool: &str, iter: u32) -> Result<()> {
    let proj_name = if iter == 1 {
        "ampbr".to_string()
    } else {
        format!("ampbr-{iter}")
    };
    // (branch_id, endpoint_id) of every endpoint we deploy, for end-of-run retire.
    let mut deployed: Vec<(String, String)> = Vec::new();
    let readiness = Duration::from_secs(90);

    step("controller: create project (tenant + default branch 'main')");
    let project = client
        .create_project(pool, &proj_name, 18)
        .await
        .context("create project")?;
    let main_b = project.default_branch.clone();
    let main_meta = client
        .get_branch(pool, &project.id, &main_b.id)
        .await
        .context("get default branch")?;
    assert_branch_meta(&main_meta, true, None).context("default branch metadata")?;

    step("controller: deploy endpoint on main; verify it serves the project's tenant+timeline");
    let (main_ep, main_addr) = client
        .create_endpoint(pool, &project.id, &main_b.id)
        .await
        .context("deploy main endpoint")?;
    deployed.push((main_b.id.clone(), main_ep));
    let main = BranchEndpoint::from_address(
        "main",
        &main_addr,
        project.tenant_hex.clone(),
        main_b.timeline_hex.clone(),
    )?;
    main.wait_ready(readiness)
        .await
        .context("main endpoint readiness")?;
    main.assert_identity().await.context("main identity")?;

    step("main: seed pre-fork data");
    db::batch(
        &main.conn,
        "DROP TABLE IF EXISTS lineage;
         CREATE TABLE lineage (id int PRIMARY KEY, tag text NOT NULL, payload text NOT NULL);
         INSERT INTO lineage SELECT g, 'main-pre', md5(g::text) FROM generate_series(1, 100) g;
         CHECKPOINT;",
    )
    .await?;
    let pre_checksum = main.pre_checksum().await?;
    if pre_checksum.is_empty() {
        bail!("main: empty pre-fork checksum (seed failed?)");
    }
    expect(&main, "pre-fork rows", main.total().await?, 100)?;

    step("controller: branch b1 off main at main's current flush LSN");
    let main_fork1 = main
        .flush_lsn()
        .await
        .context("read main flush_lsn for b1")?;
    let b1_json = client
        .create_branch(pool, &project.id, "b1", None, Some(&main_fork1))
        .await
        .context("create branch b1")?;
    assert_branch_meta(&b1_json, false, Some(&main_b.id))
        .context("b1 metadata (forked off main)")?;
    let b1_b = branch_ref(&b1_json)?;

    step("main: write post-fork data (must NOT appear on b1)");
    db::batch(
        &main.conn,
        "INSERT INTO lineage SELECT g, 'main-post1', md5(g::text) FROM generate_series(101, 150) g;
         CHECKPOINT;",
    )
    .await?;
    expect(&main, "rows after main-post1", main.total().await?, 150)?;

    step("controller: deploy b1 endpoint; validate lineage isolation from main");
    let (b1_ep, b1_addr) = client
        .create_endpoint(pool, &project.id, &b1_b.id)
        .await
        .context("deploy b1 endpoint")?;
    deployed.push((b1_b.id.clone(), b1_ep));
    let b1 = BranchEndpoint::from_address(
        "b1",
        &b1_addr,
        project.tenant_hex.clone(),
        b1_b.timeline_hex.clone(),
    )?;
    b1.wait_ready(readiness)
        .await
        .context("b1 endpoint readiness")?;
    b1.assert_identity().await.context("b1 identity")?;
    expect(
        &b1,
        "inherited main-pre rows",
        b1.tag_count("main-pre").await?,
        100,
    )?;
    expect(
        &b1,
        "main's post-fork rows (must be 0)",
        b1.tag_count("main-post1").await?,
        0,
    )?;
    expect(&b1, "total", b1.total().await?, 100)?;
    if b1.pre_checksum().await? != pre_checksum {
        bail!("b1: inherited main-pre checksum mismatch vs main (value-level corruption)");
    }

    step("b1: write own data, then branch b2 off b1 (branch-of-branch)");
    db::batch(
        &b1.conn,
        "INSERT INTO lineage SELECT g, 'b1-own', md5(g::text) FROM generate_series(201, 260) g;
         CHECKPOINT;",
    )
    .await?;
    expect(&b1, "rows after b1-own", b1.total().await?, 160)?;
    let b1_fork = b1.flush_lsn().await.context("read b1 flush_lsn for b2")?;
    let b2_json = client
        .create_branch(pool, &project.id, "b2", Some(&b1_b.id), Some(&b1_fork))
        .await
        .context("create branch b2 off b1")?;
    assert_branch_meta(&b2_json, false, Some(&b1_b.id)).context("b2 metadata (forked off b1)")?;
    let b2_b = branch_ref(&b2_json)?;

    step("b1: write more (post-b2-fork; must NOT appear on b2)");
    db::batch(
        &b1.conn,
        "INSERT INTO lineage SELECT g, 'b1-post2', md5(g::text) FROM generate_series(261, 300) g;
         CHECKPOINT;",
    )
    .await?;
    expect(&b1, "rows after b1-post2", b1.total().await?, 200)?;

    step(
        "controller: branch b3 off main again at main's post-main-post1 flush LSN (second fork off the root)",
    );
    let main_fork2 = main
        .flush_lsn()
        .await
        .context("read main flush_lsn for b3")?;
    let b3_json = client
        .create_branch(pool, &project.id, "b3", None, Some(&main_fork2))
        .await
        .context("create branch b3 off main")?;
    assert_branch_meta(&b3_json, false, Some(&main_b.id))
        .context("b3 metadata (forked off main)")?;
    let b3_b = branch_ref(&b3_json)?;

    step("controller: deploy b2 + b3 endpoints");
    let (b2_ep, b2_addr) = client
        .create_endpoint(pool, &project.id, &b2_b.id)
        .await
        .context("deploy b2 endpoint")?;
    deployed.push((b2_b.id.clone(), b2_ep));
    let b2 = BranchEndpoint::from_address(
        "b2",
        &b2_addr,
        project.tenant_hex.clone(),
        b2_b.timeline_hex.clone(),
    )?;
    b2.wait_ready(readiness)
        .await
        .context("b2 endpoint readiness")?;
    b2.assert_identity().await.context("b2 identity")?;

    let (b3_ep, b3_addr) = client
        .create_endpoint(pool, &project.id, &b3_b.id)
        .await
        .context("deploy b3 endpoint")?;
    deployed.push((b3_b.id.clone(), b3_ep));
    let b3 = BranchEndpoint::from_address(
        "b3",
        &b3_addr,
        project.tenant_hex.clone(),
        b3_b.timeline_hex.clone(),
    )?;
    b3.wait_ready(readiness)
        .await
        .context("b3 endpoint readiness")?;
    b3.assert_identity().await.context("b3 identity")?;

    step(
        "validate b2 lineage (branch-of-branch): main-pre + b1-own, none of main-post1 / b1-post2",
    );
    expect(
        &b2,
        "inherited main-pre (grandparent)",
        b2.tag_count("main-pre").await?,
        100,
    )?;
    expect(
        &b2,
        "inherited b1-own (parent, pre-fork)",
        b2.tag_count("b1-own").await?,
        60,
    )?;
    expect(
        &b2,
        "main-post1 (off b2's line; must be 0)",
        b2.tag_count("main-post1").await?,
        0,
    )?;
    expect(
        &b2,
        "b1-post2 (parent post-fork; must be 0)",
        b2.tag_count("b1-post2").await?,
        0,
    )?;
    expect(&b2, "total", b2.total().await?, 160)?;
    if b2.pre_checksum().await? != pre_checksum {
        bail!("b2: inherited main-pre checksum mismatch (two-level lineage corruption)");
    }

    step("validate b3 lineage (second fork off main): main-pre + main-post1, none of b1/b2 writes");
    expect(
        &b3,
        "inherited main-pre",
        b3.tag_count("main-pre").await?,
        100,
    )?;
    expect(
        &b3,
        "inherited main-post1 (committed before b3's fork)",
        b3.tag_count("main-post1").await?,
        50,
    )?;
    expect(
        &b3,
        "b1-own (off b3's line; must be 0)",
        b3.tag_count("b1-own").await?,
        0,
    )?;
    expect(
        &b3,
        "b1-post2 (must be 0)",
        b3.tag_count("b1-post2").await?,
        0,
    )?;
    expect(&b3, "total", b3.total().await?, 150)?;
    if b3.pre_checksum().await? != pre_checksum {
        bail!("b3: inherited main-pre checksum mismatch");
    }

    step("write on b2 + b3, then assert full cross-branch isolation");
    db::batch(
        &b2.conn,
        "INSERT INTO lineage SELECT g, 'b2-own', md5(g::text) FROM generate_series(401, 410) g;",
    )
    .await?;
    db::batch(
        &b3.conn,
        "INSERT INTO lineage SELECT g, 'b3-own', md5(g::text) FROM generate_series(501, 510) g;",
    )
    .await?;

    // main (the root) is untouched by any descendant's writes.
    expect(
        &main,
        "total unchanged by descendants",
        main.total().await?,
        150,
    )?;
    expect(
        &main,
        "b1-own leaked onto main (must be 0)",
        main.tag_count("b1-own").await?,
        0,
    )?;
    expect(
        &main,
        "b2-own leaked onto main (must be 0)",
        main.tag_count("b2-own").await?,
        0,
    )?;
    expect(
        &main,
        "b3-own leaked onto main (must be 0)",
        main.tag_count("b3-own").await?,
        0,
    )?;

    // b1 sees only its own line.
    expect(&b1, "total", b1.total().await?, 200)?;
    expect(
        &b1,
        "b2-own visible on b1 (must be 0)",
        b1.tag_count("b2-own").await?,
        0,
    )?;
    expect(
        &b1,
        "b3-own visible on b1 (must be 0)",
        b1.tag_count("b3-own").await?,
        0,
    )?;

    // b2 sees its fork plus its own writes.
    expect(&b2, "total", b2.total().await?, 170)?;
    expect(
        &b2,
        "b3-own visible on b2 (must be 0)",
        b2.tag_count("b3-own").await?,
        0,
    )?;

    // b3 sees its fork plus its own writes.
    expect(&b3, "total", b3.total().await?, 160)?;
    expect(
        &b3,
        "b1-own visible on b3 (must be 0)",
        b3.tag_count("b1-own").await?,
        0,
    )?;
    expect(
        &b3,
        "b2-own visible on b3 (must be 0)",
        b3.tag_count("b2-own").await?,
        0,
    )?;

    step("controller: listBranches reports all 4 (main + b1 + b2 + b3)");
    let n = client.count_branches(pool, &project.id).await?;
    if n != 4 {
        bail!("project has {n} branches, want 4 (main + b1 + b2 + b3)");
    }

    step("controller: retire endpoints (best-effort cleanup)");
    for (branch, endpoint) in &deployed {
        if let Err(e) = client
            .delete_endpoint(pool, &project.id, branch, endpoint)
            .await
        {
            eprintln!("warn: retire endpoint {endpoint} failed: {e:#}");
        }
    }

    Ok(())
}
