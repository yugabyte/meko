//! Whether Postgres will accept the operator's settings, asked before they are
//! written.
//!
//! A `pg_settings` row can hold any name and any value. Neither is merely
//! ignored if Postgres dislikes it:
//!
//! | in `postgresql.conf`         | on reload                          | on the next start |
//! |------------------------------|------------------------------------|-------------------|
//! | a name it does not recognise | the **whole file** is discarded    | `FATAL`           |
//! | a value it cannot parse      | other settings still apply         | `FATAL`           |
//!
//! Both end the same way: the compute does not come up. That start may be days
//! later — a pod bounce, a node drain, the next `postmaster` setting — so the
//! endpoint runs on a config that works until something restarts it, and the
//! crash looks unrelated to the settings write that caused it. Postgres has no
//! option to ignore a parameter it dislikes, so the only way not to be broken by
//! one is not to write it.
//!
//! **The oracle is the binary, not a running server.** `postgres -D <dir> -C
//! <any known name>` parses `<dir>/postgresql.conf` and exits non-zero if
//! anything in it is unacceptable. It needs no port, no server and no real data
//! directory — which is the whole point, because the case that must be caught is
//! the one with no server to ask: an endpoint being created renders its config
//! before Postgres has ever run, and a bad setting there is what leaves it
//! unable to start at all.
//!
//! **One probe, not a whitelist.** Writing the settings into a throwaway config
//! and parsing it is exactly what Postgres will do for real, so it answers every
//! case at once — a typo, an unparseable value, an `internal` setting that
//! cannot be set from a file, and the two that must be *allowed*: a dotted
//! custom GUC (`amp_probe.x`) and a setting belonging to an extension that is
//! not loaded yet (`pg_stat_statements.max`), both of which Postgres accepts as
//! placeholders. `postgres --describe-config` looks like the cheaper answer and
//! is the wrong one: it omits settable GUCs (`post_auth_delay`,
//! `ignore_system_indexes`), so as a whitelist it would silently drop an
//! operator's valid setting, and it says nothing about values.

use std::collections::BTreeMap;
use std::process::Command;

use anyhow::{Context, Result};
use camino::{Utf8Path, Utf8PathBuf};

/// A name `-C` is asked for, only to make Postgres parse the file on the way.
/// Any name it always knows would do.
const ANY_KNOWN_SETTING: &str = "data_directory";

/// One setting that is stored and not in force, and why.
///
/// The reason is a fixed phrase chosen from the probe's outcome, not Postgres's
/// own words passed through: the caller gets something stable to show a person,
/// and a wording change upstream cannot turn into a changed API. The raw message
/// goes to the log, where detail beats stability.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct NotApplied {
    pub name: String,
    pub reason: String,
}

/// Reasons, as the caller sees them.
pub const UNRECOGNIZED: &str = "unrecognized parameter";
pub const INVALID_VALUE: &str = "invalid value";
pub const NOT_SETTABLE: &str = "cannot be set in a configuration file";
pub const REFUSED: &str = "refused by Postgres";
/// Stored, but this process did not write the file it belongs in: the caller
/// brought the daemon config, so a daemon override is theirs to render.
pub const CALLERS_DAEMON_CONFIG: &str = "the caller renders this daemon's config";
/// Not refused, just inert: Postgres knows it and nothing can change it.
pub const NOTHING_APPLIES: &str = "nothing applies this setting";
/// Stored, but the compute's server certificate is rendered after the operator's
/// settings and wins. Named rather than silently overridden, because the value
/// parses and would otherwise be accepted and do nothing — see
/// [`crate::spec::COMPUTE_TLS_OWNED`].
pub const COMPUTE_TLS_OWNED: &str = "the compute's server certificate owns this setting";

/// Which of the above a probe failure amounts to.
fn reason_for(message: &str) -> &'static str {
    if message.contains("unrecognized configuration parameter") {
        UNRECOGNIZED
    } else if message.contains("invalid value") {
        INVALID_VALUE
    } else if message.contains("cannot be changed") {
        NOT_SETTABLE
    } else {
        REFUSED
    }
}

