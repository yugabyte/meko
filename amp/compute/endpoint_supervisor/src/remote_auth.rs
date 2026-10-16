//! Giving the login role a password, so non-loopback clients can authenticate.
//!
//! The login role is `cloud_admin` unless the endpoint has a [`CustomerRole`],
//! in which case it is that role and `cloud_admin` keeps no password at all
//! (AMP-15) — see [`crate::customer_role`].
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
//! 1. A role that already has a password keeps it, whatever `COMPUTE_PASSWORD`
//!    says. Otherwise a restart would revert a password its owner set themselves
//!    with `ALTER ROLE`, which they are superuser enough to do and have no reason
//!    to expect us to undo.
//! 2. No `COMPUTE_PASSWORD` means leave the role alone. The control plane sends
//!    the verifier on the create and on nothing else, so a redeploy, a fleet
//!    roll and a `read_only` endpoint all arrive here with it unset — and the
//!    built-in default this used to fall back to would have re-passworded the
//!    role to a well-known string on every one of them.
//!
//! A customer role adds a third: one with no password of its own, on an endpoint
//! whose `cloud_admin` has a SCRAM one, takes that one over, and `cloud_admin`'s
//! is cleared in the same transaction. That is a re-render carrying an endpoint
//! created before the customer role onto it — rule 2 means there is no
//! `COMPUTE_PASSWORD` to give it — and its clients keep their password and change
//! only the user name. An md5 `cloud_admin` password cannot move (md5 is salted
//! with the user name), but it has not let anyone in from outside since the
//! remote rule became `scram-sha-256` only, so it is cleared all the same and
//! the endpoint has no remote login until the customer role gets a password.
//!
//! A role of the customer role's name that the bootstrap did not make — one the
//! customer created while they were superuser — is never taken over: the run
//! fails and changes nothing (see [`CustomerRole::conflict_sql`]).
//!
//! Rule 1 is why a fork cannot take a password of its own: it inherits its
//! parent's catalog, so its first endpoint finds one already there. The control
//! plane therefore sends a password only to a root's first writer, and asks
//! `GET /v1/remote-auth` ([`Status`]) whether it landed and at what LSN — the
//! lowest point a fork of that root can be taken and still carry it
//! (yugabyte/meko#1461).
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

use std::sync::{Arc, Mutex};
use std::time::Duration;

use camino::Utf8Path;
use utils::lsn::Lsn;

use crate::customer_role::CustomerRole;

/// The readiness signal the controller's Docker deployer waits for.
///
/// LOAD-BEARING: `controller/internal/deploy/docker.go` greps the container log
/// for this exact string. If you change it, change that too. It names
/// `cloud_admin` even when the password went to a [`CustomerRole`]: the text is
/// the contract, not a description.
pub const READY_MARKER: &str = "set cloud_admin password for remote auth";

/// How many times to try before giving up, one second apart. The compute's health
/// probe only proves the port is open; Postgres accepts a connection some way
/// before it will answer a query, and a basebackup restore can widen that gap.
const ATTEMPTS: usize = 60;
const INTERVAL: Duration = Duration::from_secs(1);

/// What remote auth decided on this boot, as `GET /v1/remote-auth` reports it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Outcome {
    /// Not decided yet. A caller-rendered endpoint never runs remote auth, so
    /// it stays here.
    Pending,
    /// This process ran the `ALTER ROLE` and it committed. `lsn` is the WAL
    /// flush position read after the commit, so a fork at or above it carries
    /// the password; `None` when that read failed.
    Applied { lsn: Option<Lsn> },
    /// The catalog already had a password, whether or not one was supplied
    /// (rule 1).
    AlreadySet,
    /// The catalog has no password and none was supplied (rule 2), so remote
    /// auth fails until one is set.
    Unset,
    /// Gave up: Postgres never answered, or would not take the statement.
    Failed,
}

impl Outcome {
    /// The wire name.
    pub fn name(&self) -> &'static str {
        match self {
            Outcome::Pending => "pending",
            Outcome::Applied { .. } => "applied",
            Outcome::AlreadySet => "already_set",
            Outcome::Unset => "unset",
            Outcome::Failed => "failed",
        }
    }
}

/// The outcome, shared between the remote-auth thread and the settings API.
///
/// One for the process, because the API is, while remote auth runs once per
/// lease. Two rules keep a later lease from muddying the answer. A superseded
/// run records nothing but `Applied`: a lost lease does not stop its thread,
/// which may still be retrying, but a password it applied is this process's
/// all the same. And `Applied` is never replaced: the next lease's run finds
/// that password and would report `AlreadySet`, true of that run but without
/// the LSN the controller is asking for.
#[derive(Debug)]
pub struct Status(Mutex<StatusInner>);

#[derive(Debug)]
struct StatusInner {
    run: u64,
    outcome: Outcome,
}

impl Default for Status {
    fn default() -> Self {
        Self(Mutex::new(StatusInner {
            run: 0,
            outcome: Outcome::Pending,
        }))
    }
}

impl Status {
    pub fn get(&self) -> Outcome {
        self.lock().outcome
    }

