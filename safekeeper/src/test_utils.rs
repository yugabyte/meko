use std::ffi::CStr;
use std::sync::Arc;

use camino_tempfile::Utf8TempDir;
use postgres_ffi::v17::wal_generator::{LogicalMessageGenerator, WalGenerator};
use utils::id::{BranchId, NodeId};
use utils::lsn::Lsn;

use crate::receive_wal::WalAcceptor;
use crate::safekeeper::{
    AcceptorProposerMessage, AppendRequest, AppendRequestHeader, ProposerAcceptorMessage, SafeKeeper,
};
use crate::send_wal::EndWatch;
use crate::timeline::{SharedState, StateSK, Timeline, get_timeline_dir};
use crate::timelines_set::TimelinesSet;
use crate::yb_wal_storage::YbWalConn;
use crate::{SafeKeeperConf, control_file, receive_wal, yb_wal_storage};

/// A Safekeeper testing or benchmarking environment. Uses a tempdir for storage, removed on drop.
pub struct Env {
    /// Benchmark directory. Deleted when dropped.
    pub tempdir: Utf8TempDir,
}

impl Env {
    /// Creates a new test or benchmarking environment in a temporary directory.
    pub fn new(_fsync: bool) -> anyhow::Result<Self> {
        let tempdir = camino_tempfile::tempdir()?;
        Ok(Self { tempdir })
    }

    /// Constructs a Safekeeper config for the given node ID.
    fn make_conf(&self, node_id: NodeId) -> SafeKeeperConf {
        let mut conf = SafeKeeperConf::dummy();
        conf.my_id = node_id;
        conf.workdir = self.tempdir.path().join(format!("safekeeper-{node_id}"));
        conf
    }

    /// Constructs a Safekeeper with the given node and branch ID.
    ///
    /// The bench harness still builds a synthetic safekeeper backed by
    /// `YbWalStorage`, which in this environment is **not** wired to a
    /// live YB cluster — the produced struct is for in-memory bench
    /// scaffolding only. Tests that need to write WAL must point at a
    /// real cluster via the integration-test harness.
    pub async fn make_safekeeper(
        &self,
        node_id: NodeId,
        branch_id: BranchId,
        _start_lsn: Lsn,
    ) -> anyhow::Result<SafeKeeper<control_file::ControlFileStorage, yb_wal_storage::WalStore>>
    {
        let _ = node_id;
        let _ = branch_id;
        anyhow::bail!(
            "test_utils::Env::make_safekeeper is unsupported in YB-only mode — \
             the local-FS bench scaffolding has been removed"
        )
    }

    /// Constructs a timeline, including a new Safekeeper with the given node ID, and spawns its
    /// manager task.
    pub async fn make_timeline(
        &self,
        node_id: NodeId,
        branch_id: BranchId,
        start_lsn: Lsn,
    ) -> anyhow::Result<Arc<Timeline>> {
        let conf = Arc::new(self.make_conf(node_id));
        let timeline_dir = get_timeline_dir(&conf, &branch_id);

        let safekeeper = self.make_safekeeper(node_id, branch_id, start_lsn).await?;
        let shared_state = SharedState::new(StateSK::Loaded(safekeeper));

        let yb_wal = YbWalConn::from_conf(&conf).await?;

        let timeline = Timeline::new(
            branch_id,
            &timeline_dir,
            shared_state,
            conf.clone(),
            yb_wal.clone(),
        );
        timeline.bootstrap(
            &mut timeline.write_shared_state().await,
            &conf,
            Arc::new(TimelinesSet::default()), // ignored for now
            yb_wal,
        );
        Ok(timeline)
    }

    // This will be dead code when building a non-benchmark target with the
    // benchmarking feature enabled.
    #[allow(dead_code)]
    pub(crate) async fn write_wal(
        tli: Arc<Timeline>,
        start_lsn: Lsn,
        msg_size: usize,
        msg_count: usize,
        prefix: &CStr,
        mut next_record_lsns: Option<&mut Vec<Lsn>>,
    ) -> anyhow::Result<EndWatch> {
        let (msg_tx, msg_rx) = tokio::sync::mpsc::channel(receive_wal::MSG_QUEUE_SIZE);
        let (reply_tx, mut reply_rx) = tokio::sync::mpsc::channel(receive_wal::REPLY_QUEUE_SIZE);

        let end_watch = EndWatch::new(tli.get_commit_lsn_watch_rx());

        WalAcceptor::spawn(tli.wal_residence_guard().await?, msg_rx, reply_tx, Some(0));

        let prefixlen = prefix.to_bytes_with_nul().len();
        assert!(msg_size >= prefixlen);
        let message = vec![0; msg_size - prefixlen];

        let walgen =
            &mut WalGenerator::new(LogicalMessageGenerator::new(prefix, &message), start_lsn);
        for _ in 0..msg_count {
            let (lsn, record) = walgen.next().unwrap();
            if let Some(ref mut lsns) = next_record_lsns {
                lsns.push(lsn);
            }

            let req = AppendRequest {
                h: AppendRequestHeader {
                    begin_lsn: lsn,
                    end_lsn: lsn + record.len() as u64,
                },
                wal_data: record,
            };

            let end_lsn = req.h.end_lsn;

            let msg = ProposerAcceptorMessage::AppendRequest(req);
            msg_tx.send(msg).await?;
            while let Some(reply) = reply_rx.recv().await {
                if let AcceptorProposerMessage::AppendResponse(resp) = reply {
                    if resp.flush_lsn >= end_lsn {
                        break;
                    }
                }
            }
        }

        Ok(end_watch)
    }
}
