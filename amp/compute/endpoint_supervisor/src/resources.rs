//! What the pod actually has: CPU, memory, and the size of its data volume.
//!
//! Every derived setting is sized from these three, so they are the one place a
//! wrong answer becomes a wrong `shared_buffers` or an evicted pod.
//!
//! **The cgroup is preferred over the environment on purpose.** The controller
//! resizes endpoints *in place* via the `pods/resize` subresource (see
//! `classifyResize` in `controller/internal/deploy/kubernetes.go`), which moves
//! the container's cgroup limits without restarting it and without re-rendering
//! the ConfigMap. After such a resize `ENDPOINT_CPU` / `ENDPOINT_MEMORY` still
//! describe the size the pod *started* with, so anything derived from them is
//! silently stale. The env values remain the fallback for environments with no
//! limit set (plain Docker, local runs).

use camino::{Utf8Path, Utf8PathBuf};

/// Binary (`Ki`/`Mi`/`Gi`/`Ti`) and decimal (`k`/`M`/`G`/`T`) multipliers, as
/// Kubernetes writes them. Longest suffix first so `Mi` is not read as `M`.
const BYTE_SUFFIXES: &[(&str, f64)] = &[
    ("Ki", 1024.0),
    ("Mi", 1024.0 * 1024.0),
    ("Gi", 1024.0 * 1024.0 * 1024.0),
    ("Ti", 1024.0 * 1024.0 * 1024.0 * 1024.0),
    ("k", 1e3),
    ("K", 1e3),
    ("M", 1e6),
    ("G", 1e9),
    ("T", 1e12),
];

/// A Kubernetes memory/storage quantity in bytes, or `None` if unparseable.
///
/// `None` rather than a default: a typo should make the caller skip the derived
/// setting and say so, not silently size a cache off a guess.
pub fn parse_byte_quantity(s: &str) -> Option<u64> {
    let s = s.trim();
    if s.is_empty() {
        return None;
    }
    for (suffix, mult) in BYTE_SUFFIXES {
        if let Some(head) = s.strip_suffix(suffix) {
            let v: f64 = head.trim().parse().ok()?;
            if v < 0.0 {
                return None;
            }
            return Some((v * mult) as u64);
        }
    }
    let v: f64 = s.parse().ok()?;
    if v < 0.0 { None } else { Some(v as u64) }
}

/// A Kubernetes CPU quantity in cores (`"2"`, `"1500m"`, `"0.5"`).
pub fn parse_cpu_quantity(s: &str) -> Option<f64> {
    let s = s.trim();
    if s.is_empty() {
        return None;
    }
    let v = if let Some(millis) = s.strip_suffix('m') {
        millis.trim().parse::<f64>().ok()? / 1000.0
    } else {
        s.parse::<f64>().ok()?
    };
    if v > 0.0 { Some(v) } else { None }
}

/// cgroup v1 and v2 report "no limit" as a huge sentinel; anything past a
/// petabyte is a sentinel, not a real container limit.
const UNLIMITED_BYTES: u64 = 1 << 50;

/// Reads the container's cgroup limits. Split out behind a root path so tests
/// can point it at a fixture tree instead of `/sys/fs/cgroup`.
pub struct Cgroup {
    root: Utf8PathBuf,
}

impl Cgroup {
    pub fn new(root: impl Into<Utf8PathBuf>) -> Self {
        Self { root: root.into() }
    }

    fn read(&self, rel: &str) -> Option<String> {
        std::fs::read_to_string(self.root.join(rel))
            .ok()
            .map(|s| s.trim().to_string())
    }

    /// The memory ceiling in bytes, or `None` when unlimited/unavailable.
    pub fn memory_limit_bytes(&self) -> Option<u64> {
        let raw = self
            .read("memory.max")
            .or_else(|| self.read("memory/memory.limit_in_bytes"))?;
        if raw == "max" {
            return None;
        }
        let v: u64 = raw.parse().ok()?;
        (v > 0 && v < UNLIMITED_BYTES).then_some(v)
    }

    /// The CPU quota in cores, or `None` when unlimited/unavailable.
    pub fn cpu_limit(&self) -> Option<f64> {
        // v2: "<quota> <period>", or "max <period>" when unset.
        if let Some(raw) = self.read("cpu.max") {
            let mut parts = raw.split_whitespace();
            let quota = parts.next()?;
            let period: f64 = parts.next()?.parse().ok()?;
            if quota == "max" {
                return None;
            }
            let quota: f64 = quota.parse().ok()?;
            return (quota > 0.0 && period > 0.0).then_some(quota / period);
        }
        // v1: a negative quota means unset.
        let quota: f64 = self.read("cpu/cpu.cfs_quota_us")?.parse().ok()?;
        let period: f64 = self.read("cpu/cpu.cfs_period_us")?.parse().ok()?;
        (quota > 0.0 && period > 0.0).then_some(quota / period)
    }
}

