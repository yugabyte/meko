//! Picking up a renewed server certificate without dropping a connection.
//!
//! cert-manager rewrites the Secret in place and the kubelet swaps the volume's
//! `..data` symlink atomically, so the files under `/etc/amp/tls/compute` change
//! underneath a running postmaster. Postgres loads `ssl_cert_file` and
//! `ssl_key_file` at start and at each reload, and only for *new* backends — so
//! nothing in flight is disturbed, but nothing new picks the renewal up either
//! until something asks for a reload.
//!
//! This is that something. The alternative, letting the certificate expire until
//! the pod happens to restart, is a database that stops being connectable to at
//! a time nobody chose.
//!
//! Polling rather than inotify, for the same reason the YB lane's `yb_tls` does
//! it: the path is a symlink into a `..data` directory the kubelet replaces, so a
//! watch on the inode watches the wrong thing after the first swap. Contents
//! rather than mtime, so a rewrite with the same bytes — a resynced but unrenewed
//! Secret — reloads nothing.

use std::time::Duration;

use metrics::{IntGauge, register_int_gauge};
use once_cell::sync::Lazy;

/// `notAfter` of the certificate the compute's Postgres listener presents to
/// clients, as Unix seconds; `0` when none is mounted or it cannot be parsed.
///
/// Read from the file the postmaster is actually configured with, not from what
/// cert-manager believes it issued. Those differ exactly when the renewal has
/// not reached the pod — a Secret written but not propagated into the volume —
/// which is the failure this whole module exists to catch and which no
/// cluster-level metric can see.
pub static COMPUTE_CERT_NOT_AFTER_SECONDS: Lazy<IntGauge> = Lazy::new(|| {
    register_int_gauge!(
        "compute_cert_not_after_seconds",
        "notAfter of the certificate the compute presents to clients, as Unix seconds (0 = none)",
    )
    .expect("register compute_cert_not_after_seconds")
});

/// `notAfter` of the CA that issued it, as Unix seconds; `0` when the issuer
/// ships none.
///
/// cert-manager writes the issuing CA into the leaf Secret's `ca.crt` for a `ca`
/// issuer, so a per-data-plane root is reported by every endpoint that chains to
/// it. An ACME issuer writes no `ca.crt` and this stays `0`, which is correct:
/// there is no private root to expire.
pub static COMPUTE_CA_NOT_AFTER_SECONDS: Lazy<IntGauge> = Lazy::new(|| {
    register_int_gauge!(
        "compute_ca_not_after_seconds",
        "notAfter of the CA that issued the compute's certificate, as Unix seconds (0 = none)",
    )
    .expect("register compute_ca_not_after_seconds")
});

/// `1` while the mounted certificate is not the one the postmaster is serving.
///
/// Confirmed on the wire, not inferred from the signal. `reload_compute` returns
/// once the `SIGHUP` is delivered, and delivery proves nothing: `compute_ctl`
/// ignores a signal whose status is neither `Empty` nor `Running`, a signal that
/// lands before its handler is installed takes the default action, and a Postgres
/// reload whose new pair fails to load keeps the OLD context (`be_tls_init` swaps
/// `SSL_context` only on success). All three look identical to a caller watching
/// the return value.
///
/// So the watcher asks the compute instead — [`confirm_serving`] — and this gauge
/// is `1` from the moment a renewal is seen until a handshake shows the server
/// presenting it. That makes it the one series worth alerting on: the two expiry
/// gauges read the mounted file and report health whether or not anything ever
/// loaded it.
pub static COMPUTE_CERT_RELOAD_PENDING: Lazy<IntGauge> = Lazy::new(|| {
    register_int_gauge!(
        "compute_cert_reload_pending",
        "1 while the mounted certificate is not the one the postmaster is serving",
    )
    .expect("register compute_cert_reload_pending")
});

/// How long to wait for the reload to show up on the wire before giving up and
/// leaving it to the next poll.
///
/// `pg_reload_conf()` is sub-second, so this is not sized against how long a
/// reload takes — it is sized so that reaching the deadline MEANS something.
/// Nothing is waiting on this thread (it sleeps [`POLL`] between rounds anyway),
/// so blocking here costs nothing observable and buys confirmation a minute
/// earlier than the next poll would.
const CONFIRM_DEADLINE: Duration = Duration::from_secs(10);

