//! On-disk layout contract of the co-located topology: a timeline's
//! pageserver + safekeeper live under the endpoint that owns them
//! (`endpoints/<endpoint>/{pageserver,safekeeper}`), no legacy
//! top-level `pageserver_*/` or `safekeepers/` dirs exist, and a
//! branch's pair is provisioned at `endpoint create` and torn down at
//! `endpoint stop --destroy`.

use std::path::Path;

use anyhow::bail;
use regex::Regex;

use crate::cluster::{Cluster, step};

const LAYOUT_BRANCH: &str = "layout_b";
const LAYOUT_EP: &str = "ep_layout";
const LAYOUT_PG_PORT: u16 = 55560;

fn must_exist(p: &Path, what: &str) -> anyhow::Result<()> {
    if !p.exists() {
        bail!("missing {what}");
    }
    Ok(())
}

/// Parse `id = <n>` out of an identity.toml.
fn identity_id(path: &Path) -> anyhow::Result<String> {
    let text = std::fs::read_to_string(path)?;
    let re = Regex::new(r"id\s*=\s*([0-9]+)").expect("static regex");
    re.captures(&text)
        .and_then(|c| c.get(1))
        .map(|m| m.as_str().to_string())
        .ok_or_else(|| anyhow::anyhow!("no id in {}", path.display()))
}

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let r = c.repo_dir();
    let tid = c.tid();

    step("main timeline's pair is co-located under endpoints/main/");
    must_exist(
        &r.join("endpoints/main/pageserver/pageserver.toml"),
        "endpoints/main/pageserver/pageserver.toml",
    )?;
    must_exist(
        &r.join("endpoints/main/pageserver/identity.toml"),
        "endpoints/main/pageserver/identity.toml",
    )?;
    must_exist(
        &r.join("endpoints/main/pageserver/pageserver.pid"),
        "endpoints/main/pageserver/pageserver.pid",
    )?;
    must_exist(
        &r.join("endpoints/main/safekeeper/safekeeper.pid"),
        "endpoints/main/safekeeper/safekeeper.pid",
    )?;

    step("no legacy top-level pageserver_*/ or safekeepers/ dirs");
    if let Ok(entries) = std::fs::read_dir(r) {
        for e in entries.flatten() {
            if e.file_name().to_string_lossy().starts_with("pageserver_") {
                bail!("found legacy top-level dir: {}", e.path().display());
            }
        }
    }
    if r.join("safekeepers").is_dir() {
        bail!("found legacy top-level safekeepers/ dir");
    }

    step(&format!("branch {LAYOUT_BRANCH} (no new on-disk dirs yet)"));
    c.yb(&["timeline", "branch", "--tenant-id", tid, "--branch-name", LAYOUT_BRANCH])?;
    if r.join("endpoints").join(LAYOUT_EP).exists() {
        bail!("endpoints/{LAYOUT_EP} should not exist before endpoint create");
    }

    step(&format!(
        "endpoint create {LAYOUT_EP} provisions the branch's pair under its folder"
    ));
    c.yb(&[
        "endpoint", "create", LAYOUT_EP, "--tenant-id", tid, "--branch-name", LAYOUT_BRANCH,
        "--pg-version", "17", "--pg-port", &LAYOUT_PG_PORT.to_string(),
    ])?;
    let ep_dir = r.join("endpoints").join(LAYOUT_EP);
    must_exist(
        &ep_dir.join("pageserver/pageserver.toml"),
        &format!("endpoints/{LAYOUT_EP}/pageserver/pageserver.toml"),
    )?;
    must_exist(
        &ep_dir.join("safekeeper/safekeeper.pid"),
        &format!("endpoints/{LAYOUT_EP}/safekeeper/safekeeper.pid"),
    )?;
    // pgdata is only laid down on `endpoint start`, not at create time.
    if ep_dir.join("pgdata").exists() {
        bail!("pgdata should not exist before endpoint start");
    }
    // The branch's pageserver got a distinct id from main's.
    let main_id = identity_id(&r.join("endpoints/main/pageserver/identity.toml"))?;
    let br_id = identity_id(&ep_dir.join("pageserver/identity.toml"))?;
    if main_id == br_id {
        bail!("branch pageserver id {br_id} collides with main's");
    }

    step("endpoint stop --destroy tears the pair down (deprovision)");
    c.yb(&["endpoint", "stop", LAYOUT_EP, "--destroy"])?;
    if ep_dir.exists() {
        bail!("endpoints/{LAYOUT_EP} should be gone after --destroy");
    }

    Ok(())
}
