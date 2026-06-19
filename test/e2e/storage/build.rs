//! Auto-register e2e scenarios from the filesystem. Scans the `fast/` and
//! `slow/` category dirs for `*.rs` files (the `.sql` golden workloads
//! share those dirs and are ignored here) and generates the module
//! declarations + the `fast()`/`slow()` registries into
//! `$OUT_DIR/scenarios_registry.rs`, which `scenarios.rs` includes. Adding
//! a scenario is just dropping a `<name>.rs` (with a
//! `pub async fn run(&Cluster) -> anyhow::Result<()>`) into the right
//! subdir — no list to edit. The subdir is the fast/slow category; the
//! file stem is the test name.

use std::env;
use std::fmt::Write as _;
use std::fs;
use std::path::Path;

fn main() {
    let manifest = env::var("CARGO_MANIFEST_DIR").expect("CARGO_MANIFEST_DIR");
    let out_dir = env::var("OUT_DIR").expect("OUT_DIR");
    let manifest_dir = Path::new(&manifest);

    let mut code = String::new();
    for cat in ["fast", "slow"] {
        let dir = manifest_dir.join(cat);
        // Re-run when files are added to / removed from the category dir.
        println!("cargo:rerun-if-changed={}", dir.display());

        let mut stems: Vec<String> = fs::read_dir(&dir)
            .unwrap_or_else(|e| panic!("reading {}: {e}", dir.display()))
            .filter_map(Result::ok)
            .filter_map(|entry| {
                let path = entry.path();
                (path.extension().and_then(|e| e.to_str()) == Some("rs"))
                    .then(|| path.file_stem().and_then(|s| s.to_str()).map(String::from))
                    .flatten()
            })
            .collect();
        stems.sort();

        // Module container, one `pub mod` per scenario file (absolute
        // #[path] so resolution is independent of where this is included).
        writeln!(code, "mod {cat}_mods {{").unwrap();
        for stem in &stems {
            let abs = dir.join(format!("{stem}.rs"));
            writeln!(code, "    #[path = {:?}]", abs.display().to_string()).unwrap();
            writeln!(code, "    pub mod {stem};").unwrap();
        }
        writeln!(code, "}}\n").unwrap();

        // Registry function, entries in lexical (file-name) order.
        writeln!(code, "pub fn {cat}(c: &Cluster) -> Vec<Scenario<'_>> {{").unwrap();
        writeln!(code, "    vec![").unwrap();
        for stem in &stems {
            writeln!(code, "        ({stem:?}, Box::pin({cat}_mods::{stem}::run(c))),").unwrap();
        }
        writeln!(code, "    ]\n}}\n").unwrap();
    }

    fs::write(Path::new(&out_dir).join("scenarios_registry.rs"), code)
        .expect("writing scenarios_registry.rs");
}
