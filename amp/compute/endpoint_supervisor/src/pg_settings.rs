//! Resource-derived Postgres settings for the endpoint's compute.
//!
//! Every knob here except `shared_buffers` is set as a **ceiling/live pair**: a
//! generous `PGC_POSTMASTER` ceiling that never has to change, plus a live value
//! underneath it that is `PGC_SIGHUP` or `PGC_USERSET` and can therefore be
//! retuned with a reload instead of a restart. This is the same split PG18 uses
//! for `autovacuum_worker_slots` (ceiling) vs `autovacuum_max_workers` (live),
//! and the LFC already has the shape — `file_cache.c`'s check hook enforces
//! `file_cache_size_limit <= max_file_cache_size`.
//!
//! `shared_buffers` is the one knob with no such split (`PGC_POSTMASTER`,
//! `guc_tables.c`). Consequence: after an in-place memory *increase* it is
//! merely undersized, which is safe; after a *decrease* it can sit above the new
//! cgroup limit, which is the one case that genuinely requires a Postgres
//! restart.

use crate::resources::Resources;

/// Fraction of the endpoint's memory given to `shared_buffers`.
pub const SHARED_BUFFERS_FRACTION: f64 = 0.50;

/// Fraction of the data volume the LFC may actually occupy. The ceiling is the
/// whole volume; this is the live limit, so the WAL, the logs and the rest of
/// pgdata — which share the volume — keep headroom.
pub const LFC_VOLUME_FRACTION: f64 = 0.90;

/// Upper bound on the LFC ceiling. The ceiling reserves shared memory for the
/// chunk hash table — one entry per 1 MiB chunk, ~120 B each
/// (`RequestAddinShmemSpace` in `pgxn/yb_amp/file_cache.c`) — so at the volume
/// sizes endpoints get it is single-digit MiB, but a very large volume would
/// reserve hundreds of MiB for a cache that big is never useful.
pub const LFC_CEILING_MAX_MB: u64 = 128 * 1024;

/// Postmaster-context ceilings, deliberately generous and never derived, so the
/// live worker counts beneath them stay reload-only.
pub const MAX_WORKER_PROCESSES: u32 = 64;
pub const AUTOVACUUM_WORKER_SLOTS: u32 = 16;

/// A rendered `name = value` GUC, ready to append to the compute spec.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Setting {
    pub name: String,
    pub value: String,
    /// The compute spec's `vartype`. It must match the GUC's real type —
    /// compute_ctl passes it through to the spec it validates, so declaring an
    /// integer GUC like the worker counts as a string is rejected there.
    pub vartype: &'static str,
}

impl Setting {
    fn string(name: &str, value: impl Into<String>) -> Self {
        Self {
            name: name.to_string(),
            value: value.into(),
            vartype: "string",
        }
    }

    fn integer(name: &str, value: impl Into<String>) -> Self {
        Self {
            name: name.to_string(),
            value: value.into(),
            vartype: "integer",
        }
    }
}

/// Anything that could not be derived, so the caller can log it rather than
/// silently shipping a Postgres default that looks intentional.
#[derive(Debug, Default, PartialEq, Eq)]
pub struct Skipped {
    pub memory_unknown: bool,
    pub cpu_unknown: bool,
    pub volume_unknown: bool,
}

