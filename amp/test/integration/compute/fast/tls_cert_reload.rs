//! Prove a renewed client certificate reaches both daemons without a restart.
//!
//! The runbook's leaf-renewal step relies on this: cert-manager rewrites the
//! Secret, the kubelet swaps the mounted files, and the daemons are expected
//! to notice and reconnect on their own. Here the harness plays cert-manager:
//! it re-issues the client certificate (same key and CN, new serial) against
//! the CA the tservers trust and swaps it into place atomically, then asserts
//!   1. both daemons' `yb_tls.reloads` advances — a watcher in each process
//!      compared the file and asked its lanes to reconnect. The serial beside
//!      it is *not* that evidence: `inspect_client_certificate` re-reads the
//!      path on every request, so it reports the rewrite itself and would move
//!      with no watcher running at all;
//!   2. after a write + CHECKPOINT the thin lane keeps serving on both. This
//!      one shows the lane is alive across the reload, not that it rebuilt:
//!      the pre-reload shim session was authenticated at its handshake and
//!      stays valid, which is the property the design relies on;
//!   3. after a compute restart (a new term: the safekeeper persists its
//!      control file and the pageserver re-reads branch state, both over
//!      YSQL) each daemon's YSQL pool recorded a recycle — the slots reopened
//!      with the new certificate on their first use.
//!
//! Runs only against a TLS cluster (`YB_SSLCERT` set); a plaintext run skips —
//! and no CI job sets one today, so this is a local-cluster scenario
//! (`scripts/yb-local.sh` with `YB_TLS=1`), not coverage `amp-pr-build.yml` or
//! `/ci amp_integration_slow` provide.

use std::path::Path;
use std::time::{Duration, Instant};

use anyhow::{Context, bail};

use crate::cluster::{Cluster, step};
use crate::{db, tee_println};

