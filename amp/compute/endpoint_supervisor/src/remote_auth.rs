//! Giving `cloud_admin` a password, so non-loopback clients can authenticate.
//!
//! The compute spec sets `skip_pg_catalog_updates`, which makes `compute_ctl`
//! skip the block that would apply the spec's `encrypted_password`
//! (`compute_tools/src/compute.rs`). Loopback still works through the `trust`
//! rules in `pg_hba.conf`, but any client arriving from anywhere else — a
//! published port, a Service, a port-forward — matches `host all all all md5`
//! and finds no stored password. So the password is set here, over the loopback
//! trust connection, once Postgres is answering queries.
//!
//! The log line this emits on success is a **contract**, not a nicety: the
//! controller's Docker deployer tails the container's logs for
//! [`READY_MARKER`] and treats it as the endpoint's readiness signal
//! (`controller/internal/deploy/docker.go`). It waits on the password rather than
//! on a TCP accept precisely because clients authenticate with md5. Change the
//! string on one side and the other silently times out and tears the endpoint
//! down.

use std::time::Duration;

use camino::Utf8Path;

/// The readiness signal the controller's Docker deployer waits for.
///
/// LOAD-BEARING: `controller/internal/deploy/docker.go` greps the container log
/// for this exact string. If you change it, change that too.
pub const READY_MARKER: &str = "set cloud_admin password for remote auth";

/// How many times to try before giving up, one second apart. The compute's health
/// probe only proves the port is open; Postgres accepts a connection some way
/// before it will answer a query, and a basebackup restore can widen that gap.
const ATTEMPTS: usize = 60;
const INTERVAL: Duration = Duration::from_secs(1);

/// Single-quotes doubled, which is how a literal quote is escaped inside a
/// single-quoted SQL string. The password reaches `psql` as an argument, so it is
/// never seen by a shell.
fn quote_literal(password: &str) -> String {
    password.replace('\'', "''")
}

/// The statement to run. Split out so a test can pin the escaping without
/// needing a running Postgres.
fn alter_role_sql(password: &str) -> String {
    format!(
        "ALTER ROLE cloud_admin WITH PASSWORD '{}'",
        quote_literal(password)
    )
}

/// Sets the password, retrying until Postgres answers. Failure is a warning: the
/// endpoint is otherwise up, and refusing to supervise it because remote auth
/// could not be configured would turn a degraded endpoint into no endpoint.
///
/// Blocking, so callers that must keep supervising should run it on its own
/// thread — see [`spawn`].
pub fn set_cloud_admin_password(psql: &Utf8Path, port: u16, password: &str) {
    let sql = alter_role_sql(password);
    let mut last = String::new();
    for attempt in 1..=ATTEMPTS {
        let out = std::process::Command::new(psql)
            .args([
                "-h",
                crate::boot::LISTEN_HOST,
                "-p",
                &port.to_string(),
                "-U",
                "cloud_admin",
                "-d",
                "postgres",
                "-v",
                "ON_ERROR_STOP=1",
                "--no-psqlrc",
                "-c",
                &sql,
            ])
            .output();
        match out {
            Ok(o) if o.status.success() => {
                // The controller is waiting for exactly this. Logged as a bare
                // message so the line contains the marker verbatim.
                tracing::info!("{READY_MARKER}");
                return;
            }
            Ok(o) => last = String::from_utf8_lossy(&o.stderr).trim().to_string(),
            Err(e) => last = e.to_string(),
        }
        if attempt < ATTEMPTS {
            std::thread::sleep(INTERVAL);
        }
    }
    tracing::warn!(
        "could not set the cloud_admin password, so remote auth will fail: {}",
        last.chars().take(200).collect::<String>()
    );
}

/// Runs [`set_cloud_admin_password`] on its own thread.
///
/// Detached on purpose: it waits on Postgres, and blocking the caller would delay
/// the monitor loop — so a sibling that crashed meanwhile would not be restarted
/// until this finished.
pub fn spawn(psql: &Utf8Path, port: u16, password: &str) {
    let (psql, password) = (psql.to_owned(), password.to_owned());
    std::thread::spawn(move || set_cloud_admin_password(&psql, port, &password));
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The controller greps for this exact text. A rename here without the
    /// matching change in docker.go makes every docker-driver create time out and
    /// tear the endpoint down, so the literal is pinned.
    #[test]
    fn the_ready_marker_is_the_string_the_controller_waits_for() {
        assert_eq!(READY_MARKER, "set cloud_admin password for remote auth");
    }

    /// A quote in the password must not end the SQL literal early.
    #[test]
    fn a_quote_in_the_password_is_escaped() {
        assert_eq!(quote_literal("o'brien"), "o''brien");
        assert_eq!(
            alter_role_sql("o'brien"),
            "ALTER ROLE cloud_admin WITH PASSWORD 'o''brien'"
        );
        // The classic injection shape would terminate the literal and append a
        // statement. Doubling keeps it inside, so the whole thing is one literal
        // and the payload survives verbatim as the password.
        let sql = alter_role_sql("x'; DROP ROLE cloud_admin; --");
        assert_eq!(
            sql,
            "ALTER ROLE cloud_admin WITH PASSWORD 'x''; DROP ROLE cloud_admin; --'"
        );
        // The property that makes it safe: every quote inside the literal is
        // doubled, so the count between the delimiters is even and none of them
        // closes it early.
        let body = sql
            .strip_prefix("ALTER ROLE cloud_admin WITH PASSWORD '")
            .and_then(|r| r.strip_suffix('\''))
            .expect("statement is one quoted literal");
        assert_eq!(
            body.matches('\'').count() % 2,
            0,
            "unbalanced quotes: {body}"
        );
    }

    #[test]
    fn an_ordinary_password_is_unchanged() {
        assert_eq!(
            alter_role_sql("cloud_admin"),
            "ALTER ROLE cloud_admin WITH PASSWORD 'cloud_admin'"
        );
    }

    /// The retry budget has to outlast a basebackup restore, which is what
    /// delays the compute answering queries after its port opens.
    #[test]
    fn the_retry_budget_is_at_least_a_minute() {
        assert!(INTERVAL * ATTEMPTS as u32 >= Duration::from_secs(60));
    }
}
