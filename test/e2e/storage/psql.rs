//! Thin `psql` wrappers. Typed assertions go through [`crate::db`];
//! `psql` is used only where it's load-bearing: the `.sql`/`.out` golden
//! workloads (whose goldens capture `psql`'s exact aligned text
//! rendering) and the background write/read loops the restart +
//! going-away scenarios drive (which must be killable mid-flight).

use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};

/// The Postgres install built from yb-light's own vendored Postgres
/// (`pg_install/v17`), relative to this crate (`<repo>/test/e2e/storage`).
fn pg_install() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../pg_install/v17")
}

/// Base `psql` command against a compute endpoint: trust auth (empty
/// `PGPASSWORD`), `cloud_admin`, db `postgres`.
///
/// Uses the `psql` built from yb-light's own Postgres — NOT whatever
/// `psql` is on `PATH`. The golden `.sql` workloads use psql
/// meta-commands (e.g. `\d`) whose generated catalog queries must match
/// the compute's Postgres major version; a mismatched system `psql`
/// emits queries for the wrong catalog shape (e.g. `pg_class.relhasoids`,
/// dropped in PG12) and the workload errors out.
fn base(port: u16) -> Command {
    let pg = pg_install();
    let lib = pg.join("lib");
    let ld_library_path = match std::env::var("LD_LIBRARY_PATH") {
        Ok(existing) if !existing.is_empty() => format!("{}:{}", lib.display(), existing),
        _ => lib.display().to_string(),
    };
    let mut cmd = Command::new(pg.join("bin/psql"));
    cmd.env("LD_LIBRARY_PATH", ld_library_path)
        .env("PGPASSWORD", "")
        .args(["-h", "127.0.0.1", "-p"])
        .arg(port.to_string())
        .args(["-U", "cloud_admin", "postgres"]);
    cmd
}

/// Spawn a long-running `psql -c <sql>` in the background, detached from
/// our stdio, returning the child so the caller can kill it later.
/// `ON_ERROR_STOP=0` because these loops are expected to die when their
/// pageserver/safekeeper is yanked — that's the orderly hand-off.
pub fn spawn_bg(port: u16, sql: &str) -> std::io::Result<Child> {
    base(port)
        .args(["-v", "ON_ERROR_STOP=0", "-q", "-c", sql])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
}

/// Run a `.sql` file with `ON_ERROR_STOP=1`, capturing stdout AND stderr
/// **interleaved** (for the golden diff). Returns the captured output;
/// errors if `psql` exits non-zero.
///
/// psql writes `NOTICE`/`WARNING`/`ERROR` to stderr, so goldens that
/// exercise them (e.g. `event-triggers`) need those lines to land in
/// order with the command tags on stdout — exactly what pg_regress gets
/// from `2>&1`. We reproduce that by pointing both child streams at one
/// file handle (cloned, so they share a single OS offset): the kernel
/// interleaves the writes in emission order. A test that wants its
/// intentional errors captured rather than aborting the run overrides the
/// runner's `ON_ERROR_STOP=1` with `\set ON_ERROR_STOP 0` in its `.sql`.
pub fn run_file(port: u16, sql: &Path) -> anyhow::Result<String> {
    let stem = sql
        .file_stem()
        .map(|s| s.to_string_lossy().into_owned())
        .unwrap_or_else(|| "workload".to_string());
    let sink_path =
        std::env::temp_dir().join(format!("e2e-psql-{}-{}.out", std::process::id(), stem));
    let sink = std::fs::File::create(&sink_path)?;

    let status = base(port)
        .args(["-v", "ON_ERROR_STOP=1", "-f"])
        .arg(sql)
        .stdout(sink.try_clone()?)
        .stderr(sink.try_clone()?)
        .status()?;

    let captured = std::fs::read(&sink_path)?;
    let _ = std::fs::remove_file(&sink_path);
    if !status.success() {
        anyhow::bail!("psql -f {} exited with {}", sql.display(), status);
    }
    Ok(String::from_utf8_lossy(&captured).into_owned())
}