/// What a render may write, and what it must leave out.
#[derive(Debug, Default, PartialEq, Eq)]
pub struct Screened {
    /// Settings Postgres accepts.
    pub keep: BTreeMap<String, String>,
    /// Settings it does not: stored, not rendered, not in force. Naming them is
    /// the only way the operator learns the edit did nothing.
    pub refused: Vec<NotApplied>,
}

/// Split `settings` into what Postgres will take and what it will not.
///
/// One probe when everything is fine, which is the common case. Only when the
/// set is rejected is each setting probed on its own, because that is what says
/// *which* one is at fault — cheaper than parsing an error message, and immune
/// to how that message is worded.
pub fn screen(
    pg_bin: &Utf8Path,
    work_dir: &Utf8Path,
    settings: &BTreeMap<String, String>,
) -> Screened {
    let mut out = Screened::default();
    if settings.is_empty() {
        return out;
    }
    // A probe that cannot be run leaves everything in place. Refusing an
    // operator's settings because the check itself broke would turn a missing
    // binary into a silently under-configured database, and unchecked is what
    // these were before this existed.
    let probe = match probe_dir(work_dir) {
        Ok(d) => d,
        Err(e) => {
            tracing::warn!("cannot prepare a settings probe, rendering unchecked: {e:#}");
            out.keep = settings.clone();
            return out;
        }
    };
    let postgres = pg_bin.join("postgres");

    // Best effort: a probe directory left behind is inert, and failing a settings
    // render because a temp directory would not delete would be worse.
    let _cleanup = Cleanup(probe.clone());

    if accepts(&postgres, &probe, settings) {
        out.keep = settings.clone();
        return out;
    }
    // Each on its own, which is the bisect: with three bad settings the operator
    // hears about all three rather than fixing one and coming back for the next.
    for (name, value) in settings {
        let one = BTreeMap::from([(name.clone(), value.clone())]);
        match verdict(&postgres, &probe, &one) {
            Ok(()) => {
                out.keep.insert(name.clone(), value.clone());
            }
            Err(why) => {
                tracing::warn!(
                    name,
                    value,
                    postgres = %why,
                    "Postgres will not accept this setting; not rendering it"
                );
                out.refused.push(NotApplied {
                    name: name.clone(),
                    reason: reason_for(&why).to_string(),
                });
            }
        }
    }
    // Individually fine and jointly refused is a conflict between two settings
    // rather than a bad one, and nothing here can say which to drop. Keeping
    // them is the lesser harm: this leaves the endpoint exactly as it was before
    // this check existed, where blanking an operator's configuration on a guess
    // would be a new way to break it.
    if !out.keep.is_empty() && !accepts(&postgres, &probe, &out.keep) {
        tracing::error!(
            settings = ?out.keep.keys().collect::<Vec<_>>(),
            "Postgres refuses these settings together though each is valid alone; \
             rendering them anyway — the compute may fail to start"
        );
    }
    out
}

/// A directory holding just the settings under test, private to one call.
///
/// Ours rather than the compute's PGDATA: at create there is no PGDATA yet, and
/// pointing a second `postgres` at a live one is not worth the doubt.
///
/// **Private, not shared.** A fixed path would be written by every caller, and
/// there are concurrent ones — the settings API answers checks on a thread pool
/// while a reload renders on another. Two probes sharing a file interleave as
/// write, write, read, read: one `postgres` then judges the other's settings, and
/// a verdict of "acceptable" for a set that was never tested is how a fatal
/// setting gets written. That is the failure this module exists to prevent, so it
/// must not be reintroduced by the mechanism preventing it.
fn probe_dir(work_dir: &Utf8Path) -> Result<Utf8PathBuf> {
    static NEXT: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
    let unique = NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
    // Process id as well as the counter: a restarted supervisor reuses run_dir,
    // and a directory left behind by a killed one must not be mistaken for ours.
    let dir = work_dir
        .join("settings-probe")
        .join(format!("{}-{unique}", std::process::id()));
    std::fs::create_dir_all(&dir).with_context(|| format!("create {dir}"))?;
    Ok(dir)
}