/// How often to re-ask while waiting.
const CONFIRM_INTERVAL: Duration = Duration::from_millis(250);

/// How often to look. The renewal window is `renewBefore`, days wide, so this is
/// sized against how long a pod may serve an already-replaced certificate rather
/// than against any deadline: a minute of that is invisible, and the poll is two
/// file reads. It is also the retry interval for a reload that failed.
const POLL: Duration = Duration::from_secs(60);

/// One observation of the mount: the bytes a successful reload will have made
/// the postmaster serve.
type Seen = (Option<Vec<u8>>, Option<Vec<u8>>);

/// The contents the compute has LOADED, and the comparison that decides a
/// renewal happened.
///
/// A type rather than two locals in the loop so the decision is testable on its
/// own: what matters here is which file changes count, and pinning that through a
/// sleeping thread makes a timing test out of a logic one.
struct Watched {
    cert_file: String,
    key_file: String,
    /// The pair a handshake last showed the compute serving — not merely what
    /// was last seen on disk. Advanced only by [`Self::loaded`], which is what
    /// makes a failed or dropped reload retry rather than vanish.
    ///
    /// `None` in either half means "could not be read", not "changed to
    /// nothing" — see [`Self::poll`].
    last: Seen,
}

impl Watched {
    fn new(cert_file: &str, key_file: &str) -> Self {
        let last = (read(cert_file), read(key_file));
        let w = Self {
            cert_file: cert_file.to_owned(),
            key_file: key_file.to_owned(),
            last,
        };
        w.publish_expiry();
        COMPUTE_CERT_RELOAD_PENDING.set(0);
        w
    }

    /// Move both gauges to what is on disk right now.
    ///
    /// Called at construction and on every observed change — which is BEFORE any
    /// reload is attempted, so while one is pending or failing these report the
    /// mounted file rather than what the next handshake will use. That is
    /// deliberate: they are the file's expiry, and
    /// [`COMPUTE_CERT_RELOAD_PENDING`] is what says whether the compute is
    /// serving it. Reading either one alone will mislead.
    ///
    /// A file that will not parse sets the gauge to `0` rather than leaving a
    /// stale value standing: a gauge that keeps reporting last week's healthy
    /// expiry is worse than one that reports nothing, because an alert on it
    /// stays quiet.
    fn publish_expiry(&self) {
        COMPUTE_CERT_NOT_AFTER_SECONDS.set(not_after_of(&self.cert_file));
        COMPUTE_CA_NOT_AFTER_SECONDS.set(not_after_of(&ca_file_beside(&self.cert_file)));
    }

    /// Re-read both files and hand back what is there when either has moved away
    /// from what the compute loaded.
    ///
    /// An unreadable file keeps its last contents. Mid-swap the mount is never
    /// half-written — the kubelet's symlink swap is atomic — but the *first* read
    /// can land before cert-manager has issued at all, and treating "absent" as a
    /// change would reload the compute the moment the file appeared, on a
    /// certificate it already started with. A file that goes away is likewise not
    /// a renewal: reloading for it would reload every poll for a mount that never
    /// arrives.
    ///
    /// The expiry gauges move here, because they describe the mounted file.
    /// `last` does not: only [`Self::loaded`] advances it, so a renewal whose
    /// reload failed is still a change on the next poll and is retried until it
    /// lands.
    fn poll(&mut self) -> Option<Seen> {
        let now = (read(&self.cert_file), read(&self.key_file));
        let moved = |old: &Option<Vec<u8>>, new: &Option<Vec<u8>>| match (old, new) {
            (Some(a), Some(b)) => a != b,
            (None, Some(_)) => true,
            _ => false,
        };
        if !(moved(&self.last.0, &now.0) || moved(&self.last.1, &now.1)) {
            return None;
        }
        self.publish_expiry();
        COMPUTE_CERT_RELOAD_PENDING.set(1);
        Some(now)
    }