    /// Starts a run, returning the token [`Self::finish`] takes.
    pub(crate) fn begin(&self) -> u64 {
        let mut s = self.lock();
        s.run += 1;
        if !matches!(s.outcome, Outcome::Applied { .. }) {
            s.outcome = Outcome::Pending;
        }
        s.run
    }

    pub(crate) fn finish(&self, run: u64, outcome: Outcome) {
        let mut s = self.lock();
        let applied = matches!(outcome, Outcome::Applied { .. });
        if (s.run == run || applied) && !matches!(s.outcome, Outcome::Applied { .. }) {
            s.outcome = outcome;
        }
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, StatusInner> {
        self.0.lock().unwrap_or_else(|e| e.into_inner())
    }
}

/// Single-quotes doubled, which is how a literal quote is escaped inside a
/// single-quoted SQL string. The password reaches `psql` as an argument, so it is
/// never seen by a shell.
fn quote_literal(password: &str) -> String {
    password.replace('\'', "''")
}

/// The statement to run. Split out so a test can pin the escaping without
/// needing a running Postgres. `role` is `cloud_admin` or a name
/// [`CustomerRole::new`] accepted, neither of which needs quoting.
fn alter_role_sql(role: &str, password: &str) -> String {
    format!(
        "ALTER ROLE {role} WITH PASSWORD '{}'",
        quote_literal(password)
    )
}

/// Moves `cloud_admin`'s verifier onto `role`. Postgres stores a SCRAM verifier
/// verbatim, and SCRAM's is not salted with the user name, so it authenticates
/// the same password under the new name.
fn hand_over_sql(role: &str) -> String {
    format!(
        "DO $amp_auth$ BEGIN EXECUTE pg_catalog.format('ALTER ROLE {role} WITH PASSWORD %L', \
         (SELECT rolpassword FROM pg_catalog.pg_authid \
         WHERE rolname OPERATOR(pg_catalog.=) 'cloud_admin')); END $amp_auth$"
    )
}

/// Leaves `cloud_admin` reachable only by what needs no password: loopback.
const CLEAR_ADMIN_SQL: &str = "ALTER ROLE cloud_admin WITH PASSWORD NULL";

/// Has the login role a password; has `cloud_admin` one; is that one SCRAM. One
/// `t|f|f` row, from a superuser-only catalog we can read because this
/// connection IS the superuser, over loopback. Without a customer role the login
/// is `cloud_admin`, and only the first column is used.
fn state_sql(login: &str) -> String {
    let has = |role: &str, test: &str| {
        format!(
            "coalesce((SELECT {test} FROM pg_catalog.pg_authid \
             WHERE rolname OPERATOR(pg_catalog.=) '{role}'), false)"
        )
    };
    format!(
        "SELECT {}, {}, {}",
        has(login, "rolpassword IS NOT NULL"),
        has("cloud_admin", "rolpassword IS NOT NULL"),
        has("cloud_admin", "rolpassword LIKE 'SCRAM-SHA-256$%'"),
    )
}

/// What the catalog holds, as [`state_sql`] reads it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct State {
    login_has_password: bool,
    admin_has_password: bool,
    admin_password_is_scram: bool,
}

impl State {
    fn parse(row: &str) -> Option<Self> {
        let mut it = row.split('|').map(|f| f == "t");
        let s = Self {
            login_has_password: it.next()?,
            admin_has_password: it.next()?,
            admin_password_is_scram: it.next()?,
        };
        it.next().is_none().then_some(s)
    }
}

/// What to do about the login role's password.
#[derive(Debug, Clone, PartialEq, Eq)]
enum Password<'a> {
    /// Rule 1: the role has one already.
    Keep,
    /// Rule 2: no `COMPUTE_PASSWORD` and nothing to take over.
    Unset,
    Apply(&'a str),
    /// A customer role takes over `cloud_admin`'s SCRAM password.
    HandOver,
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct Decision<'a> {
    password: Password<'a>,
    /// Clear `cloud_admin`'s password. Always, with a customer role: a SCRAM one
    /// has just moved to the customer, and an md5 one admits nobody remotely
    /// (the remote rule is `scram-sha-256` only), so keeping either would only
    /// leave a superuser password lying in the catalog.
    clear_admin: bool,
}

fn decide(password: Option<&str>, customer: bool, s: State) -> Decision<'_> {
    let password = match (s.login_has_password, password) {
        (true, _) => Password::Keep,
        (false, Some(pw)) => Password::Apply(pw),
        (false, None) if customer && s.admin_password_is_scram => Password::HandOver,
        (false, None) => Password::Unset,
    };
    Decision {
        clear_admin: customer && s.admin_has_password,
        password,
    }
}

/// Its own `psql` invocation, after the `ALTER ROLE`'s has exited: in one `-c`
/// string the two share an implicit transaction, and this would read a position
/// before the commit record. A commit flushes its record before returning, so it
/// ends at or before what this returns. The flush position rather than the
/// insert one: when the record ends on a page boundary the insert position is
/// past the next page's header, above the safekeeper's flush LSN that a default
/// fork is taken at, which would refuse every default fork of an idle root.
const FLUSH_LSN_SQL: &str = "SELECT pg_current_wal_flush_lsn()";

