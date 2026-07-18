use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::task::{Context, Poll};

use crate::send_wal::EndWatch;
use crate::timeline::WalResidentTimeline;
use crate::wal_storage::WalReader;
use anyhow::Context as _;
use bytes::Bytes;
use futures::stream::BoxStream;
use futures::{Stream, StreamExt};
use utils::id::BranchId;
use utils::lsn::Lsn;

#[derive(PartialEq, Eq, Debug)]
pub(crate) struct WalBytes {
    /// Raw PG WAL
    pub(crate) wal: Bytes,
    /// Start LSN of [`Self::wal`]
    #[allow(dead_code)]
    pub(crate) wal_start_lsn: Lsn,
    /// End LSN of [`Self::wal`]
    pub(crate) wal_end_lsn: Lsn,
    /// End LSN of WAL available on the safekeeper.
    ///
    /// For pagservers this will be commit LSN,
    /// while for the compute it will be the flush LSN.
    pub(crate) available_wal_end_lsn: Lsn,
}

struct PositionedWalReader {
    start: Lsn,
    end: Lsn,
    reader: Option<WalReader>,
}

/// A streaming WAL reader wrapper which can be reset while running
pub(crate) struct StreamingWalReader {
    stream: BoxStream<'static, WalOrReset>,
    start_changed_tx: tokio::sync::watch::Sender<Lsn>,
    // Added BranchId for instrumentation purposes.
    pub(crate) branch_id: BranchId,
}

pub(crate) enum WalOrReset {
    Wal(anyhow::Result<WalBytes>),
    Reset(Lsn),
}

impl WalOrReset {
    pub(crate) fn get_wal(self) -> Option<anyhow::Result<WalBytes>> {
        match self {
            WalOrReset::Wal(wal) => Some(wal),
            WalOrReset::Reset(_) => None,
        }
    }
}

impl StreamingWalReader {
    pub(crate) fn new(
        tli: WalResidentTimeline,
        start: Lsn,
        end: Lsn,
        end_watch: EndWatch,
        buffer_size: usize,
    ) -> Self {
        let (start_changed_tx, start_changed_rx) = tokio::sync::watch::channel(start);
        let branch_id = tli.branch_id;

        let state = WalReaderStreamState {
            tli,
            wal_reader: PositionedWalReader {
                start,
                end,
                reader: None,
            },
            end_watch,
            buffer: vec![0; buffer_size],
            buffer_size,
            flush_lsn: Arc::new(AtomicU64::new(end.0)),
        };

        // When a change notification is received while polling the internal
        // reader, stop polling the read future and service the change.
        let stream = futures::stream::unfold(
            (state, start_changed_rx),
            |(mut state, mut rx)| async move {
                let wal_or_reset = tokio::select! {
                    read_res = state.read() => { WalOrReset::Wal(read_res) },
                    changed_res = rx.changed() => {
                        if changed_res.is_err() {
                            return None;
                        }

                        let new_start_pos = rx.borrow_and_update();
                        WalOrReset::Reset(*new_start_pos)
                    }
                };

                if let WalOrReset::Reset(lsn) = wal_or_reset {
                    state.wal_reader.start = lsn;
                    state.wal_reader.reader = None;
                }

                Some((wal_or_reset, (state, rx)))
            },
        )
        .boxed();

        Self {
            stream,
            start_changed_tx,
            branch_id,
        }
    }

    /// Reset the stream to a given position.
    pub(crate) async fn reset(&mut self, start: Lsn) {
        self.start_changed_tx.send(start).unwrap();
        while let Some(wal_or_reset) = self.stream.next().await {
            match wal_or_reset {
                WalOrReset::Reset(at) => {
                    // Stream confirmed the reset.
                    // There may only one ongoing reset at any given time,
                    // hence the assertion.
                    assert_eq!(at, start);
                    break;
                }
                WalOrReset::Wal(_) => {
                    // Ignore wal generated before reset was handled
                }
            }
        }
    }
}

impl Stream for StreamingWalReader {
    type Item = WalOrReset;