    /// A handshake showed the compute serving this pair ([`confirm_serving`]).
    ///
    /// Takes the observation the reload was made for rather than re-reading, so
    /// a swap that happened *during* the reload stays a change for the next
    /// poll instead of being silently adopted.
    fn loaded(&mut self, seen: Seen) {
        self.last = seen;
        COMPUTE_CERT_RELOAD_PENDING.set(0);
    }
}

/// The CA bundle cert-manager writes beside the leaf in the same Secret.
/// Derived rather than configured: it is the same mount, and a second
/// environment variable for a file whose name the Secret format fixes would be
/// one more thing to set wrong.
fn ca_file_beside(cert_file: &str) -> String {
    match cert_file.rsplit_once('/') {
        Some((dir, _)) => format!("{dir}/ca.crt"),
        None => "ca.crt".to_string(),
    }
}

/// `notAfter` as Unix seconds, or `0` for a file that is absent, empty or
/// unparseable — all of which mean "nothing to report an expiry for".
fn not_after_of(path: &str) -> i64 {
    match read(path)
        .as_deref()
        .map(ysql_client::describe_certificate_pem)
    {
        Some(Ok(Some(info))) => info.not_after_unix as i64,
        Some(Err(e)) => {
            tracing::warn!("could not read the expiry of `{path}`: {e:#}");
            0
        }
        _ => 0,
    }
}

fn read(path: &str) -> Option<Vec<u8>> {
    std::fs::read(path).ok()
}

/// The DER of the leaf in the mounted PEM — what a handshake would present, so
/// the two are directly comparable.
fn leaf_der(path: &str) -> Option<Vec<u8>> {
    ysql_client::leaf_der_from_pem(&read(path)?)
}

/// Wait until the compute is presenting `expected`, or the deadline passes.
///
/// This is what turns "the signal went out" into "the postmaster has it". The
/// handshake is the only place the loaded certificate is visible — see
/// [`ysql_client::served_certificate_der`] for why SQL cannot answer it.
///
/// Every failure reads the same way and is handled the same way: not yet. A
/// compute that is still starting refuses the connection, one mid-reload presents
/// the old leaf, one that dropped the signal presents the old leaf forever. The
/// caller does not advance the watched pair for any of them, so the next poll
/// signals again — the retry is the absence of a commit, not a special case.
fn confirm_serving(cert_file: &str, compute_port: u16, expected: &[u8]) -> bool {
    let until = std::time::Instant::now() + CONFIRM_DEADLINE;
    // Assigned on every arm that does not return, so the deadline branch always
    // has a reason to report.
    let mut last;
    loop {
        match ysql_client::served_certificate_der("127.0.0.1", compute_port, Duration::from_secs(2))
        {
            Ok(Some(served)) if served == expected => return true,
            Ok(Some(_)) => last = "still presenting the previous certificate".into(),
            Ok(None) => last = "the compute is not offering TLS".into(),
            Err(e) => last = format!("{e:#}"),
        }
        if std::time::Instant::now() >= until {
            tracing::warn!(
                cert_file,
                "the compute has not picked up the renewed certificate within {}s; \
                 will signal again on the next poll: {}",
                CONFIRM_DEADLINE.as_secs(),
                last,
            );
            return false;
        }
        std::thread::sleep(CONFIRM_INTERVAL);
    }
}

/// Log the names the compute is about to present, at startup and after every
/// renewal it loads.
///
/// This is the only place that can say them. Under `mode: issue` the chart
/// derives the SAN list from the Service name and the ingress slot it was
/// handed, and refuses to render an empty one — but under
/// `mode: existingSecret` it mounts a Secret it did not create, typically one
/// `*.<zone>` wildcard, and nothing in the render path compares that
/// certificate's names to the endpoint's published address. A wildcard matches
/// exactly ONE label, so a zone that drifts from the controller's
/// `--ingress-zone-*` leaves every endpoint offering TLS that no client can
/// verify, with nothing failing along the way. A log line is what turns that
/// into something an operator can see.
fn log_served_names(cert_file: &str) {
    let Some(pem) = read(cert_file) else {
        tracing::warn!(
            "no server certificate at `{cert_file}`; the compute offers TLS with nothing to present"
        );
        return;
    };
    match ysql_client::describe_certificate_pem(&pem) {
        Ok(Some(info)) => tracing::info!(
            "the compute's server certificate covers {:?}, notAfter {}",
            info.dns_names,
            info.not_after_unix
        ),
        Ok(None) => tracing::warn!("`{cert_file}` carries no PEM certificate block"),
        Err(e) => tracing::warn!("could not describe `{cert_file}`: {e:#}"),
    }
}

