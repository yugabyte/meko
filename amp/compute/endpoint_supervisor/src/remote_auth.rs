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

/// Decides what the role's password should be and applies it if anything is to
/// be done, retrying until Postgres answers. Failure is a warning: the endpoint
/// is otherwise up, and refusing to supervise it because remote auth could not
/// be configured would turn a degraded endpoint into no endpoint.
fn configure_remote_auth(
    db: &impl Sql,
    password: Option<&str>,
    attempts: usize,
    interval: Duration,
) -> Outcome {
    let mut last = String::new();
    for attempt in 1..=attempts {
        if attempt > 1 {
            std::thread::sleep(interval);
        }
        // Also the wait for Postgres to answer queries at all, so nothing is
        // decided against a compute still restoring its basebackup.
        let has_password = match db.run(HAS_PASSWORD_SQL) {
            Ok(answer) => answer == "t",
            Err(e) => {
                last = e;
                continue;
            }
        };
        match (has_password, password) {
            (true, _) => {
                tracing::info!("{READY_MARKER}: skipped, cloud_admin already has a password");
                return Outcome::AlreadySet;
            }
            (false, None) => {
                tracing::warn!(
                    "{READY_MARKER}: skipped, COMPUTE_PASSWORD is unset and cloud_admin has no \
                     password, so remote auth will fail"
                );
                return Outcome::Unset;
            }
            (false, Some(pw)) => match db.run(&alter_role_sql(pw)) {
                Ok(_) => {
                    // The controller is waiting for exactly this.
                    tracing::info!("{READY_MARKER}");
                    return Outcome::Applied { lsn: flush_lsn(db) };
                }
                Err(e) => last = scrub(&e, pw),
            },
        }
    }
    tracing::warn!(
        "could not set the cloud_admin password, so remote auth will fail: {}",
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
pub fn spawn(psql_bin: &Utf8Path, port: u16, password: Option<&str>, status: Arc<Status>) {
    let (psql_bin, password) = (psql_bin.to_owned(), password.map(str::to_owned));
    let run = status.begin();
    std::thread::spawn(move || {
        let db = Psql {
            bin: &psql_bin,
            port,
        };
        let outcome = configure_remote_auth(&db, password.as_deref(), ATTEMPTS, INTERVAL);
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

    const PW: &str = "SCRAM-SHA-256$4096:c2FsdA==$c3RvcmVk:c2VydmVy";

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
            configure_remote_auth(self, password, 5, Duration::ZERO)
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
        let db = Scripted::new(&[Ok("f")]);
        assert_eq!(db.decide(None), Outcome::Unset);
        assert_eq!(*db.sent.borrow(), [HAS_PASSWORD_SQL]);
    }

    /// The catalog decides, not COMPUTE_PASSWORD: a role with a password is
    /// already set whether or not one was supplied.
    #[test]
    fn a_role_with_a_password_keeps_it() {
        for password in [Some(PW), None] {
            let db = Scripted::new(&[Ok("t")]);
            assert_eq!(db.decide(password), Outcome::AlreadySet, "{password:?}");
            assert_eq!(*db.sent.borrow(), [HAS_PASSWORD_SQL]);
        }
    }

    /// The LSN is read by a statement of its own, after the `ALTER ROLE`'s has
    /// returned: sent in one string they would share a transaction, and the
    /// position would precede the commit.
    #[test]
    fn an_applied_password_reports_the_lsn_read_after_its_commit() {
        let db = Scripted::new(&[Ok("f"), Ok(""), Ok("0/16B3748")]);
        assert_eq!(
            db.decide(Some(PW)),
            Outcome::Applied {
                lsn: lsn("0/16B3748")
            }
        );
        assert_eq!(
            *db.sent.borrow(),
            [
                HAS_PASSWORD_SQL.to_string(),
                alter_role_sql(PW),
                FLUSH_LSN_SQL.to_string()
            ]
        );
    }

    #[test]
    fn it_waits_for_postgres_to_answer_before_deciding() {
        let db = Scripted::new(&[
            Err("the database system is starting up"),
            Err("the database system is starting up"),
            Ok("f"),
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
            let db = Scripted::new(&[Ok("f"), Ok(""), answer]);
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
        let db = Scripted::new(&[Ok("f"), Err("ERROR: permission denied"), Ok("f")]);
        assert_eq!(db.decide(Some(PW)), Outcome::Failed);
    }

    /// The `ALTER ROLE` committed but its reply was lost: the next pass finds a
    /// password and, like any other, reports it already set.
    #[test]
    fn a_password_found_after_a_lost_reply_is_already_set() {
        let db = Scripted::new(&[
            Ok("f"),
            Err("server closed the connection unexpectedly"),
            Ok("t"),
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
                vec![Ok("f"), Ok(""), Ok("0/1")],
                vec![Ok("f"), Ok(""), Err("x")],
                vec![Ok("t")],
                // Every pass refused, so the last error logged is the echo.
                [Ok("f"), Err(echo)].repeat(5),
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
}
