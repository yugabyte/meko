//! Thin `psql` wrappers. Typed assertions go through [`crate::db`];
//! `psql` is used only where it's load-bearing: the `.sql`/`.out` golden
//! workloads (whose goldens capture `psql`'s exact aligned text
//! rendering) and the background write/read loops the restart +
//! going-away scenarios drive (which must be killable mid-flight).

use std::path::Path;
use std::process::{Child, Command, Stdio};

/// Base `psql` command against a compute endpoint: trust auth (empty
/// `PGPASSWORD`), `cloud_admin`, db `postgres`.
fn base(port: u16) -> Command {
    let mut cmd = Command::new("psql");
    cmd.env("PGPASSWORD", "")
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

/// Run a `.sql` file with `ON_ERROR_STOP=1`, capturing stdout (for the
/// golden diff) while stderr flows to our log. Returns the captured
/// stdout; errors if `psql` exits non-zero.
pub fn run_file(port: u16, sql: &Path) -> anyhow::Result<String> {
    let out = base(port)
        .args(["-v", "ON_ERROR_STOP=1", "-f"])
        .arg(sql)
        .stderr(Stdio::inherit())
        .output()?;
    if !out.status.success() {
        anyhow::bail!("psql -f {} exited with {}", sql.display(), out.status);
    }
    Ok(String::from_utf8_lossy(&out.stdout).into_owned())
}
