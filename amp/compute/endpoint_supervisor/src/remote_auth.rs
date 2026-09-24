//! Giving `cloud_admin` a password, so non-loopback clients can authenticate.
//!
//! The compute spec sets `skip_pg_catalog_updates`, which makes `compute_ctl`
//! skip the block that would apply the spec's `encrypted_password`
//! (`compute_tools/src/compute.rs`). Loopback still works through the `trust`
//! rules in `pg_hba.conf`, but any client arriving from anywhere else — a
//! published port, a Service, a port-forward — matches
//! `host all all all scram-sha-256` and finds no stored password. So the
//! password is set here, over the loopback trust connection, once Postgres is
//! answering queries.
//!
//! **The password is bootstrap state, not desired state.** Two rules, and both
//! exist because this runs on EVERY boot while the catalog it writes to survives
//! the pod:
//!
//! 1. No `COMPUTE_PASSWORD` means leave the role alone. The control plane sends
//!    the verifier on the create and on nothing else, so a redeploy, a fleet
//!    roll and a `read_only` endpoint all arrive here with it unset — and the
//!    built-in default this used to fall back to would have re-passworded the
//!    role to a well-known string on every one of them.
//! 2. A role that already has a password keeps it. Otherwise a restart would
//!    revert a password its owner set themselves with `ALTER ROLE`, which they
//!    are superuser enough to do and have no reason to expect us to undo.
//!
//! Rule 2 has a known gap: a branch FORK inherits its parent's catalog, so the
//! first endpoint on a fork finds a password already there and declines to apply
//! the one its caller supplied. The fork answers to the parent's password and
//! the caller's does not work. It is logged loudly rather than silently skipped
//! — yugabyte/meko#1461 tracks making "has this endpoint applied its verifier?"
//! answerable, which the catalog alone cannot answer.
//!
//! The log line this emits on success is a **contract**, not a nicety: the
//! controller's Docker deployer tails the container's logs for
//! [`READY_MARKER`] and treats it as the endpoint's readiness signal
//! (`controller/internal/deploy/docker.go`). It waits on the password rather than
//! on a TCP accept precisely because clients authenticate by password. Change
//! the string on one side and the other silently times out and tears the
//! endpoint down. Every path that finishes the decision logs it — including the
//! two that decide to change nothing, because "settled" is what the deployer is
//! waiting for, not "wrote to pg_authid".

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

/// Does `cloud_admin` already have a password? `t`/`f` from a superuser-only
/// catalog we can read because this connection IS the superuser, over loopback.
const HAS_PASSWORD_SQL: &str =
    "SELECT rolpassword IS NOT NULL FROM pg_authid WHERE rolname = 'cloud_admin'";

/// Runs one statement through `psql`, returning its trimmed stdout.
fn psql(psql_bin: &Utf8Path, port: u16, sql: &str) -> Result<String, String> {
    let out = std::process::Command::new(psql_bin)
        .args([
            "-h",
            crate::boot::LISTEN_HOST,
            "-p",
            &port.to_string(),
            "-U",
            "cloud_admin",
            "-d",
            "postgres",
            // Tuples-only + unaligned, so a SELECT prints just `t` or `f`.
            "-tA",
            "-v",
            "ON_ERROR_STOP=1",
            "--no-psqlrc",
            "-c",
            sql,
        ])
        .output()
        .map_err(|e| e.to_string())?;
    if out.status.success() {
        Ok(String::from_utf8_lossy(&out.stdout).trim().to_string())
    } else {
        Err(String::from_utf8_lossy(&out.stderr).trim().to_string())
    }
}

/// Decides what the role's password should be and applies it if anything is to
/// be done, retrying until Postgres answers. Failure is a warning: the endpoint
/// is otherwise up, and refusing to supervise it because remote auth could not
/// be configured would turn a degraded endpoint into no endpoint.
///
/// Blocking, so callers that must keep supervising should run it on its own
/// thread — see [`spawn`].
pub fn configure_remote_auth(psql_bin: &Utf8Path, port: u16, password: Option<&str>) {
    let mut last = String::new();
    for attempt in 1..=ATTEMPTS {
        // The same query doubles as the wait for Postgres to answer queries at
        // all, so nothing decides anything against a compute that is still
        // restoring its basebackup.
        match psql(psql_bin, port, HAS_PASSWORD_SQL) {
            Ok(answer) => {
                let has_password = answer == "t";
                match (password, has_password) {
                    (None, _) => {
                        // Rule 1. Not a warning: this is every redeploy and every
                        // read_only endpoint, not an anomaly.
                        tracing::info!(
                            "{READY_MARKER}: skipped, COMPUTE_PASSWORD is unset so the \
                             cloud_admin password is left as the catalog has it"
                        );
                        return;
                    }
                    (Some(_), true) => {
                        // Rule 2. Loud, because one shape of this is a fork whose
                        // caller supplied a password that will not work.
                        tracing::warn!(
                            "{READY_MARKER}: skipped, cloud_admin already has a password \
                             and this endpoint's own password was NOT applied. Expected on \
                             a restart; on the FIRST endpoint of a forked branch it means \
                             clients must use the parent branch's password (yugabyte/meko#1461)"
                        );
                        return;
                    }
                    (Some(pw), false) => match psql(psql_bin, port, &alter_role_sql(pw)) {
                        Ok(_) => {
                            // The controller is waiting for exactly this.
                            tracing::info!("{READY_MARKER}");
                            return;
                        }
                        Err(e) => last = e,
                    },
                }
            }
            Err(e) => last = e,
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

/// Runs [`configure_remote_auth`] on its own thread.
///
/// Detached on purpose: it waits on Postgres, and blocking the caller would delay
/// the monitor loop — so a sibling that crashed meanwhile would not be restarted
/// until this finished.
pub fn spawn(psql_bin: &Utf8Path, port: u16, password: Option<&str>) {
    let (psql_bin, password) = (psql_bin.to_owned(), password.map(str::to_owned));
    std::thread::spawn(move || configure_remote_auth(&psql_bin, port, password.as_deref()));
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

    /// A SCRAM verifier is what the control plane now sends, and it must survive
    /// the escaping untouched: Postgres stores a well-formed one verbatim, and
    /// anything altered on the way would be hashed AS the password instead.
    #[test]
    fn a_scram_verifier_passes_through_unchanged() {
        let verifier = "SCRAM-SHA-256$4096:W22ZaJ0SNY7soEsUEjb6gQ==$\
                        WG5d8oPm3OtcPnkdi4Uo7BkeZkBFzpcXkuLmtbsT4qY=:\
                        wfPLwcE6nTWhTAmQ7tl2KeoiWGPlZqQxSrmfPwDl2dU=";
        assert_eq!(quote_literal(verifier), verifier);
        assert_eq!(
            alter_role_sql(verifier),
            format!("ALTER ROLE cloud_admin WITH PASSWORD '{verifier}'")
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