/// Whether Postgres parses a config file made of exactly `settings`.
///
/// Values are escaped exactly as `compute_ctl` escapes them
/// (`compute_tools::pg_helpers::escape_conf_value`: double the quotes, double the
/// backslashes, wrap in single quotes). It has to be byte-identical or the probe
/// answers about a different file than the one that gets written — and a value
/// ending in a backslash is the case that bites, because `'foo\'` escapes its own
/// closing quote and turns a valid setting into a refusal.
fn accepts(postgres: &Utf8Path, probe: &Utf8Path, settings: &BTreeMap<String, String>) -> bool {
    verdict(postgres, probe, settings).is_ok()
}

/// [`accepts`], keeping what Postgres said when it refused.
///
/// The message is for the log, never for a decision: what to do is settled by
/// the exit status, so nothing here depends on how the wording might change.
fn verdict(
    postgres: &Utf8Path,
    probe: &Utf8Path,
    settings: &BTreeMap<String, String>,
) -> Result<(), String> {
    let body: String = settings
        .iter()
        .map(|(k, v)| {
            let escaped = v.replace('\'', "''").replace('\\', "\\\\");
            format!("{k} = '{escaped}'\n")
        })
        .collect();
    let conf = probe.join("postgresql.conf");
    if let Err(e) = std::fs::write(&conf, body) {
        tracing::warn!("cannot write the settings probe, treating them as acceptable: {e:#}");
        return Ok(());
    }
    match Command::new(postgres)
        .args(["-D", probe.as_str(), "-C", ANY_KNOWN_SETTING])
        .output()
    {
        Ok(out) if out.status.success() => Ok(()),
        Ok(out) => {
            let text = String::from_utf8_lossy(&out.stderr);
            let why = text
                .lines()
                .find(|l| {
                    l.contains("unrecognized configuration parameter")
                        || l.contains("invalid value")
                        || l.contains("cannot be changed")
                })
                .or_else(|| text.lines().last())
                .unwrap_or("no output")
                .trim()
                .to_string();
            Err(why)
        }
        Err(e) => {
            tracing::warn!("could not run the settings probe, treating them as acceptable: {e:#}");
            Ok(())
        }
    }
}

/// Removes a probe directory when the call that made it returns.
struct Cleanup(Utf8PathBuf);

impl Drop for Cleanup {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn set(pairs: &[(&str, &str)]) -> BTreeMap<String, String> {
        pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect()
    }

    fn tmp(name: &str) -> Utf8PathBuf {
        let dir = Utf8PathBuf::try_from(std::env::temp_dir())
            .expect("temp dir is utf8")
            .join(format!("endpoint-supervisor-{name}"));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("temp dir");
        dir
    }

    #[test]
    fn nothing_stored_probes_nothing() {
        let got = screen(
            Utf8Path::new("/nonexistent/bin"),
            &tmp("guc-empty"),
            &BTreeMap::new(),
        );
        assert_eq!(got, Screened::default());
    }

    /// The check must fail open. If it could not run, the settings are rendered
    /// as they were before it existed — a supervisor that blanked an operator's
    /// configuration because it could not find its own Postgres would be a worse
    /// failure than the one this prevents.
    #[test]
    fn a_postgres_it_cannot_run_keeps_every_setting() {
        let settings = set(&[("work_mem", "64MB"), ("total_nonsense", "1")]);
        let got = screen(
            Utf8Path::new("/nonexistent/bin"),
            &tmp("guc-nopg"),
            &settings,
        );
        assert_eq!(got.keep, settings);
        assert!(got.refused.is_empty());
    }

    /// Needs the Postgres the compute runs. Absent in the plain unit-test job,
    /// present wherever the debug tree has been built — which is the job that runs
    /// against a real cluster, and any developer's tree.
    mod live {
        use super::*;