const LANE: &str = "yb_thin_client";
const PS_METRIC: &str = "pageserver_yb_store_op_seconds";
const SK_METRIC: &str = "safekeeper_yb_wal_insert_seconds";
const RECYCLES: &str = "yb_client_connection_recycles_total";

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let tls = c.yb_tls();
    let (Some(ca), Some(cert), Some(key)) = (
        tls.sslrootcert.clone(),
        tls.sslcert.clone(),
        tls.sslkey.clone(),
    ) else {
        tee_println!("  skipped: plaintext run (no YB_SSLCERT)");
        return Ok(());
    };
    let ca_key = std::env::var("YB_TLS_CA_KEY")
        .ok()
        .filter(|v| !v.is_empty())
        .unwrap_or_else(|| sibling(&ca, "ca.key"));
    if !Path::new(&ca_key).exists() {
        bail!(
            "no CA key at {ca_key} to re-issue the client certificate with; set YB_TLS_CA_KEY \
             (scripts/yb-local.sh exports ca.key next to ca.crt)"
        );
    }
    let poll = Duration::from_secs(
        std::env::var("YB_TLS_RELOAD_POLL_SECS")
            .ok()
            .and_then(|s| s.parse().ok())
            .unwrap_or(2),
    );

    step("baseline: the client certificate each daemon reports");
    let sk0 = client_cert(c.sk_http_port(), "safekeeper").await?;
    let ps0 = client_cert(c.ps_http_port(), "pageserver").await?;
    if sk0.serial != ps0.serial {
        bail!(
            "daemons disagree on the client certificate before the test: safekeeper {} vs \
             pageserver {}",
            sk0.serial,
            ps0.serial
        );
    }
    tee_println!(
        "  in use: serial {} CN={}",
        sk0.serial,
        sk0.common_name.as_deref().unwrap_or("-")
    );
    let sk_reloads0 = sk0.reloads;
    let ps_reloads0 = ps0.reloads;
    let sk_lane0 = lane_op_samples(c.sk_http_port(), SK_METRIC).await?;
    let ps_lane0 = lane_op_samples(c.ps_http_port(), PS_METRIC).await?;
    let sk_rec0 = recycles_ok(c.sk_http_port()).await?;
    let ps_rec0 = recycles_ok(c.ps_http_port()).await?;

    step("re-issue the client certificate: same key and CN, new serial");
    let cn = sk0
        .common_name
        .clone()
        .context("the client certificate in use has no CN")?;
    reissue(&cert, &key, &ca, &ca_key, &cn).context("re-issuing the client certificate")?;

    step("wait for a watcher in each daemon to notice and reconnect");
    // The phase offset means the first poll can be a whole interval late.
    let deadline = Instant::now() + poll * 4 + Duration::from_secs(30);
    let (sk1, ps1) = loop {
        let sk = client_cert(c.sk_http_port(), "safekeeper").await?;
        let ps = client_cert(c.ps_http_port(), "pageserver").await?;
        if sk.reloads > sk_reloads0 && ps.reloads > ps_reloads0 {
            break (sk, ps);
        }
        if Instant::now() > deadline {
            bail!(
                "certificate rewritten but no watcher acted on it: safekeeper reloads \
                 {sk_reloads0}->{}, pageserver {ps_reloads0}->{} (serials {} / {})",
                sk.reloads,
                ps.reloads,
                sk.serial,
                ps.serial
            );
        }
        tokio::time::sleep(Duration::from_millis(500)).await;
    };
    if sk1.serial == sk0.serial || ps1.serial == ps0.serial {
        bail!(
            "a reload was reported but the certificate on disk did not change: \
             safekeeper {} pageserver {} (was {})",
            sk1.serial,
            ps1.serial,
            sk0.serial
        );
    }
    if sk1.serial != ps1.serial {
        bail!(
            "daemons picked up different certificates: safekeeper {} vs pageserver {}",
            sk1.serial,
            ps1.serial
        );
    }
    tee_println!(
        "  now: serial {} (was {}); reloads safekeeper {sk_reloads0}->{}, pageserver \
         {ps_reloads0}->{}",
        sk1.serial,
        sk0.serial,
        sk1.reloads,
        ps1.reloads
    );

    step("write + CHECKPOINT: the thin lane must keep serving across the reload");
    let compute = db::compute_conn(c.compute_port());
    db::batch(
        &compute,
        "DROP TABLE IF EXISTS tls_reload_probe;\n\
         CREATE TABLE tls_reload_probe (id int primary key, v text);\n\
         INSERT INTO tls_reload_probe SELECT g, 'r' || g FROM generate_series(1, 200) g;\n\
         CHECKPOINT;",
    )
    .await
    .context("seed tls_reload_probe")?;
    let deadline = Instant::now() + Duration::from_secs(60);
    loop {
        let sk_lane = lane_op_samples(c.sk_http_port(), SK_METRIC).await?;
        let ps_lane = lane_op_samples(c.ps_http_port(), PS_METRIC).await?;
        if sk_lane > sk_lane0 && ps_lane > ps_lane0 {
            break;
        }
        if Instant::now() > deadline {
            bail!(
                "after the reload: safekeeper lane ops {sk_lane0}->{sk_lane}, pageserver lane \
                 ops {ps_lane0}->{ps_lane}; both must advance"
            );
        }
        tokio::time::sleep(Duration::from_millis(500)).await;
    }

    step("restart the compute: the YSQL pools must reopen on the new certificate");
    c.yb_quiet(&["endpoint", "stop", "main", "--mode", "immediate"]);
    c.yb(&["endpoint", "start", "main"])
        .context("restart main endpoint")?;
    let compute = db::compute_conn(c.compute_port());
    let n = db::query_i64(&compute, "SELECT count(*) FROM tls_reload_probe")
        .await
        .context("cold read tls_reload_probe")?;
    if n != 200 {
        bail!("cold read saw {n} rows, want 200");
    }
    let deadline = Instant::now() + Duration::from_secs(60);
    loop {
        let sk_rec = recycles_ok(c.sk_http_port()).await?;
        let ps_rec = recycles_ok(c.ps_http_port()).await?;
        if sk_rec > sk_rec0 && ps_rec > ps_rec0 {
            tee_println!(
                "  YSQL pools reopened on the new certificate (recycles: safekeeper \
                 {sk_rec0}->{sk_rec}, pageserver {ps_rec0}->{ps_rec})"
            );
            break;
        }
        if Instant::now() > deadline {
            bail!(
                "YSQL pools did not reopen after the reload: recycles safekeeper \
                 {sk_rec0}->{sk_rec}, pageserver {ps_rec0}->{ps_rec}"
            );
        }
        tokio::time::sleep(Duration::from_millis(500)).await;
    }

    step("cleanup: drop the probe table");
    db::batch(&compute, "DROP TABLE IF EXISTS tls_reload_probe;")
        .await
        .context("drop tls_reload_probe")?;
    Ok(())
}

struct ClientCert {
    common_name: Option<String>,
    serial: String,
    /// `yb_tls.reloads`: how many times this process acted on a changed file.
    reloads: u64,
}

