use std::str::FromStr as _;

use crate::worker_process::lfc_metrics::LfcMetricsCollector;

use measured::MetricGroup;
use measured::metric::MetricEncoding;
use measured::metric::gauge::GaugeState;
use measured::metric::group::Encoding;
use utils::id::BranchId;

pub struct CommunicatorWorkerProcessStruct {
    runtime: tokio::runtime::Runtime,

    /*** Metrics ***/
    pub(crate) lfc_metrics: LfcMetricsCollector,
}

/// Launch the communicator process's Rust subsystems
pub(super) fn init(
    branch_id: Option<&str>,
) -> Result<&'static CommunicatorWorkerProcessStruct, String> {
    // The caller validated this already
    let _branch_id = branch_id
        .map(BranchId::from_str)
        .transpose()
        .map_err(|e| format!("invalid branch ID: {e}"))?;

    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .thread_name("communicator thread")
        .build()
        .unwrap();

    let worker_struct = CommunicatorWorkerProcessStruct {
        // Note: it's important to not drop the runtime, or all the tasks are dropped
        // too. Including it in the returned struct is one way to keep it around.
        runtime,

        // metrics
        lfc_metrics: LfcMetricsCollector,
    };
    let worker_struct = Box::leak(Box::new(worker_struct));

    // Start the listener on the control socket
    worker_struct
        .runtime
        .block_on(worker_struct.launch_control_socket_listener())
        .map_err(|e| e.to_string())?;

    Ok(worker_struct)
}

impl<T> MetricGroup<T> for CommunicatorWorkerProcessStruct
where
    T: Encoding,
    GaugeState: MetricEncoding<T>,
{
    fn collect_group_into(&self, enc: &mut T) -> Result<(), T::Err> {
        self.lfc_metrics.collect_group_into(enc)
    }
}