/// The resources every derived setting is computed from. A field is `None` when
/// its input could not be determined, which callers must treat as "leave the
/// setting alone" rather than substituting a guess.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Resources {
    pub memory_bytes: Option<u64>,
    pub vcpu: Option<f64>,
    /// The budget the data volume's contents must stay inside.
    pub data_volume_bytes: Option<u64>,
}

impl Resources {
    /// cgroup first, env second — see the module docs.
    ///
    /// `data_volume_declared` is the chart's `ENDPOINT_DATA_VOLUME_SIZE` and
    /// `data_volume_statvfs` is what the filesystem reports; the **smaller**
    /// wins. On an `emptyDir` the two differ enormously: the kubelet enforces
    /// `sizeLimit` by watching usage rather than with a filesystem quota, so
    /// `statvfs` reports the whole node filesystem, and sizing a cache off that
    /// lets it grow past the limit and get the pod evicted for exceeding
    /// ephemeral-storage.
    pub fn resolve(
        cgroup: &Cgroup,
        env_cpu: Option<&str>,
        env_memory: Option<&str>,
        data_volume_declared: Option<&str>,
        data_volume_statvfs: Option<u64>,
    ) -> Self {
        let memory_bytes = cgroup
            .memory_limit_bytes()
            .or_else(|| env_memory.and_then(parse_byte_quantity));
        let vcpu = cgroup
            .cpu_limit()
            .or_else(|| env_cpu.and_then(parse_cpu_quantity));

        let declared = data_volume_declared.and_then(parse_byte_quantity);
        let data_volume_bytes = match (declared, data_volume_statvfs) {
            (Some(d), Some(fs)) => Some(d.min(fs)),
            (Some(d), None) => Some(d),
            (None, Some(fs)) => Some(fs),
            (None, None) => None,
        };

        Self {
            memory_bytes,
            vcpu,
            data_volume_bytes,
        }
    }

    /// Whole cores, floored at 1: a fractional reservation still gets the
    /// baseline sizing rather than zero-sized pools.
    pub fn whole_vcpu(&self) -> Option<u32> {
        self.vcpu.map(|c| (c as u32).max(1))
    }
}

