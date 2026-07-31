//! process metrics that the [`::prometheus`] crate doesn't provide.

// This module has heavy inspiration from the prometheus crate's `process_collector.rs`.

use once_cell::sync::Lazy;
use prometheus::Gauge;

use crate::UIntGauge;

pub struct Collector {
    descs: Vec<prometheus::core::Desc>,
    vmlck: crate::UIntGauge,
    vm_rss: crate::UIntGauge,
    vm_data: crate::UIntGauge,
    rss_anon: crate::UIntGauge,
    rss_file: crate::UIntGauge,
    rss_shmem: crate::UIntGauge,
    cpu_seconds_highres: Gauge,
}

const NMETRICS: usize = 7;

static CLK_TCK_F64: Lazy<f64> = Lazy::new(|| {
    // SAFETY: libc::sysconf is safe, it merely returns a value.
    let long = unsafe { libc::sysconf(libc::_SC_CLK_TCK) };
    if long == -1 {
        panic!("sysconf(_SC_CLK_TCK) failed");
    }
    let convertible_to_f64: i32 =
        i32::try_from(long).expect("sysconf(_SC_CLK_TCK) is larger than i32");
    convertible_to_f64 as f64
});

// procfs reports VmRSS/VmData/RssAnon/RssFile/RssShmem in kibibytes —
// convert to bytes so the gauges line up with our other byte-valued
// metrics (pageserver_yb_pagewriter_unflushed_bytes etc.).
const KIB: u64 = 1024;

impl prometheus::core::Collector for Collector {
    fn desc(&self) -> Vec<&prometheus::core::Desc> {
        self.descs.iter().collect()
    }

    fn collect(&self) -> Vec<prometheus::proto::MetricFamily> {
        let Ok(myself) = procfs::process::Process::myself() else {
            return vec![];
        };
        let mut mfs = Vec::with_capacity(NMETRICS);
        if let Ok(status) = myself.status() {
            if let Some(vmlck) = status.vmlck {
                self.vmlck.set(vmlck);
                mfs.extend(self.vmlck.collect())
            }
            if let Some(v) = status.vmrss {
                self.vm_rss.set(v.saturating_mul(KIB));
                mfs.extend(self.vm_rss.collect());
            }
            if let Some(v) = status.vmdata {
                self.vm_data.set(v.saturating_mul(KIB));
                mfs.extend(self.vm_data.collect());
            }
            if let Some(v) = status.rssanon {
                self.rss_anon.set(v.saturating_mul(KIB));
                mfs.extend(self.rss_anon.collect());
            }
            if let Some(v) = status.rssfile {
                self.rss_file.set(v.saturating_mul(KIB));
                mfs.extend(self.rss_file.collect());
            }
            if let Some(v) = status.rssshmem {
                self.rss_shmem.set(v.saturating_mul(KIB));
                mfs.extend(self.rss_shmem.collect());
            }
        }
        if let Ok(stat) = myself.stat() {
            let cpu_seconds = stat.utime + stat.stime;
            self.cpu_seconds_highres
                .set(cpu_seconds as f64 / *CLK_TCK_F64);
            mfs.extend(self.cpu_seconds_highres.collect());
        }
        mfs
    }
}

impl Collector {
    pub fn new() -> Self {
        let mut descs = Vec::new();

        fn gauge(name: &str, help: &str, descs: &mut Vec<prometheus::core::Desc>) -> UIntGauge {
            let g = UIntGauge::new(name, help).unwrap();
            descs.extend(prometheus::core::Collector::desc(&g).into_iter().cloned());
            g
        }

        let vmlck = gauge(
            "libmetrics_process_status_vmlck",
            "/proc/self/status vmlck (KiB)",
            &mut descs,
        );
        let vm_rss = gauge(
            "libmetrics_process_status_vm_rss_bytes",
            "/proc/self/status VmRSS (bytes) — total resident set size",
            &mut descs,
        );
        let vm_data = gauge(
            "libmetrics_process_status_vm_data_bytes",
            "/proc/self/status VmData (bytes) — data + heap + bss segment",
            &mut descs,
        );
        let rss_anon = gauge(
            "libmetrics_process_status_rss_anon_bytes",
            "/proc/self/status RssAnon (bytes) — resident anonymous memory (heap, stack, private mappings)",
            &mut descs,
        );
        let rss_file = gauge(
            "libmetrics_process_status_rss_file_bytes",
            "/proc/self/status RssFile (bytes) — resident file-backed memory (shared libs, file mmaps)",
            &mut descs,
        );
        let rss_shmem = gauge(
            "libmetrics_process_status_rss_shmem_bytes",
            "/proc/self/status RssShmem (bytes) — resident shared memory (System V / POSIX shm)",
            &mut descs,
        );

        let cpu_seconds_highres = Gauge::new(
            "libmetrics_process_cpu_seconds_highres",
            "Total user and system CPU time spent in seconds.\
             Sub-second resolution, hence better than `process_cpu_seconds_total`.",
        )
        .unwrap();
        descs.extend(
            prometheus::core::Collector::desc(&cpu_seconds_highres)
                .into_iter()
                .cloned(),
        );

        Self {
            descs,
            vmlck,
            vm_rss,
            vm_data,
            rss_anon,
            rss_file,
            rss_shmem,
            cpu_seconds_highres,
        }
    }
}

impl Default for Collector {
    fn default() -> Self {
        Self::new()
    }
}
