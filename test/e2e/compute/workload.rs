//! `.sql` golden-workload runner: execute through `psql`, normalize
//! stdout, and diff against the checked-in `<name>.out` capture
//! (pg_regress-style). `E2E_REGEN_EXPECTED=1` regenerates the goldens.

use std::path::Path;
use std::process::Command;

use anyhow::{Context, bail};
use regex::Regex;

use crate::cluster::Cluster;
use crate::tee_println;

/// Normalize psql output (stdout + interleaved stderr) for golden
/// diffing: canonicalize psql's message location prefix to a basename,
/// drop `\timing`'s `Time:` lines and libpq/server version-mismatch
/// warnings, strip trailing whitespace. Result sets, command tags, and
/// `NOTICE`/`WARNING`/`ERROR` text are kept — they carry signal.
fn normalize(input: &str) -> String {
    let time = Regex::new(r"^Time: [0-9.]+ ms").expect("static regex");
    // psql prefixes client messages with the path we pass to `-f`
    // (`psql:/abs/checkout/foo.sql:NN: NOTICE: ...`). Reduce it to the
    // basename so goldens don't bake in a machine-specific checkout path.
    let psql_loc = Regex::new(r"^psql:.*/([^/]+:\d+:)").expect("static regex");
    let version = Regex::new(r"^(psql:\S+ )?(WARNING|NOTICE): .*(server|libpq|protocol) version")
        .expect("static regex");
    let mut out = String::new();
    for line in input.lines() {
        let line = psql_loc.replace(line, "psql:$1");
        if time.is_match(&line) || version.is_match(&line) {
            continue;
        }
        out.push_str(line.trim_end());
        out.push('\n');
    }
    out
}

pub fn run_sql_workload(cluster: &Cluster, label: &str, sql: &Path) -> anyhow::Result<()> {
    let expected = sql.with_extension("out");
    let stem = sql
        .file_stem()
        .map(|s| s.to_string_lossy().into_owned())
        .unwrap_or_else(|| "workload".to_string());
    let actual_path = cluster.repo_dir().join(format!("{stem}.actual"));

    let raw = crate::psql::run_file(cluster.compute_port(), sql)?;
    std::fs::write(&actual_path, &raw).ok();
    let norm = normalize(&raw);

    let regen = std::env::var("E2E_REGEN_EXPECTED").as_deref() == Ok("1");
    if regen {
        std::fs::write(&expected, &norm)
            .with_context(|| format!("regenerating {}", expected.display()))?;
        tee_println!(
            "  ok  — regenerated {}",
            expected.file_name().unwrap_or_default().to_string_lossy()
        );
        return Ok(());
    }

    if !expected.exists() {
        eprintln!("FAIL — no golden output for {label}.");
        eprintln!("  expected file missing: {}", expected.display());
        eprintln!("  generate it with: E2E_REGEN_EXPECTED=1 test/e2e/compute/run.sh …");
        bail!("no golden output for {label}");
    }

    let expected_norm = normalize(
        &std::fs::read_to_string(&expected)
            .with_context(|| format!("reading {}", expected.display()))?,
    );

    if expected_norm == norm {
        tee_println!("  ok  (output matches)");
        return Ok(());
    }

    // Mismatch — produce a unified diff (head -40) exactly like the
    // shell did, by writing both normalized streams and shelling to
    // `diff -u`.
    let exp_tmp = cluster.repo_dir().join(format!("{stem}.expected.norm"));
    let act_tmp = cluster.repo_dir().join(format!("{stem}.actual.norm"));
    std::fs::write(&exp_tmp, &expected_norm).ok();
    std::fs::write(&act_tmp, &norm).ok();
    let diff = Command::new("diff")
        .arg("-u")
        .arg(&exp_tmp)
        .arg(&act_tmp)
        .output();
    eprintln!(
        "FAIL — output mismatch for {label} (vs {}):",
        expected.file_name().unwrap_or_default().to_string_lossy()
    );
    if let Ok(d) = diff {
        for line in String::from_utf8_lossy(&d.stdout).lines().take(40) {
            eprintln!("{line}");
        }
    }
    bail!("output mismatch for {label}");
}
