//! Thin `psql` wrappers. Typed assertions go through [`crate::db`];
//! `psql` is used only where it's load-bearing: the `.sql`/`.out` golden
//! workloads (whose goldens capture `psql`'s exact aligned text
//! rendering), the background write/read loops the restart +
//! going-away scenarios drive (which must be killable mid-flight), and a
//! `pg_dump` then `psql -f` restore, which is the client pair under test.

use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};

/// The Postgres install built from yb-amp's own vendored Postgres — the same one
/// the compute runs, so `psql`'s catalog queries match its major version.
///
/// `POSTGRES_DISTRIB_DIR` when the runner set it: run.sh points it at the tree
/// matching its profile — `pg_install-debug/` for a default debug run,
/// `pg_install-release/` for `--release` (see pg_tree.sh). Otherwise the
/// repo-root default, relative to this crate
/// (`<repo>/test/integration/compute`), which is the debug tree.
fn pg_install() -> PathBuf {
    match std::env::var_os("POSTGRES_DISTRIB_DIR") {
        Some(root) => PathBuf::from(root).join("v18"),
        None => Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../pg_install-debug/v18"),
    }
}

/// Base `psql` command against a compute endpoint: trust auth (empty
/// `PGPASSWORD`), `cloud_admin`, db `postgres`.
///
/// Uses the `psql` built from yb-amp's own Postgres — NOT whatever
/// `psql` is on `PATH`. The golden `.sql` workloads use psql
/// meta-commands (e.g. `\d`) whose generated catalog queries must match
/// the compute's Postgres major version; a mismatched system `psql`
/// emits queries for the wrong catalog shape (e.g. `pg_class.relhasoids`,
/// dropped in PG12) and the workload errors out.
fn base(port: u16) -> Command {
    client("psql", port, "postgres")
}

/// `bin` (`psql`, `pg_dump`) from that same install, connected as [`base`] is,
/// to database `dbname`.
fn client(bin: &str, port: u16, dbname: &str) -> Command {
    let pg = pg_install();
    let lib = pg.join("lib");
    let ld_library_path = match std::env::var("LD_LIBRARY_PATH") {
        Ok(existing) if !existing.is_empty() => format!("{}:{}", lib.display(), existing),
        _ => lib.display().to_string(),
    };
    let mut cmd = Command::new(pg.join("bin").join(bin));
    cmd.env("LD_LIBRARY_PATH", ld_library_path)
        .env("PGPASSWORD", "")
        .args(["-h", "127.0.0.1", "-p"])
        .arg(port.to_string())
        .args(["-U", "cloud_admin", dbname]);
    cmd
}

/// `pg_dump <db>` into `file`, as plain SQL. Errors with pg_dump's stderr if it
/// exits non-zero.
pub fn dump(port: u16, db: &str, file: &Path) -> anyhow::Result<()> {
    let out = client("pg_dump", port, db).arg("-f").arg(file).output()?;
    if !out.status.success() {
        anyhow::bail!(
            "pg_dump {db} exited with {}: {}",
            out.status,
            String::from_utf8_lossy(&out.stderr)
        );
    }
    Ok(())
}

/// `psql <db> -f <file>`, the plain-SQL restore as an operator runs it: without
/// `ON_ERROR_STOP`, so a statement that fails is reported on stderr and the
/// restore carries on to exit 0. Returns psql's stderr, which is the only place
/// a skipped table shows; errors, with that stderr, only if psql exits non-zero.
pub fn restore(port: u16, db: &str, file: &Path) -> anyhow::Result<String> {
    let out = client("psql", port, db)
        .args(["-X", "-q", "-f"])
        .arg(file)
        .stdout(Stdio::null())
        .output()?;
    let stderr = String::from_utf8_lossy(&out.stderr).into_owned();
    if !out.status.success() {
        anyhow::bail!(
            "psql {db} -f {} exited with {}: {stderr}",
            file.display(),
            out.status
        );
    }
    Ok(stderr)
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
    let sink_path = std::env::temp_dir().join(format!(
        "integration-psql-{}-{}.out",
        std::process::id(),
        stem
    ));
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