/// One statement against the compute, returning its trimmed stdout. A trait so
/// the decision can be tested without a Postgres.
trait Sql {
    fn run(&self, sql: &str) -> Result<String, String>;
}

struct Psql<'a> {
    bin: &'a Utf8Path,
    port: u16,
}

impl Sql for Psql<'_> {
    fn run(&self, sql: &str) -> Result<String, String> {
        let out = std::process::Command::new(self.bin)
            .args([
                "-h",
                crate::boot::LISTEN_HOST,
                "-p",
                &self.port.to_string(),
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
}

/// An `ALTER ROLE` error, fit to log. psql follows a server error with a
/// `LINE 1:` echo of the statement, password and all, so only the first line is
/// kept, and the password is masked in case the message itself quotes it.
fn scrub(err: &str, password: &str) -> String {
    let first = err.lines().next().unwrap_or_default();
    if password.is_empty() {
        return first.to_string();
    }
    first.replace(password, "<redacted>")
}

/// Decides what the login role's password should be and applies it if anything
/// is to be done, retrying until Postgres answers. With a `customer` role it
/// first creates that role ([`CustomerRole::bootstrap_sql`]) and the password is
/// its; without one it is `cloud_admin`'s, as it always was.
///
/// Failure is a warning: the endpoint is otherwise up, and refusing to supervise
/// it because remote auth could not be configured would turn a degraded endpoint
/// into no endpoint.
fn configure_remote_auth(
    db: &impl Sql,
    password: Option<&str>,
    customer: Option<&CustomerRole>,
    attempts: usize,
    interval: Duration,
) -> Outcome {
    let login = customer.map_or("cloud_admin", |c| c.name.as_str());
    let mut last = String::new();
    for attempt in 1..=attempts {
        if attempt > 1 {
            std::thread::sleep(interval);
        }
        // The first query is also the wait for Postgres to answer queries at
        // all, so nothing is decided against a compute still restoring its
        // basebackup. The bootstrap goes first: the role has to exist before its
        // password can be read.
        if let Some(c) = customer {
            match db.run(&c.conflict_sql()) {
                Ok(answer) if answer == "t" => {
                    tracing::error!(
                        "role {login} exists but this supervisor did not create it, so it is not \
                         taken over and nothing is changed: cloud_admin keeps its password and \
                         stays the login. Choose a CUSTOMER_ROLE no existing role has (AMP-15)"
                    );
                    return Outcome::Failed;
                }
                Ok(_) => {}
                Err(e) => {
                    last = e;
                    continue;
                }
            }
            if let Err(e) = db.run(&c.bootstrap_sql()) {
                last = e;
                continue;
            }
        }
        let state = match db
            .run(&state_sql(login))
            .map(|row| (State::parse(&row), row))
        {
            Ok((Some(s), _)) => s,
            Ok((None, row)) => {
                last = format!("unexpected state row {row:?}");
                continue;
            }
            Err(e) => {
                last = e;
                continue;
            }
        };
        let d = decide(password, customer.is_some(), state);

        // One query, so the customer never loses cloud_admin's password without
        // having gained its own.
        let mut sql = Vec::new();
        match d.password {
            Password::Apply(pw) => sql.push(alter_role_sql(login, pw)),
            Password::HandOver => sql.push(hand_over_sql(login)),
            Password::Keep | Password::Unset => {}
        }
        if d.clear_admin {
            sql.push(CLEAR_ADMIN_SQL.to_string());
        }
        if !sql.is_empty()
            && let Err(e) = db.run(&sql.join(";\n"))
        {
            last = scrub(&e, password.unwrap_or_default());
            continue;
        }

        let admin = if d.clear_admin {
            "; cloud_admin's password cleared"
        } else {
            ""
        };
        if customer.is_some() && state.admin_has_password && d.password == Password::Unset {
            tracing::warn!(
                "cloud_admin's password was md5, so {login} could not take it over; it had not \
                 admitted anyone remotely since the remote rule became scram-sha-256 only, and \
                 is cleared. The endpoint has no remote login until {login} is given a password"
            );
        }
        return match d.password {
            Password::Keep => {
                tracing::info!("{READY_MARKER}: skipped, {login} already has a password{admin}");
                Outcome::AlreadySet
            }
            Password::Unset => {
                tracing::warn!(
                    "{READY_MARKER}: skipped, COMPUTE_PASSWORD is unset and {login} has no \
                     password, so remote auth will fail"
                );
                Outcome::Unset
            }
            Password::Apply(_) => {
                // The controller is waiting for exactly this.
                if customer.is_none() {
                    tracing::info!("{READY_MARKER}");
                } else {
                    tracing::info!("{READY_MARKER}: role {login}{admin}");
                }
                Outcome::Applied { lsn: flush_lsn(db) }
            }
            // A password is in the catalog, set at no LSN the control plane asked
            // for: what `already_set` means to it.
            Password::HandOver => {
                tracing::info!(
                    "{READY_MARKER}: {login} took over cloud_admin's password, so clients keep \
                     it and log in as {login} from now on{admin}"
                );
                Outcome::AlreadySet
            }
        };
    }
    tracing::warn!(
        "could not configure the {login} password, so remote auth will fail: {}",
        last.chars().take(200).collect::<String>()
    );
    Outcome::Failed
}

/// Where the WAL is flushed to once the password has committed (see
/// [`FLUSH_LSN_SQL`]).
fn flush_lsn(db: &impl Sql) -> Option<Lsn> {
    match db.run(FLUSH_LSN_SQL).map(|out| out.parse::<Lsn>()) {
        Ok(Ok(lsn)) => Some(lsn),
        _ => {
            tracing::warn!(
                "the cloud_admin password is set, but the LSN after it could not be read, so \
                 the control plane cannot record where this branch's forks carry it"
            );
            None
        }
    }
}

/// Runs [`configure_remote_auth`] on its own thread and records its outcome in
/// `status`.
///
/// Detached on purpose: it waits on Postgres, and blocking the caller would delay
/// the monitor loop — so a sibling that crashed meanwhile would not be restarted
/// until this finished.
pub fn spawn(
    psql_bin: &Utf8Path,
    port: u16,
    password: Option<&str>,
    customer: Option<CustomerRole>,
    status: Arc<Status>,
) {
    let (psql_bin, password) = (psql_bin.to_owned(), password.map(str::to_owned));
    let run = status.begin();
    std::thread::spawn(move || {
        let db = Psql {
            bin: &psql_bin,
            port,
        };
        let outcome = configure_remote_auth(
            &db,
            password.as_deref(),
            customer.as_ref(),
            ATTEMPTS,
            INTERVAL,
        );
        status.finish(run, outcome);
    });
}

#[cfg(test)]
mod tests {
    use std::cell::RefCell;
    use std::collections::VecDeque;

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
            alter_role_sql("cloud_admin", "o'brien"),
            "ALTER ROLE cloud_admin WITH PASSWORD 'o''brien'"
        );
        // The classic injection shape would terminate the literal and append a
        // statement. Doubling keeps it inside, so the whole thing is one literal
        // and the payload survives verbatim as the password.
        let sql = alter_role_sql("cloud_admin", "x'; DROP ROLE cloud_admin; --");
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
            alter_role_sql("cloud_admin", verifier),
            format!("ALTER ROLE cloud_admin WITH PASSWORD '{verifier}'")
        );
    }

    #[test]
    fn an_ordinary_password_is_unchanged() {
        assert_eq!(
            alter_role_sql("cloud_admin", "cloud_admin"),
            "ALTER ROLE cloud_admin WITH PASSWORD 'cloud_admin'"
        );
    }

    /// The retry budget has to outlast a basebackup restore, which is what
    /// delays the compute answering queries after its port opens.
    #[test]
    fn the_retry_budget_is_at_least_a_minute() {
        assert!(INTERVAL * ATTEMPTS as u32 >= Duration::from_secs(60));
    }

    const PW: &str = "SCRAM-SHA-256$4096:c2FsdA==$c3RvcmVk:c2VydmVy";

    /// [`state_sql`] rows without a customer role, where the login is
    /// `cloud_admin` and so the first two columns agree.
    const NONE: &str = "f|f|f";
    const SET: &str = "t|t|t";

    fn customer() -> CustomerRole {
        CustomerRole::new("amp_admin", "yb_amp_superuser").unwrap()
    }

    #[test]
    fn the_state_row_parses_and_nothing_else_does() {
        assert_eq!(
            State::parse("t|f|f"),
            Some(State {
                login_has_password: true,
                admin_has_password: false,
                admin_password_is_scram: false,
            })
        );
        assert_eq!(State::parse("t|f"), None);
        assert_eq!(State::parse("t|f|f|t"), None);
    }

    fn state(login: bool, admin: bool, scram: bool) -> State {
        State {
            login_has_password: login,
            admin_has_password: admin,
            admin_password_is_scram: scram,
        }
    }

    /// Without a customer role, cloud_admin's password is never cleared.
    #[test]
    fn without_a_customer_role_cloud_admin_is_never_cleared() {
        for s in [
            state(true, true, true),
            state(false, false, false),
            state(false, true, true),
        ] {
            assert!(!decide(None, false, s).clear_admin, "{s:?}");
            assert!(!decide(Some("v"), false, s).clear_admin, "{s:?}");
        }
        assert_eq!(
            decide(None, false, state(false, true, true)).password,
            Password::Unset,
            "only a customer role takes cloud_admin's password over"
        );
    }

    /// A new endpoint: the customer gets the caller's password, and cloud_admin
    /// — which has none — is left without one.
    #[test]
    fn a_new_customer_endpoint_passwords_the_customer() {
        let d = decide(Some("v"), true, state(false, false, false));
        assert_eq!(d.password, Password::Apply("v"));
        assert!(!d.clear_admin);
    }

    /// An endpoint created before the customer role, re-rendered onto it: no
    /// COMPUTE_PASSWORD, so the customer takes cloud_admin's, and cloud_admin
    /// loses it in the same query.
    #[test]
    fn an_existing_endpoint_hands_its_scram_password_over() {
        let d = decide(None, true, state(false, true, true));
        assert_eq!(d.password, Password::HandOver);
        assert!(d.clear_admin);
    }

    /// Restarts after the handover, or a cloud_admin password set by hand since:
    /// the customer keeps its own and cloud_admin's goes.
    #[test]
    fn a_customer_with_a_password_keeps_it_and_cloud_admin_loses_its() {
        let d = decide(Some("v"), true, state(true, true, true));
        assert_eq!(d.password, Password::Keep);
        assert!(d.clear_admin);
        assert!(!decide(None, true, state(true, false, false)).clear_admin);
    }

    /// md5 is salted with the user name, so it cannot move. It admits nobody
    /// remotely anyway (the remote rule is scram-sha-256 only), so it is cleared
    /// rather than left as a superuser password in the catalog.
    #[test]
    fn an_md5_cloud_admin_password_is_cleared_not_moved() {
        let d = decide(None, true, state(false, true, false));
        assert_eq!(d.password, Password::Unset);
        assert!(d.clear_admin);
        let d = decide(Some("v"), true, state(false, true, false));
        assert_eq!(d.password, Password::Apply("v"));
        assert!(d.clear_admin);
    }

    #[test]
    fn the_handover_reads_cloud_admins_verifier_into_the_customer() {
        assert_eq!(
            hand_over_sql("amp_admin"),
            "DO $amp_auth$ BEGIN EXECUTE pg_catalog.format('ALTER ROLE amp_admin WITH PASSWORD %L', \
             (SELECT rolpassword FROM pg_catalog.pg_authid \
             WHERE rolname OPERATOR(pg_catalog.=) 'cloud_admin')); END $amp_auth$"
        );
    }

    /// The whole run with a customer role: bootstrap, read, then the password
    /// and the clear in one query, then the LSN on its own — so the fork floor
    /// is past the commit that cleared cloud_admin too.
    #[test]
    fn a_customer_run_bootstraps_then_applies_and_clears_in_one_query() {
        let c = customer();
        let db = Scripted::new(&[Ok("f"), Ok(""), Ok("f|t|t"), Ok(""), Ok("0/40")]);
        assert_eq!(
            configure_remote_auth(&db, Some(PW), Some(&c), 5, Duration::ZERO),
            Outcome::Applied { lsn: lsn("0/40") }
        );
        assert_eq!(
            *db.sent.borrow(),
            [
                c.conflict_sql(),
                c.bootstrap_sql(),
                state_sql("amp_admin"),
                format!("{};\n{CLEAR_ADMIN_SQL}", alter_role_sql("amp_admin", PW)),
                FLUSH_LSN_SQL.to_string(),
            ]
        );
    }

    /// A handover sets a password the control plane never sent, at no LSN it
    /// asked about: `already_set`, which gives the root a 0/0 fork floor.
    #[test]
    fn a_handover_is_already_set() {
        let c = customer();
        let db = Scripted::new(&[Ok("f"), Ok(""), Ok("f|t|t"), Ok("")]);
        assert_eq!(
            configure_remote_auth(&db, None, Some(&c), 5, Duration::ZERO),
            Outcome::AlreadySet
        );
        assert_eq!(
            db.sent.borrow()[3],
            format!("{};\n{CLEAR_ADMIN_SQL}", hand_over_sql("amp_admin"))
        );
    }

    /// A failed bootstrap decides nothing, and is retried from the top.
    #[test]
    fn a_failed_bootstrap_is_retried_before_anything_is_decided() {
        let c = customer();
        let db = Scripted::new(&[Ok("f"), Err("starting up"), Ok("f"), Ok(""), Ok("t|f|f")]);
        assert_eq!(
            configure_remote_auth(&db, None, Some(&c), 5, Duration::ZERO),
            Outcome::AlreadySet
        );
        assert_eq!(db.sent.borrow()[2], c.conflict_sql());
        assert_eq!(db.sent.borrow()[3], c.bootstrap_sql());
    }

    /// A role of the customer role's name that the bootstrap did not make is
    /// the customer's own: taking it over would hand its password-holder the
    /// group's privileges and lock out the clients using cloud_admin. Nothing
    /// past the check runs.
    #[test]
    fn a_role_the_bootstrap_did_not_make_is_left_alone() {
        let c = customer();
        let db = Scripted::new(&[Ok("t")]);
        assert_eq!(
            configure_remote_auth(&db, Some(PW), Some(&c), 5, Duration::ZERO),
            Outcome::Failed
        );
        assert_eq!(*db.sent.borrow(), [c.conflict_sql()]);
    }

    #[test]
    fn the_conflict_check_names_the_login_and_the_group() {
        let sql = customer().conflict_sql();
        assert!(
            sql.contains("r.rolname OPERATOR(pg_catalog.=) 'amp_admin'"),
            "{sql}"
        );
        assert!(
            sql.contains("g.rolname OPERATOR(pg_catalog.=) 'yb_amp_superuser'"),
            "{sql}"
        );
    }

    /// A compute that answers from a script, recording what it was sent.
    /// Running out of script is a connection error, like a compute that is not up.
    struct Scripted {
        answers: RefCell<VecDeque<Result<&'static str, &'static str>>>,
        sent: RefCell<Vec<String>>,
    }

    impl Scripted {
        fn new(answers: &[Result<&'static str, &'static str>]) -> Self {
            Self {
                answers: RefCell::new(answers.iter().copied().collect()),
                sent: RefCell::new(Vec::new()),
            }
        }

        fn decide(&self, password: Option<&str>) -> Outcome {
            configure_remote_auth(self, password, None, 5, Duration::ZERO)
        }
    }

    impl Sql for Scripted {
        fn run(&self, sql: &str) -> Result<String, String> {
            self.sent.borrow_mut().push(sql.to_string());
            match self.answers.borrow_mut().pop_front() {
                Some(Ok(out)) => Ok(out.to_string()),
                Some(Err(e)) => Err(e.to_string()),
                None => Err("connection refused".to_string()),
            }
        }
    }

    fn lsn(s: &str) -> Option<Lsn> {
        Some(s.parse().expect("an LSN"))
    }

    #[test]
    fn no_password_and_none_in_the_catalog_is_unset_and_writes_nothing() {
        let db = Scripted::new(&[Ok(NONE)]);
        assert_eq!(db.decide(None), Outcome::Unset);
        assert_eq!(*db.sent.borrow(), [state_sql("cloud_admin")]);
    }

    /// The catalog decides, not COMPUTE_PASSWORD: a role with a password is
    /// already set whether or not one was supplied.
    #[test]
    fn a_role_with_a_password_keeps_it() {
        for password in [Some(PW), None] {
            let db = Scripted::new(&[Ok(SET)]);
            assert_eq!(db.decide(password), Outcome::AlreadySet, "{password:?}");
            assert_eq!(*db.sent.borrow(), [state_sql("cloud_admin")]);
        }
    }

    /// The LSN is read by a statement of its own, after the `ALTER ROLE`'s has
    /// returned: sent in one string they would share a transaction, and the
    /// position would precede the commit.
    #[test]
    fn an_applied_password_reports_the_lsn_read_after_its_commit() {
        let db = Scripted::new(&[Ok(NONE), Ok(""), Ok("0/16B3748")]);
        assert_eq!(
            db.decide(Some(PW)),
            Outcome::Applied {
                lsn: lsn("0/16B3748")
            }
        );
        assert_eq!(
            *db.sent.borrow(),
            [
                state_sql("cloud_admin"),
                alter_role_sql("cloud_admin", PW),
                FLUSH_LSN_SQL.to_string()
            ]
        );
    }

    #[test]
    fn it_waits_for_postgres_to_answer_before_deciding() {
        let db = Scripted::new(&[
            Err("the database system is starting up"),
            Err("the database system is starting up"),
            Ok(NONE),
            Ok(""),
            Ok("1/0"),
        ]);
        assert_eq!(db.decide(Some(PW)), Outcome::Applied { lsn: lsn("1/0") });
    }

    /// The password is in; only the LSN is missing. Still "applied", so the
    /// controller can tell this from a password that never landed.
    #[test]
    fn an_unreadable_lsn_is_applied_without_one() {
        for answer in [Err("gone"), Ok("garbage")] {
            let db = Scripted::new(&[Ok(NONE), Ok(""), answer]);
            assert_eq!(
                db.decide(Some(PW)),
                Outcome::Applied { lsn: None },
                "{answer:?}"
            );
            assert_eq!(db.sent.borrow().len(), 3, "the LSN is read once");
        }
    }

    #[test]
    fn a_compute_that_never_answers_is_failed() {
        let db = Scripted::new(&[]);
        assert_eq!(db.decide(Some(PW)), Outcome::Failed);
        assert_eq!(db.sent.borrow().len(), 5);
    }

    #[test]
    fn a_refused_alter_role_is_failed() {
        let db = Scripted::new(&[Ok(NONE), Err("ERROR: permission denied"), Ok(NONE)]);
        assert_eq!(db.decide(Some(PW)), Outcome::Failed);
    }

    /// The `ALTER ROLE` committed but its reply was lost: the next pass finds a
    /// password and, like any other, reports it already set.
    #[test]
    fn a_password_found_after_a_lost_reply_is_already_set() {
        let db = Scripted::new(&[
            Ok(NONE),
            Err("server closed the connection unexpectedly"),
            Ok(SET),
        ]);
        assert_eq!(db.decide(Some(PW)), Outcome::AlreadySet);
    }

    #[test]
    fn an_error_is_scrubbed_of_the_statement_and_the_password() {
        let err = format!(
            "ERROR:  bad thing near '{PW}'\nLINE 1: ALTER ROLE cloud_admin WITH PASSWORD '{PW}'\n                                            ^"
        );
        let got = scrub(&err, PW);
        assert!(!got.contains(PW), "{got}");
        assert!(!got.contains("LINE"), "{got}");
        assert!(got.starts_with("ERROR:  bad thing near"), "{got}");
    }

    /// What every path logs, captured, must not carry the password — including
    /// a failure whose psql error echoes the statement.
    #[test]
    fn the_password_never_reaches_the_log() {
        #[derive(Clone, Default)]
        struct Buf(Arc<Mutex<Vec<u8>>>);
        impl std::io::Write for Buf {
            fn write(&mut self, b: &[u8]) -> std::io::Result<usize> {
                self.0.lock().unwrap().extend_from_slice(b);
                Ok(b.len())
            }
            fn flush(&mut self) -> std::io::Result<()> {
                Ok(())
            }
        }

        let echo: &'static str = Box::leak(
            format!("ERROR:  oops\nLINE 1: ALTER ROLE cloud_admin WITH PASSWORD '{PW}'")
                .into_boxed_str(),
        );
        let buf = Buf::default();
        let writer = buf.clone();
        let subscriber = tracing_subscriber::fmt()
            .with_writer(move || writer.clone())
            .with_ansi(false)
            .finish();
        tracing::subscriber::with_default(subscriber, || {
            for script in [
                vec![Ok(NONE), Ok(""), Ok("0/1")],
                vec![Ok(NONE), Ok(""), Err("x")],
                vec![Ok(SET)],
                // Every pass refused, so the last error logged is the echo.
                [Ok(NONE), Err(echo)].repeat(5),
            ] {
                Scripted::new(&script).decide(Some(PW));
            }
        });
        let logged = String::from_utf8(buf.0.lock().unwrap().clone()).unwrap();
        assert!(
            logged.contains(READY_MARKER),
            "nothing was captured: {logged}"
        );
        assert!(
            logged.contains("oops"),
            "the failure was not logged: {logged}"
        );
        assert!(!logged.contains(PW), "the password was logged: {logged}");
        assert!(
            !logged.contains("c2VydmVy"),
            "part of it was logged: {logged}"
        );
    }

    #[test]
    fn the_status_is_pending_until_a_run_finishes() {
        let s = Status::default();
        assert_eq!(s.get(), Outcome::Pending);
        let run = s.begin();
        assert_eq!(s.get(), Outcome::Pending);
        s.finish(run, Outcome::Unset);
        assert_eq!(s.get(), Outcome::Unset);
    }

    /// A lost lease does not stop its run's thread; when that thread finishes
    /// late it must not overwrite the answer of the lease serving now.
    #[test]
    fn a_superseded_run_records_nothing() {
        let s = Status::default();
        let old = s.begin();
        let new = s.begin();
        s.finish(new, Outcome::AlreadySet);
        s.finish(old, Outcome::Failed);
        assert_eq!(s.get(), Outcome::AlreadySet);
    }

    /// The old lease's thread applied the password after the new lease began,
    /// so the new run finds it and says `AlreadySet`. Whichever finishes first,
    /// the answer is the applied one with its LSN.
    #[test]
    fn a_superseded_run_still_records_applied() {
        let applied = Outcome::Applied { lsn: lsn("0/40") };
        for old_first in [true, false] {
            let s = Status::default();
            let old = s.begin();
            let new = s.begin();
            if old_first {
                s.finish(old, applied);
                s.finish(new, Outcome::AlreadySet);
            } else {
                s.finish(new, Outcome::AlreadySet);
                s.finish(old, applied);
            }
            assert_eq!(s.get(), applied, "old run finished first: {old_first}");
        }
    }

    /// A new run resets a settled answer to pending, so a caller polling
    /// across a lease change waits for the run that is actually deciding.
    #[test]
    fn a_new_run_starts_pending() {
        let s = Status::default();
        let run = s.begin();
        s.finish(run, Outcome::Failed);
        s.begin();
        assert_eq!(s.get(), Outcome::Pending);
    }

    /// The next lease finds the password this process applied and would say
    /// `AlreadySet`; the LSN the controller needs survives that.
    #[test]
    fn applied_is_never_replaced() {
        let s = Status::default();
        let run = s.begin();
        let applied = Outcome::Applied { lsn: lsn("0/30") };
        s.finish(run, applied);
        let next = s.begin();
        assert_eq!(s.get(), applied);
        s.finish(next, Outcome::AlreadySet);
        assert_eq!(s.get(), applied);
    }

    #[test]
    fn outcomes_have_their_wire_names() {
        for (o, name) in [
            (Outcome::Pending, "pending"),
            (Outcome::Applied { lsn: None }, "applied"),
            (Outcome::AlreadySet, "already_set"),
            (Outcome::Unset, "unset"),
            (Outcome::Failed, "failed"),
        ] {
            assert_eq!(o.name(), name);
        }
    }

    /// Against a real Postgres, because the SQL is the part a unit test cannot
    /// check. It rewrites roles, so it wants a scratch cluster whose bootstrap
    /// superuser is `cloud_admin`, reachable over `127.0.0.1` with `trust`:
    ///
    /// ```text
    /// initdb -D $D -U cloud_admin --auth=trust
    /// pg_ctl -D $D -o "-p 55999 -k $D -c listen_addresses=127.0.0.1" start
    /// AMP_TEST_PG_BIN=<pg>/bin AMP_TEST_PG_PORT=55999 \
    ///   cargo test -p endpoint_supervisor -- --ignored customer_role_against
    /// ```
    #[test]
    #[ignore = "needs a scratch Postgres; see the doc comment"]
    fn customer_role_against_a_real_postgres() {
        let bin = camino::Utf8PathBuf::from(std::env::var("AMP_TEST_PG_BIN").unwrap());
        let psql_bin = bin.join("psql");
        let port: u16 = std::env::var("AMP_TEST_PG_PORT").unwrap().parse().unwrap();
        let db = Psql {
            bin: &psql_bin,
            port,
        };
        let q = |sql: &str| db.run(sql).unwrap_or_else(|e| panic!("{sql}: {e}"));
        let as_customer = |sql: &str| {
            let out = std::process::Command::new(&psql_bin)
                .args([
                    "-h",
                    "127.0.0.1",
                    "-p",
                    &port.to_string(),
                    "-U",
                    "amp_admin",
                ])
                .args([
                    "-d",
                    "postgres",
                    "-tA",
                    "-v",
                    "ON_ERROR_STOP=1",
                    "--no-psqlrc",
                ])
                .args(["-c", sql])
                .output()
                .unwrap();
            if out.status.success() {
                Ok(String::from_utf8_lossy(&out.stdout).trim().to_string())
            } else {
                Err(String::from_utf8_lossy(&out.stderr).trim().to_string())
            }
        };

        // An endpoint whose clients have been logging in as cloud_admin — and,
        // so the test can run again on one cluster, no customer password yet.
        q(
            "DO $$ BEGIN IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'amp_admin') THEN \
           ALTER ROLE amp_admin WITH PASSWORD NULL; END IF; END $$",
        );
        q("ALTER ROLE cloud_admin WITH PASSWORD 'existing-secret'");
        let verifier = q("SELECT rolpassword FROM pg_authid WHERE rolname = 'cloud_admin'");
        assert!(verifier.starts_with("SCRAM-SHA-256$"), "{verifier}");

        // A role the customer made themselves under the configured name is not
        // taken over: nothing changes, and cloud_admin keeps its password.
        q("DROP ROLE IF EXISTS amp_taken; CREATE ROLE amp_taken LOGIN PASSWORD 'theirs'");
        let taken = CustomerRole::new("amp_taken", "yb_amp_superuser").unwrap();
        assert_eq!(
            configure_remote_auth(&db, None, Some(&taken), ATTEMPTS, INTERVAL),
            Outcome::Failed
        );
        assert_eq!(
            q("SELECT rolpassword FROM pg_authid WHERE rolname = 'cloud_admin'"),
            verifier
        );
        assert_eq!(
            q(
                "SELECT count(*) FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.member \
               WHERE r.rolname = 'amp_taken'"
            ),
            "0"
        );

        let role = customer();
        // Twice: every boot runs it. The first hands the password over; the
        // second finds it there.
        for _ in 0..2 {
            assert_eq!(
                configure_remote_auth(&db, None, Some(&role), ATTEMPTS, INTERVAL),
                Outcome::AlreadySet
            );
            assert_eq!(
                q(
                    "SELECT rolsuper, rolcanlogin, rolcreaterole, rolcreatedb, rolbypassrls, \
                   rolreplication FROM pg_roles WHERE rolname = 'amp_admin'"
                ),
                "f|t|t|t|t|t"
            );
            assert_eq!(
                q("SELECT pg_has_role('amp_admin', 'yb_amp_superuser', 'member')"),
                "t"
            );
            assert_eq!(
                q("SELECT rolpassword FROM pg_authid WHERE rolname = 'amp_admin'"),
                verifier,
                "the customer takes over cloud_admin's password verbatim"
            );
            assert_eq!(
                q("SELECT rolpassword IS NULL FROM pg_authid WHERE rolname = 'cloud_admin'"),
                "t"
            );
            assert_eq!(q("SELECT id FROM yb_amp_migration.migration_id"), "12");
        }

        // AMP-15 step 1: the customer cannot reach the machine.
        let denied = as_customer("COPY (SELECT 1) TO PROGRAM 'id'").unwrap_err();
        assert!(
            denied.contains("permission denied") || denied.contains("must be superuser"),
            "{denied}"
        );
        let denied = as_customer("SELECT pg_read_file('/etc/hostname')").unwrap_err();
        assert!(denied.contains("permission denied"), "{denied}");
        let denied = as_customer("ALTER SYSTEM SET work_mem = '1GB'").unwrap_err();
        assert!(denied.contains("permission denied"), "{denied}");

        // ...and can still administer its own data.
        as_customer(
            "DROP TABLE IF EXISTS amp15_t; CREATE TABLE amp15_t (x int); \
             INSERT INTO amp15_t VALUES (1)",
        )
        .unwrap();
        as_customer("CREATE SCHEMA IF NOT EXISTS amp15_s").unwrap();
        as_customer("DROP ROLE IF EXISTS amp15_r; CREATE ROLE amp15_r LOGIN").unwrap();
        as_customer("SELECT count(*) FROM pg_stat_activity").unwrap();
        // Data cloud_admin owns — what an endpoint from before the customer role
        // is full of — through pg_read_all_data / pg_write_all_data.
        q("DROP TABLE IF EXISTS amp15_admin; CREATE TABLE amp15_admin (x int)");
        as_customer("INSERT INTO amp15_admin VALUES (1); SELECT * FROM amp15_admin").unwrap();
        // pg_read_all_data reaches pg_authid too — Neon's model has the same
        // property — so what keeps the superuser out of reach is that its row
        // holds no password to take.
        assert_eq!(
            as_customer("SELECT rolpassword IS NULL FROM pg_authid WHERE rolname = 'cloud_admin'")
                .unwrap(),
            "t"
        );
    }
}