/// The client certificate a daemon's `/v1/status` says it is using
/// (`yb_tls.client_cert`, see `ysql_client::TlsStatus`).
async fn client_cert(port: u16, who: &str) -> anyhow::Result<ClientCert> {
    let status: serde_json::Value = reqwest::get(format!("http://127.0.0.1:{port}/v1/status"))
        .await
        .and_then(|r| r.error_for_status())
        .with_context(|| format!("GET {who} /v1/status"))?
        .json()
        .await
        .with_context(|| format!("decode {who} /v1/status"))?;
    let tls = &status["yb_tls"];
    if tls.is_null() {
        bail!("{who} /v1/status carries no yb_tls block: {status}");
    }
    if let Some(e) = tls["client_cert_error"].as_str() {
        bail!("{who} reports a client certificate problem: {e}");
    }
    let cert = &tls["client_cert"];
    let serial = cert["serial"]
        .as_str()
        .with_context(|| format!("{who} reports no client certificate: {tls}"))?
        .to_string();
    Ok(ClientCert {
        common_name: cert["common_name"].as_str().map(str::to_string),
        serial,
        reloads: tls["reloads"].as_u64().unwrap_or(0),
    })
}

/// Re-sign `key`'s public key as CN=`cn` with a fresh random serial and swap
/// the result into `cert_path` atomically, the way the kubelet swaps a
/// projected Secret.
fn reissue(cert_path: &str, key: &str, ca: &str, ca_key: &str, cn: &str) -> anyhow::Result<()> {
    let dir = Path::new(cert_path)
        .parent()
        .context("cert path has no parent")?;
    let csr = dir.join("reissue.csr");
    let ext = dir.join("reissue.ext");
    let tmp = dir.join("reissue.crt");
    std::fs::write(&ext, "extendedKeyUsage=clientAuth\n")?;
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(1);
    let serial = format!("0x{nanos:x}");
    let steps: [&[&str]; 2] = [
        &[
            "req", "-new", "-key", key, "-subj", &format!("/CN={cn}"), "-out",
            csr.to_str().unwrap(),
        ],
        &[
            "x509", "-req", "-in", csr.to_str().unwrap(), "-CA", ca, "-CAkey", ca_key,
            "-set_serial", &serial, "-days", "1", "-extfile", ext.to_str().unwrap(), "-out",
            tmp.to_str().unwrap(),
        ],
    ];
    for args in steps {
        let out = std::process::Command::new("openssl")
            .args(args)
            .output()
            .with_context(|| format!("running openssl {}", args[0]))?;
        if !out.status.success() {
            bail!(
                "openssl {} failed: {}",
                args[0],
                String::from_utf8_lossy(&out.stderr)
            );
        }
    }
    std::fs::rename(&tmp, cert_path).context("swapping the new certificate into place")?;
    let _ = std::fs::remove_file(csr);
    let _ = std::fs::remove_file(ext);
    Ok(())
}

fn sibling(path: &str, name: &str) -> String {
    Path::new(path)
        .parent()
        .map(|d| d.join(name))
        .unwrap_or_else(|| Path::new(name).to_path_buf())
        .to_string_lossy()
        .into_owned()
}

async fn scrape(port: u16) -> anyhow::Result<String> {
    reqwest::get(format!("http://127.0.0.1:{port}/metrics"))
        .await
        .and_then(|r| r.error_for_status())
        .with_context(|| format!("scrape /metrics on port {port}"))?
        .text()
        .await
        .context("read /metrics body")
}

/// Largest `<metric>_count` across the histogram's `path="yb_thin_client"`
/// series: how many ops the thin lane has served. 0 until the lane serves one.
async fn lane_op_samples(port: u16, metric: &str) -> anyhow::Result<u64> {
    let body = scrape(port).await?;
    let prefix = format!("{metric}_count{{");
    let label = format!("path=\"{LANE}\"");
    Ok(body
        .lines()
        .filter(|l| l.starts_with(&prefix) && l.contains(&label))
        .filter_map(|l| l.split_whitespace().last())
        .filter_map(|v| v.parse::<f64>().ok())
        .map(|v| v as u64)
        .max()
        .unwrap_or(0))
}

/// `yb_client_connection_recycles_total{outcome="ok"}` on a daemon: YSQL pool
/// slots reopened past their lifetime or after a `recycle_now`.
async fn recycles_ok(port: u16) -> anyhow::Result<u64> {
    let body = scrape(port).await?;
    Ok(body
        .lines()
        .filter(|l| l.starts_with(RECYCLES) && l.contains("outcome=\"ok\""))
        .filter_map(|l| l.split_whitespace().last())
        .filter_map(|v| v.parse::<f64>().ok())
        .map(|v| v as u64)
        .max()
        .unwrap_or(0))
}