        fn pg_bin() -> Option<Utf8PathBuf> {
            let dir: Utf8PathBuf = match std::env::var("PG_TEST_BIN") {
                Ok(v) => v.into(),
                // From this file to the workspace's built Postgres.
                Err(_) => Utf8PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                    .join("../../pg_install-debug/v18/bin"),
            };
            if dir.join("postgres").exists() {
                Some(dir)
            } else {
                eprintln!("SKIP: no postgres binary at {dir} (set PG_TEST_BIN)");
                None
            }
        }

        /// The bug this exists for: one bad name would take the whole config file
        /// down, so the good setting beside it has to survive the screening.
        #[test]
        fn a_name_postgres_does_not_know_is_refused_and_the_rest_kept() {
            let Some(bin) = pg_bin() else { return };
            let got = screen(
                &bin,
                &tmp("guc-typo"),
                &set(&[("work_mem", "64MB"), ("work_memm", "5MB")]),
            );
            assert_eq!(
                got.refused,
                vec![NotApplied {
                    name: "work_memm".into(),
                    reason: UNRECOGNIZED.into()
                }]
            );
            assert_eq!(got.keep, set(&[("work_mem", "64MB")]));
        }

        /// A value Postgres cannot parse is as fatal to the next start as a bad
        /// name, so the name being real is not enough to render it.
        #[test]
        fn a_value_postgres_cannot_parse_is_refused() {
            let Some(bin) = pg_bin() else { return };
            let got = screen(
                &bin,
                &tmp("guc-badvalue"),
                &set(&[("work_mem", "banana"), ("maintenance_work_mem", "128MB")]),
            );
            assert_eq!(
                got.refused,
                vec![NotApplied {
                    name: "work_mem".into(),
                    reason: INVALID_VALUE.into()
                }]
            );
            assert_eq!(got.keep, set(&[("maintenance_work_mem", "128MB")]));
        }