/// Total bytes of the filesystem backing `path`.
#[cfg(unix)]
pub fn statvfs_total_bytes(path: &Utf8Path) -> Option<u64> {
    use std::ffi::CString;
    use std::os::unix::ffi::OsStrExt as _;

    let c = CString::new(path.as_os_str().as_bytes()).ok()?;
    // SAFETY: `c` is a valid NUL-terminated path and `stat` is only read after
    // a zero (success) return.
    unsafe {
        let mut stat: libc::statvfs = std::mem::zeroed();
        if libc::statvfs(c.as_ptr(), &mut stat) != 0 {
            return None;
        }
        // The casts are no-ops on glibc/x86_64, where both fields are already
        // u64, but statvfs's field types vary by target and libc (32-bit and
        // musl differ), so they are kept for portability rather than removed to
        // satisfy the lint on this one platform.
        #[allow(clippy::unnecessary_cast)]
        let total = (stat.f_blocks as u64).checked_mul(stat.f_frsize as u64)?;
        (total > 0).then_some(total)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A unique-per-test cgroup fixture tree, so cases cannot see each other's
    /// files (they run in parallel threads of one process).
    fn fixture(files: &[(&str, &str)]) -> camino::Utf8PathBuf {
        static N: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
        let n = N.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        let dir = camino::Utf8PathBuf::from_path_buf(std::env::temp_dir())
            .unwrap()
            .join(format!("cg-{pid}-{n}", pid = std::process::id()));
        for (rel, body) in files {
            let p = dir.join(rel);
            std::fs::create_dir_all(p.parent().unwrap()).unwrap();
            std::fs::write(p, body).unwrap();
        }
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    #[test]
    fn byte_quantities() {
        assert_eq!(parse_byte_quantity("10Gi"), Some(10 << 30));
        assert_eq!(parse_byte_quantity("512Mi"), Some(512 << 20));
        assert_eq!(parse_byte_quantity("98831908Ki"), Some(98831908 << 10));
        assert_eq!(parse_byte_quantity("2G"), Some(2_000_000_000));
        assert_eq!(parse_byte_quantity("1073741824"), Some(1 << 30));
        assert_eq!(parse_byte_quantity(" 4Gi "), Some(4 << 30));
        // Unparseable and negative are None, never a default.
        assert_eq!(parse_byte_quantity(""), None);
        assert_eq!(parse_byte_quantity("nope"), None);
        assert_eq!(parse_byte_quantity("-1Gi"), None);
    }

    #[test]
    fn cpu_quantities() {
        assert_eq!(parse_cpu_quantity("2"), Some(2.0));
        assert_eq!(parse_cpu_quantity("1500m"), Some(1.5));
        assert_eq!(parse_cpu_quantity("500m"), Some(0.5));
        assert_eq!(parse_cpu_quantity("0.5"), Some(0.5));
        assert_eq!(parse_cpu_quantity(""), None);
        assert_eq!(parse_cpu_quantity("abc"), None);
        // Zero is not a usable reservation.
        assert_eq!(parse_cpu_quantity("0"), None);
    }

    #[test]
    fn cgroup_v2_is_read() {
        let dir = fixture(&[
            ("memory.max", "8589934592\n"),
            ("cpu.max", "400000 100000\n"),
        ]);
        let cg = Cgroup::new(dir);
        assert_eq!(cg.memory_limit_bytes(), Some(8 << 30));
        assert_eq!(cg.cpu_limit(), Some(4.0));
    }

    #[test]
    fn cgroup_v2_unlimited_reads_as_none() {
        let dir = fixture(&[("memory.max", "max\n"), ("cpu.max", "max 100000\n")]);
        let cg = Cgroup::new(dir);
        assert_eq!(cg.memory_limit_bytes(), None);
        assert_eq!(cg.cpu_limit(), None);
    }

    #[test]
    fn cgroup_v1_fallback() {
        let dir = fixture(&[
            ("memory/memory.limit_in_bytes", "4294967296\n"),
            ("cpu/cpu.cfs_quota_us", "200000\n"),
            ("cpu/cpu.cfs_period_us", "100000\n"),
        ]);
        let cg = Cgroup::new(dir);
        assert_eq!(cg.memory_limit_bytes(), Some(4 << 30));
        assert_eq!(cg.cpu_limit(), Some(2.0));
    }

    #[test]
    fn cgroup_v1_sentinel_is_unlimited() {
        let dir = fixture(&[("memory/memory.limit_in_bytes", "9223372036854771712\n")]);
        assert_eq!(Cgroup::new(dir).memory_limit_bytes(), None);
    }

    /// The whole point of preferring the cgroup: after an in-place resize the
    /// env still says what the pod started with.
    #[test]
    fn cgroup_wins_over_env() {
        let dir = fixture(&[
            ("memory.max", "12884901888\n"),
            ("cpu.max", "600000 100000\n"),
        ]);
        let r = Resources::resolve(
            &Cgroup::new(dir),
            Some("2"),
            Some("4Gi"),
            Some("10Gi"),
            None,
        );
        assert_eq!(r.memory_bytes, Some(12 << 30));
        assert_eq!(r.vcpu, Some(6.0));
    }

    #[test]
    fn env_is_the_fallback_when_no_cgroup_limit() {
        let r = Resources::resolve(
            &Cgroup::new("/nonexistent-cgroup-root"),
            Some("4"),
            Some("8Gi"),
            None,
            None,
        );
        assert_eq!(r.memory_bytes, Some(8 << 30));
        assert_eq!(r.vcpu, Some(4.0));
    }

    /// An emptyDir's statvfs reports the node filesystem, so the declared
    /// budget has to win or the LFC grows past sizeLimit and the pod is evicted.
    #[test]
    fn data_volume_takes_the_smaller_of_declared_and_statvfs() {
        let cg = Cgroup::new("/nonexistent-cgroup-root");
        let r = Resources::resolve(&cg, None, None, Some("10Gi"), Some(500 << 30));
        assert_eq!(r.data_volume_bytes, Some(10 << 30));
        // A PVC reports its own size, which may be the smaller of the two.
        let r = Resources::resolve(&cg, None, None, Some("40Gi"), Some(20 << 30));
        assert_eq!(r.data_volume_bytes, Some(20 << 30));
        // Either alone still yields a budget; neither yields none.
        assert_eq!(
            Resources::resolve(&cg, None, None, None, Some(7 << 30)).data_volume_bytes,
            Some(7 << 30)
        );
        assert_eq!(
            Resources::resolve(&cg, None, None, None, None).data_volume_bytes,
            None
        );
    }

    #[test]
    fn whole_vcpu_floors_at_one() {
        let cg = Cgroup::new("/nonexistent-cgroup-root");
        assert_eq!(
            Resources::resolve(&cg, Some("500m"), None, None, None).whole_vcpu(),
            Some(1)
        );
        assert_eq!(
            Resources::resolve(&cg, Some("7500m"), None, None, None).whole_vcpu(),
            Some(7)
        );
        assert_eq!(
            Resources::resolve(&cg, None, None, None, None).whole_vcpu(),
            None
        );
    }
}