    fn poll_next(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Option<Self::Item>> {
        Pin::new(&mut self.stream).poll_next(cx)
    }
}

struct WalReaderStreamState {
    tli: WalResidentTimeline,
    wal_reader: PositionedWalReader,
    end_watch: EndWatch,
    buffer: Vec<u8>,
    buffer_size: usize,
    /// Live committed/flush frontier (`= wal_reader.end`), shared into the
    /// `WalReader` and the `YbSegmentReader`s it opens so they never serve YB
    /// bytes that were uncommitted when fetched — a reconnect truncate can
    /// rewrite those, and a stale held chunk would splice them in (#139). Kept
    /// in step with `end` before each read.
    flush_lsn: Arc<std::sync::atomic::AtomicU64>,
}

impl WalReaderStreamState {
    async fn read(&mut self) -> anyhow::Result<WalBytes> {
        // Create reader if needed
        if self.wal_reader.reader.is_none() {
            self.wal_reader.reader = self
                .tli
                .get_walreader(self.wal_reader.start, self.flush_lsn.clone())
                .await
                .map(Some)
                .with_context(|| {
                    format!(
                        "get_walreader failed at start={} (branch_id={})",
                        self.wal_reader.start, self.tli.branch_id
                    )
                })?;
        }

        let have_something_to_send = self.wal_reader.end > self.wal_reader.start;
        if !have_something_to_send {
            tracing::debug!(
                "Waiting for wal: start={}, end={}",
                self.wal_reader.end,
                self.wal_reader.start
            );
            self.wal_reader.end = self
                .end_watch
                .wait_for_lsn(self.wal_reader.start)
                .await
                .with_context(|| {
                    format!("wait_for_lsn failed at start={}", self.wal_reader.start)
                })?;
            tracing::debug!(
                "Done waiting for wal: start={}, end={}",
                self.wal_reader.end,
                self.wal_reader.start
            );
        }

        assert!(
            self.wal_reader.end > self.wal_reader.start,
            "nothing to send after waiting for WAL"
        );

        // Publish the (possibly just-advanced) committed frontier to the YB
        // readers before we read: each `YbSegmentReader` will only serve bytes
        // it fetched at or below this, and re-fetch anything above — so a held
        // chunk can't shadow a reconnect truncate's rewrite of once-uncommitted
        // bytes.
        self.flush_lsn
            .store(self.wal_reader.end.0, Ordering::Release);

        // Calculate chunk size
        let mut chunk_end_pos = self.wal_reader.start + self.buffer_size as u64;
        if chunk_end_pos >= self.wal_reader.end {
            chunk_end_pos = self.wal_reader.end;
        } else {
            chunk_end_pos = chunk_end_pos
                .checked_sub(chunk_end_pos.block_offset())
                .unwrap();
        }

        let send_size = (chunk_end_pos.0 - self.wal_reader.start.0) as usize;
        let buffer = &mut self.buffer[..send_size];

        // Read WAL
        let read_start = self.wal_reader.start;
        let send_size = self
            .wal_reader
            .reader
            .as_mut()
            .unwrap()
            .read(buffer)
            .await
            .with_context(|| {
                format!(
                    "WalReader::read failed at start={read_start} \
                     (chunk_end={chunk_end_pos}, send_size={send_size})"
                )
            })?;

        let wal = Bytes::copy_from_slice(&buffer[..send_size]);
        let result = WalBytes {
            wal,
            wal_start_lsn: self.wal_reader.start,
            wal_end_lsn: self.wal_reader.start + send_size as u64,
            available_wal_end_lsn: self.wal_reader.end,
        };

        self.wal_reader.start += send_size as u64;

        Ok(result)
    }
}

#[cfg(test)]
mod tests {
    use std::str::FromStr;

    use futures::StreamExt;
    use postgres_ffi::MAX_SEND_SIZE;
    use utils::id::{BranchId, NodeId};
    use utils::lsn::Lsn;

    use crate::test_utils::Env;
    use crate::wal_reader_stream::StreamingWalReader;

    #[tokio::test]
    #[ignore = "yb-amp: depends on removed local-FS test scaffolding"]
    async fn test_streaming_wal_reader_reset() {
        let _ = env_logger::builder().is_test(true).try_init();

        const SIZE: usize = 8 * 1024;
        const MSG_COUNT: usize = 200;

        let start_lsn = Lsn::from_str("0/149FD18").unwrap();
        let env = Env::new(true).unwrap();
        let tli = env
            .make_timeline(NodeId(1), BranchId::generate(), start_lsn)
            .await
            .unwrap();

        let resident_tli = tli.wal_residence_guard().await.unwrap();
        let end_watch = Env::write_wal(tli, start_lsn, SIZE, MSG_COUNT, c"yb_amp-file:", None)
            .await
            .unwrap();
        let end_pos = end_watch.get();

        tracing::info!("Doing first round of reads ...");

        let mut streaming_wal_reader =
            StreamingWalReader::new(resident_tli, start_lsn, end_pos, end_watch, MAX_SEND_SIZE);

        let mut before_reset = Vec::new();
        while let Some(wor) = streaming_wal_reader.next().await {
            let wal = wor.get_wal().unwrap().unwrap();
            let stop = wal.available_wal_end_lsn == wal.wal_end_lsn;
            before_reset.push(wal);

            if stop {
                break;
            }
        }

        tracing::info!("Resetting the WAL stream ...");

        streaming_wal_reader.reset(start_lsn).await;

        tracing::info!("Doing second round of reads ...");

        let mut after_reset = Vec::new();
        while let Some(wor) = streaming_wal_reader.next().await {
            let wal = wor.get_wal().unwrap().unwrap();
            let stop = wal.available_wal_end_lsn == wal.wal_end_lsn;
            after_reset.push(wal);

            if stop {
                break;
            }
        }

        assert_eq!(before_reset, after_reset);
    }
}