        /// A backslash must be escaped the way `compute_ctl` escapes it, or the
        /// probe judges different bytes than get written. A trailing one is the
        /// case that bites: unescaped, `'foo\'` escapes its own closing quote and
        /// a perfectly good setting comes back refused.
        #[test]
        fn a_backslash_is_escaped_the_way_compute_ctl_writes_it() {
            let Some(bin) = pg_bin() else { return };
            let settings = set(&[
                ("search_path", r"public, a\b"),
                ("archive_command", r"cp %p /wal/%f\"),
            ]);
            let got = screen(&bin, &tmp("guc-backslash"), &settings);
            assert!(
                got.refused.is_empty(),
                "a backslash in a value was refused: {:?}",
                got.refused
            );
            assert_eq!(got.keep, settings);
        }

        /// The probe writes a config file and then reads it back with `postgres`.
        /// Concurrent calls sharing one file interleave as write, write, read,
        /// read — and then a verdict describes settings its caller never sent,
        /// which is how a fatal setting gets judged acceptable and written. The
        /// settings API answers checks on a thread pool while a reload renders, so
        /// this is a real schedule, not a theoretical one.
        #[test]
        fn concurrent_probes_do_not_read_each_other_s_file() {
            let Some(bin) = pg_bin() else { return };
            let dir = tmp("guc-concurrent");
            let mut handles = Vec::new();
            for i in 0..8 {
                let bin = bin.clone();
                let dir = dir.clone();
                handles.push(std::thread::spawn(move || {
                    // Half ask about a good setting, half about a typo, so a
                    // crossed verdict shows up as the wrong answer either way.
                    let settings = if i % 2 == 0 {
                        set(&[("work_mem", "64MB")])
                    } else {
                        set(&[("work_memm", "5MB")])
                    };
                    let got = screen(&bin, &dir, &settings);
                    (i, got)
                }));
            }
            for h in handles {
                let (i, got) = h.join().expect("probe thread");
                if i % 2 == 0 {
                    assert!(
                        got.refused.is_empty(),
                        "thread {i} asked about work_mem and was told {:?}",
                        got.refused
                    );
                } else {
                    assert_eq!(
                        got.refused.len(),
                        1,
                        "thread {i} asked about work_memm and was told {:?}",
                        got.refused
                    );
                    assert_eq!(got.refused[0].name, "work_memm");
                }
            }
        }

        /// Several bad settings at once, which is the case an operator actually
        /// hits after a careless edit. Each is named, so the fix is one edit
        /// rather than a round trip per bad setting — and the good ones beside
        /// them are still applied instead of being held hostage.
        #[test]
        fn every_bad_setting_is_named_not_just_the_first() {
            let Some(bin) = pg_bin() else { return };
            let got = screen(
                &bin,
                &tmp("guc-many"),
                &set(&[
                    ("work_memm", "5MB"),              // typo
                    ("work_mem", "banana"),            // real name, unusable value
                    ("block_size", "8192"),            // compiled in
                    ("maintenance_work_mem", "128MB"), // fine
                    ("amp_probe.custom", "1"),         // fine, a placeholder
                ]),
            );
            // Named, and each with the reason that fits it — a typo and an
            // unusable value are different mistakes to go and fix.
            assert_eq!(
                got.refused,
                vec![
                    NotApplied {
                        name: "block_size".into(),
                        reason: NOT_SETTABLE.into()
                    },
                    NotApplied {
                        name: "work_mem".into(),
                        reason: INVALID_VALUE.into()
                    },
                    NotApplied {
                        name: "work_memm".into(),
                        reason: UNRECOGNIZED.into()
                    },
                ],
                "every refused setting must be named, with why"
            );
            assert_eq!(
                got.keep,
                set(&[("amp_probe.custom", "1"), ("maintenance_work_mem", "128MB"),]),
                "the valid settings must still be applied"
            );
        }

        /// Dotted names must pass. A custom GUC and a setting belonging to an
        /// extension that is not loaded are both placeholders Postgres accepts,
        /// and refusing them would break configuring extensions at all.
        #[test]
        fn dotted_settings_are_kept() {
            let Some(bin) = pg_bin() else { return };
            let settings = set(&[
                ("amp_probe.nothing_applies_this", "1"),
                ("pg_stat_statements.max", "5000"),
            ]);
            let got = screen(&bin, &tmp("guc-dotted"), &settings);
            assert!(got.refused.is_empty(), "refused {:?}", got.refused);
            assert_eq!(got.keep, settings);
        }

        /// Compiled into the binary, so it cannot be set from a file at all.
        /// Postgres only logs it rather than refusing to start, but it never
        /// takes effect, and saying so beats writing it and staying quiet.
        #[test]
        fn a_compiled_in_setting_is_refused() {
            let Some(bin) = pg_bin() else { return };
            let got = screen(&bin, &tmp("guc-internal"), &set(&[("block_size", "8192")]));
            assert_eq!(
                got.refused,
                vec![NotApplied {
                    name: "block_size".into(),
                    reason: NOT_SETTABLE.into()
                }]
            );
            assert!(got.keep.is_empty());
        }

        /// A quote in a value must not end the quoted string and turn the rest
        /// into syntax — the escaping is what makes a probe test the file that
        /// will actually be written.
        #[test]
        fn a_quote_in_a_value_is_escaped_not_injected() {
            let Some(bin) = pg_bin() else { return };
            let got = screen(
                &bin,
                &tmp("guc-quote"),
                &set(&[("search_path", "public, o'brien")]),
            );
            assert!(got.refused.is_empty(), "refused {:?}", got.refused);
        }

        /// Everything valid is kept, and the settings are unchanged on the way
        /// through: the screening decides what to render, not what it says.
        #[test]
        fn a_valid_set_passes_through_untouched() {
            let Some(bin) = pg_bin() else { return };
            let settings = set(&[
                ("work_mem", "64MB"),
                ("post_auth_delay", "0"),
                ("log_min_duration_statement", "250ms"),
            ]);
            let got = screen(&bin, &tmp("guc-valid"), &settings);
            assert!(got.refused.is_empty(), "refused {:?}", got.refused);
            assert_eq!(got.keep, settings);
        }
    }
}