/// The derived GUCs for `resources`, plus what had to be skipped.
///
/// These are *defaults*: the caller appends operator overrides after them, and
/// `postgresql.conf` keeps the last occurrence of a name, so an override always
/// wins. (Note the contrast with the daemons' toml, where a duplicated key is a
/// parse error rather than an override — see `config::render`.)
pub fn derive(res: &Resources) -> (Vec<Setting>, Skipped) {
    let mut out = Vec::new();
    let mut skipped = Skipped::default();

    match res.memory_bytes {
        Some(mem) => {
            let mb = ((mem as f64 * SHARED_BUFFERS_FRACTION) as u64 / (1024 * 1024)).max(16);
            out.push(Setting::string("shared_buffers", format!("{mb}MB")));
        }
        None => skipped.memory_unknown = true,
    }

    match res.data_volume_bytes {
        Some(vol) => {
            let vol_mb = vol / (1024 * 1024);
            let ceiling_mb = vol_mb.min(LFC_CEILING_MAX_MB);
            let live_mb = (((vol_mb as f64) * LFC_VOLUME_FRACTION) as u64)
                .min(ceiling_mb)
                .max(1);
            out.push(Setting::string(
                "yb_amp.max_file_cache_size",
                format!("{ceiling_mb}MB"),
            ));
            out.push(Setting::string(
                "yb_amp.file_cache_size_limit",
                format!("{live_mb}MB"),
            ));
        }
        None => skipped.volume_unknown = true,
    }

    // Ceilings are unconditional: they do not depend on the resources, and they
    // are what make the values below reload-only.
    out.push(Setting::integer(
        "max_worker_processes",
        MAX_WORKER_PROCESSES.to_string(),
    ));
    out.push(Setting::integer(
        "autovacuum_worker_slots",
        AUTOVACUUM_WORKER_SLOTS.to_string(),
    ));

    match res.whole_vcpu() {
        Some(vcpu) => {
            let max_parallel = vcpu.min(MAX_WORKER_PROCESSES);
            let per_gather = (vcpu / 2).max(2);
            let maintenance = (vcpu / 4).max(2);
            let autovacuum = (vcpu / 2).clamp(3, 8).min(AUTOVACUUM_WORKER_SLOTS);
            for (name, v) in [
                ("max_parallel_workers", max_parallel),
                ("max_parallel_workers_per_gather", per_gather),
                ("max_parallel_maintenance_workers", maintenance),
                ("autovacuum_max_workers", autovacuum),
            ] {
                out.push(Setting::integer(name, v.to_string()));
            }
        }
        None => skipped.cpu_unknown = true,
    }

    (out, skipped)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::resources::{Cgroup, Resources};

    fn res(mem: Option<u64>, vcpu: Option<f64>, vol: Option<u64>) -> Resources {
        Resources {
            memory_bytes: mem,
            vcpu,
            data_volume_bytes: vol,
        }
    }

    fn get<'a>(s: &'a [Setting], name: &str) -> Option<&'a str> {
        s.iter().find(|x| x.name == name).map(|x| x.value.as_str())
    }

    /// The 4-core shape verified against a live endpoint: 8Gi/12Gi volume.
    #[test]
    fn four_core_matches_the_deployed_values() {
        let (s, skipped) = derive(&res(Some(8 << 30), Some(4.0), Some(12 << 30)));
        assert_eq!(get(&s, "shared_buffers"), Some("4096MB"));
        assert_eq!(get(&s, "yb_amp.max_file_cache_size"), Some("12288MB"));
        assert_eq!(get(&s, "yb_amp.file_cache_size_limit"), Some("11059MB"));
        assert_eq!(get(&s, "max_parallel_workers"), Some("4"));
        assert_eq!(get(&s, "max_parallel_workers_per_gather"), Some("2"));
        assert_eq!(get(&s, "max_parallel_maintenance_workers"), Some("2"));
        assert_eq!(get(&s, "autovacuum_max_workers"), Some("3"));
        assert_eq!(get(&s, "max_worker_processes"), Some("64"));
        assert_eq!(get(&s, "autovacuum_worker_slots"), Some("16"));
        assert_eq!(skipped, Skipped::default());
    }

    /// The 8-core shape, likewise measured: 16Gi/23Gi volume.
    #[test]
    fn eight_core_matches_the_deployed_values() {
        let (s, _) = derive(&res(Some(16 << 30), Some(8.0), Some(23 << 30)));
        assert_eq!(get(&s, "shared_buffers"), Some("8192MB"));
        assert_eq!(get(&s, "yb_amp.max_file_cache_size"), Some("23552MB"));
        assert_eq!(get(&s, "yb_amp.file_cache_size_limit"), Some("21196MB"));
        assert_eq!(get(&s, "max_parallel_workers"), Some("8"));
        assert_eq!(get(&s, "max_parallel_workers_per_gather"), Some("4"));
        assert_eq!(get(&s, "max_parallel_maintenance_workers"), Some("2"));
        assert_eq!(get(&s, "autovacuum_max_workers"), Some("4"));
    }

    /// An unknown input skips its settings instead of substituting a guess — the
    /// caller logs it. Nothing may silently ship a Postgres default that looks
    /// deliberate.
    #[test]
    fn unknown_inputs_are_skipped_not_guessed() {
        let (s, skipped) = derive(&res(None, None, None));
        assert_eq!(get(&s, "shared_buffers"), None);
        assert_eq!(get(&s, "yb_amp.file_cache_size_limit"), None);
        assert_eq!(get(&s, "max_parallel_workers"), None);
        // The ceilings do not depend on resources, so they are still emitted.
        assert_eq!(get(&s, "max_worker_processes"), Some("64"));
        assert_eq!(
            skipped,
            Skipped {
                memory_unknown: true,
                cpu_unknown: true,
                volume_unknown: true
            }
        );
    }

    /// The LFC lives on the data volume, so a huge volume must not reserve
    /// hundreds of MiB of shared memory for the ceiling's hash table.
    #[test]
    fn lfc_ceiling_is_clamped_and_the_live_limit_never_exceeds_it() {
        let (s, _) = derive(&res(None, None, Some(500 << 30)));
        assert_eq!(
            get(&s, "yb_amp.max_file_cache_size"),
            Some("131072MB"),
            "ceiling clamps at 128GiB"
        );
        let live: u64 = get(&s, "yb_amp.file_cache_size_limit")
            .unwrap()
            .trim_end_matches("MB")
            .parse()
            .unwrap();
        assert!(
            live <= LFC_CEILING_MAX_MB,
            "live {live} exceeded the ceiling"
        );
    }

    /// A 1-vCPU endpoint still gets usable parallelism, not zero.
    #[test]
    fn single_core_floors() {
        let (s, _) = derive(&res(Some(1 << 30), Some(1.0), Some(1 << 30)));
        assert_eq!(get(&s, "max_parallel_workers"), Some("1"));
        assert_eq!(get(&s, "max_parallel_workers_per_gather"), Some("2"));
        assert_eq!(get(&s, "autovacuum_max_workers"), Some("3"));
        // shared_buffers floors at 16MB rather than rounding to nothing.
        assert_eq!(get(&s, "shared_buffers"), Some("512MB"));
    }

    /// A fractional reservation is a whole core for sizing purposes.
    #[test]
    fn fractional_cpu_is_floored_to_one() {
        let cg = Cgroup::new("/nonexistent-cgroup-root");
        let r = Resources::resolve(&cg, Some("500m"), Some("2Gi"), Some("5Gi"), None);
        let (s, skipped) = derive(&r);
        assert!(!skipped.cpu_unknown);
        assert_eq!(get(&s, "max_parallel_workers"), Some("1"));
    }
}