/// Watches the certificate for the life of the process, reloading the compute
/// whenever it changes.
///
/// Once per process rather than once per lease: the certificate belongs to the
/// endpoint, not to whichever lease it is serving under, and a reload between
/// leases is a no-op that says so ([`crate::http::reload_compute`] finds no
/// supervisor published).
pub fn spawn(cert_file: &str, key_file: &str, compute_port: u16) {
    let mut watched = Watched::new(cert_file, key_file);
    log_served_names(cert_file);
    let spawned = std::thread::Builder::new()
        .name("compute-tls".into())
        .spawn(move || {
            loop {
                std::thread::sleep(POLL);
                let Some(seen) = watched.poll() else {
                    continue;
                };
                tracing::info!("the compute's server certificate changed; reloading Postgres");
                // A SIGHUP to `compute_ctl` reaches Postgres as
                // `pg_reload_conf()`, which re-reads both files and keeps every
                // open connection — the whole point of not restarting.
                //
                // Signal, then CONFIRM. Delivery is not application: a SIGHUP
                // is dropped by `compute_ctl` when its status is neither Empty
                // nor Running, kills it outright if it lands before the handler
                // is installed, and a Postgres reload whose new pair fails to
                // load keeps the old context — none of which the return value
                // distinguishes from success.
                //
                // `loaded` is what advances the watched pair, and nothing calls
                // it but a confirmed handshake. So every failure — undeliverable
                // signal, dropped signal, rejected pair, compute still starting
                // — is the same retry on the next poll, and
                // `compute_cert_reload_pending` stays 1 across all of them.
                if let Err(e) = crate::http::reload_compute() {
                    tracing::warn!(
                        "could not signal Postgres for the renewed certificate; \
                         retrying in {}s: {e:#}",
                        POLL.as_secs()
                    );
                    continue;
                }
                // The signal went out; now find out whether it took. Only a
                // confirmed handshake advances the watched pair, so every way
                // this can fail ends up as the same retry.
                let Some(expected) = leaf_der(&watched.cert_file) else {
                    tracing::warn!(
                        "the mounted certificate is not readable as PEM; \
                         cannot confirm the reload"
                    );
                    continue;
                };
                if confirm_serving(&watched.cert_file, compute_port, &expected) {
                    watched.loaded(seen);
                    log_served_names(&watched.cert_file);
                }
            }
        });
    if let Err(e) = spawned {
        tracing::warn!("no certificate watcher: {e:#}");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A self-signed certificate for an endpoint-shaped name.
    fn self_signed_pem() -> String {
        let params = rcgen::CertificateParams::new(vec!["ep-test.example.com".into()]).unwrap();
        let key = rcgen::KeyPair::generate().unwrap();
        params.self_signed(&key).unwrap().pem()
    }

    struct Mount {
        _dir: camino_tempfile::Utf8TempDir,
        cert: camino::Utf8PathBuf,
        key: camino::Utf8PathBuf,
    }

    fn mount() -> (Mount, Watched) {
        let dir = camino_tempfile::tempdir().unwrap();
        let (cert, key) = (dir.path().join("tls.crt"), dir.path().join("tls.key"));
        std::fs::write(&cert, b"leaf-1").unwrap();
        std::fs::write(&key, b"key-1").unwrap();
        let watched = Watched::new(cert.as_str(), key.as_str());
        (
            Mount {
                _dir: dir,
                cert,
                key,
            },
            watched,
        )
    }

    /// The renewal case: cert-manager writes a new leaf and key into the same
    /// paths. One reload for the pair, and none on the polls after the reload
    /// lands — a second would restart nothing but would keep reporting a change
    /// that is over.
    #[test]
    fn a_renewed_certificate_reloads_the_compute_once() {
        let (m, mut w) = mount();
        assert!(w.poll().is_none(), "nothing changed yet");
        std::fs::write(&m.cert, b"leaf-2").unwrap();
        std::fs::write(&m.key, b"key-2").unwrap();
        let seen = w.poll().expect("a renewal must reload the compute");
        w.loaded(seen);
        assert!(
            w.poll().is_none(),
            "the same renewal must not reload twice once it has landed"
        );
    }

    /// A reload that fails is NOT the end of the renewal. `last` tracks what the
    /// compute loaded, not what was last seen, so the change is still a change
    /// on the next poll and keeps being offered until a reload lands. Without
    /// this, a renewal that arrived while no write lease was held would be
    /// dropped and the compute would serve the old certificate until it expired
    /// — with the expiry gauges reporting the NEW one the whole time.
    #[test]
    fn a_failed_reload_is_retried_until_it_lands() {
        let (m, mut w) = mount();
        std::fs::write(&m.cert, b"leaf-2").unwrap();
        std::fs::write(&m.key, b"key-2").unwrap();

        // Three polls, standing in for three failed reloads: each must still
        // report the renewal.
        for attempt in 1..=3 {
            assert!(
                w.poll().is_some(),
                "attempt {attempt}: a renewal whose reload failed must be retried"
            );
        }

        // The fourth lands.
        let seen = w.poll().expect("still pending");
        w.loaded(seen);
        assert!(w.poll().is_none(), "a landed reload settles the renewal");
    }

    /// What `loaded` adopts is the observation the reload was made FOR, so a
    /// swap during the reload is still a change afterwards rather than being
    /// silently treated as already served.
    #[test]
    fn a_swap_during_the_reload_is_not_adopted() {
        let (m, mut w) = mount();
        std::fs::write(&m.cert, b"leaf-2").unwrap();
        let seen = w.poll().expect("the first renewal");
        // cert-manager writes again while the reload is in flight.
        std::fs::write(&m.cert, b"leaf-3").unwrap();
        w.loaded(seen);
        assert!(
            w.poll().is_some(),
            "the certificate written during the reload is still unloaded"
        );
    }

    /// A Secret resync that rewrites the same bytes is not a renewal.
    #[test]
    fn identical_contents_reload_nothing() {
        let (m, mut w) = mount();
        std::fs::write(&m.cert, b"leaf-1").unwrap();
        std::fs::write(&m.key, b"key-1").unwrap();
        assert!(w.poll().is_none());
    }

    /// An unreadable file is a mount that is not there, not a certificate that
    /// changed — and it must not start reloading when it comes back to what the
    /// compute already loaded.
    #[test]
    fn a_vanished_file_reloads_nothing() {
        let (m, mut w) = mount();
        std::fs::remove_file(&m.cert).unwrap();
        assert!(w.poll().is_none(), "an unreadable file is not a change");
        std::fs::write(&m.cert, b"leaf-1").unwrap();
        assert!(
            w.poll().is_none(),
            "the same bytes coming back is not a change"
        );
    }

    /// The CA bundle is derived from the leaf's own path, because cert-manager
    /// writes both into one Secret and one mount.
    #[test]
    fn the_ca_is_looked_for_beside_the_leaf() {
        assert_eq!(
            ca_file_beside("/etc/amp/tls/compute/tls.crt"),
            "/etc/amp/tls/compute/ca.crt"
        );
        assert_eq!(ca_file_beside("tls.crt"), "ca.crt");
    }

    /// Everything that is not a parseable certificate reports 0 rather than a
    /// stale or panicking value: an absent mount, an empty file, and bytes that
    /// are not PEM at all. An alert reads 0 as "no certificate", which is the
    /// truth in every one of those cases.
    #[test]
    fn an_unreadable_certificate_reports_no_expiry() {
        let dir = camino_tempfile::tempdir().unwrap();
        let missing = dir.path().join("nope.crt");
        assert_eq!(not_after_of(missing.as_str()), 0);

        let empty = dir.path().join("empty.crt");
        std::fs::write(&empty, b"").unwrap();
        assert_eq!(not_after_of(empty.as_str()), 0);

        let junk = dir.path().join("junk.crt");
        std::fs::write(&junk, b"not a certificate").unwrap();
        assert_eq!(not_after_of(junk.as_str()), 0);
    }

    /// A real certificate's notAfter reaches the gauge as a future Unix time.
    /// Generated rather than checked in, so the fixture cannot expire and start
    /// failing the suite on a date nobody chose — which is also why the
    /// assertion is a bound rather than an equality.
    #[test]
    fn a_real_certificate_reports_a_future_expiry() {
        let dir = camino_tempfile::tempdir().unwrap();
        let cert = dir.path().join("tls.crt");
        std::fs::write(&cert, self_signed_pem()).unwrap();

        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64;
        let not_after = not_after_of(cert.as_str());
        assert!(
            not_after > now,
            "notAfter {not_after} should be in the future (now {now})"
        );
    }

    /// A certificate that only appears after the watcher started — cert-manager
    /// had not issued when the pod came up — is a change, because the compute
    /// started with `ssl = on` and no key file to serve.
    #[test]
    fn a_certificate_that_arrives_late_reloads_the_compute() {
        let dir = camino_tempfile::tempdir().unwrap();
        let (cert, key) = (dir.path().join("tls.crt"), dir.path().join("tls.key"));
        let mut w = Watched::new(cert.as_str(), key.as_str());
        std::fs::write(&cert, b"leaf-1").unwrap();
        std::fs::write(&key, b"key-1").unwrap();
        assert!(w.poll().is_some());
    }

    /// The confirmation compares the DER the server presents against the DER in
    /// the mounted PEM, so the extraction has to produce exactly what a
    /// handshake yields. Round-tripped against the parser rather than a checked
    /// -in fixture: a hand-rolled base64 that dropped a byte would still look
    /// like a certificate to the eye, and would then never match anything.
    #[test]
    fn the_leaf_der_is_what_a_handshake_would_present() {
        let dir = camino_tempfile::tempdir().unwrap();
        let cert = dir.path().join("tls.crt");
        let pem = self_signed_pem();
        std::fs::write(&cert, &pem).unwrap();

        let der = leaf_der(cert.as_str()).expect("a PEM certificate has a leaf");
        assert!(
            !der.is_empty() && der[0] == 0x30,
            "a certificate is a DER SEQUENCE"
        );
        assert_eq!(
            ysql_client::leaf_der_from_pem(pem.as_bytes()).as_deref(),
            Some(der.as_slice()),
            "reading through the file must match reading the PEM directly"
        );
    }

    /// Absence and junk are absence, not a panic and not a wrong answer — the
    /// same contract the expiry side has, because the confirmation path reads
    /// the same kubelet-written file.
    #[test]
    fn a_file_with_no_certificate_has_no_leaf() {
        let dir = camino_tempfile::tempdir().unwrap();
        for (name, body) in [
            ("missing.crt", None),
            ("empty.crt", Some("".as_bytes())),
            ("junk.crt", Some(b"not a certificate".as_slice())),
            (
                "halfpem.crt",
                Some(b"-----BEGIN CERTIFICATE-----".as_slice()),
            ),
        ] {
            let p = dir.path().join(name);
            if let Some(b) = body {
                std::fs::write(&p, b).unwrap();
            }
            assert_eq!(leaf_der(p.as_str()), None, "{name} should have no leaf");
        }
    }

    /// The names a certificate carries are what an operator needs to compare
    /// against the address clients were handed — the check nothing in the
    /// render path can do for a Secret the chart did not create.
    #[test]
    fn a_certificate_reports_the_names_it_covers() {
        let info = ysql_client::describe_certificate_pem(self_signed_pem().as_bytes())
            .unwrap()
            .expect("a self-signed certificate describes itself");
        assert_eq!(info.dns_names, vec!["ep-test.example.com".to_string()]);
    }
}
