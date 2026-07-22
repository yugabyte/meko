//! On-disk layout contract of the co-located topology: a timeline's
//! pageserver + safekeeper live under the endpoint that owns them
//! (`endpoints/<endpoint>/{pageserver,safekeeper}`), each endpoint also
//! gets its own `endpoint_storage/`, no legacy top-level `pageserver_*/`,
//! `safekeepers/`, or `endpoint_storage/` dirs exist, and a branch's
//! pair + endpoint_storage are provisioned at `endpoint create` and torn
//! down at `endpoint stop --destroy`.

use std::path::Path;

use anyhow::bail;

use crate::cluster::{Cluster, step};

const LAYOUT_BRANCH: &str = "layout_b";
const LAYOUT_EP: &str = "ep_layout";

fn must_exist(p: &Path, what: &str) -> anyhow::Result<()> {
    if !p.exists() {
        bail!("missing {what}");
    }
    Ok(())
}

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    // Layout-test endpoint sits in the compute lane, above `main`.
    let layout_pg_port = c.compute_port() + 128;
    let r = c.repo_dir();

    step("main branch's pair is co-located under endpoints/main/");
    must_exist(
        &r.join("endpoints/main/pageserver/pageserver.toml"),
        "endpoints/main/pageserver/pageserver.toml",
    )?;
    // The pageserver has no node id, so no identity.toml is written.
    if r.join("endpoints/main/pageserver/identity.toml").exists() {
        bail!("endpoints/main/pageserver/identity.toml should not exist (no node id)");
    }
    must_exist(
        &r.join("endpoints/main/pageserver/pageserver.pid"),
        "endpoints/main/pageserver/pageserver.pid",
    )?;
    must_exist(
        &r.join("endpoints/main/safekeeper/safekeeper.pid"),
        "endpoints/main/safekeeper/safekeeper.pid",
    )?;
    must_exist(
        &r.join("endpoints/main/endpoint_storage/endpoint_storage.pid"),
        "endpoints/main/endpoint_storage/endpoint_storage.pid",
    )?;

    step("no legacy top-level pageserver_*/, safekeepers/, or endpoint_storage/ dirs");
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
    if r.join("endpoint_storage").is_dir() {
        bail!("found legacy top-level endpoint_storage/ dir");
    }

    step(&format!("branch {LAYOUT_BRANCH} (no new on-disk dirs yet)"));
    c.yb(&["branch", "create", "--branch-name", LAYOUT_BRANCH, "--ancestor-branch-name", "main"])?;
    if r.join("endpoints").join(LAYOUT_EP).exists() {
        bail!("endpoints/{LAYOUT_EP} should not exist before endpoint create");
    }

    step(&format!(
        "endpoint create {LAYOUT_EP} provisions the branch's pair under its folder"
    ));
    c.yb(&[
        "endpoint", "create", LAYOUT_EP, "--branch-name", LAYOUT_BRANCH,
        "--pg-version", "18", "--pg-port", &layout_pg_port.to_string(),
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
    must_exist(
        &ep_dir.join("endpoint_storage/endpoint_storage.pid"),
        &format!("endpoints/{LAYOUT_EP}/endpoint_storage/endpoint_storage.pid"),
    )?;
    // pgdata is only laid down on `endpoint start`, not at create time.
    if ep_dir.join("pgdata").exists() {
        bail!("pgdata should not exist before endpoint start");
    }

    step("endpoint stop --destroy tears the pair down (deprovision)");
    c.yb(&["endpoint", "stop", LAYOUT_EP, "--destroy"])?;
    if ep_dir.exists() {
        bail!("endpoints/{LAYOUT_EP} should be gone after --destroy");
    }

    Ok(())
}
