//! Control file serialization, deserialization and persistence.
//!
//! yb-light persists the safekeeper control file as a single blob in
//! `GenericRemoteStorage` (the `YbStore` backend in production). The
//! upstream local-FS `FileStorage` has been removed — the in-row YB
//! durability path is the only one we run, so the dual-backend
//! plumbing (`ControlFileBackend` enum, local FS write/fsync code) is
//! gone too. `YbControlFileStorage` writes a bit-for-bit equivalent of
//! the old on-disk format so existing remote-storage blobs continue to
//! deserialize after the cleanup.

use std::future::Future;
use std::ops::Deref;
use std::sync::Arc;
use std::time::Instant;

use anyhow::{Context, Result, bail, ensure};
use byteorder::{LittleEndian, ReadBytesExt, WriteBytesExt};
use bytes::Bytes;
use futures::StreamExt;
use remote_storage::{DownloadError, DownloadOpts, GenericRemoteStorage, RemotePath};
use safekeeper_api::membership::INVALID_GENERATION;
use tokio_util::sync::CancellationToken;
use utils::bin_ser::LeSer;
use utils::id::TenantTimelineId;

use crate::control_file_upgrade::{downgrade_v10_to_v9, upgrade_control_file};
use crate::metrics::PERSIST_CONTROL_FILE_SECONDS;
use crate::state::{EvictionState, TimelinePersistentState};

pub const SK_MAGIC: u32 = 0xcafeceefu32;
pub const SK_FORMAT_VERSION: u32 = 10;

pub const CHECKSUM_SIZE: usize = size_of::<u32>();

/// Storage should keep actual state inside of it. It should implement Deref
/// trait to access state fields and have persist method for updating that state.
pub trait Storage: Deref<Target = TimelinePersistentState> {
    /// Persist safekeeper state on disk and update internal state.
    fn persist(&mut self, s: &TimelinePersistentState) -> impl Future<Output = Result<()>> + Send;

    /// Timestamp of last persist.
    fn last_persist_at(&self) -> Instant;
}

/// Deserialize a control-file blob. Used by both the YB-backed control
/// file and any external tooling that needs to inspect the bytes.
fn deser_sk_state(buf: &mut &[u8]) -> Result<TimelinePersistentState> {
    let magic = ReadBytesExt::read_u32::<LittleEndian>(buf)?;
    if magic != SK_MAGIC {
        bail!(
            "bad control file magic: {:X}, expected {:X}",
            magic,
            SK_MAGIC
        );
    }
    let version = ReadBytesExt::read_u32::<LittleEndian>(buf)?;
    if version == SK_FORMAT_VERSION {
        let res = TimelinePersistentState::des(buf)?;
        return Ok(res);
    }
    upgrade_control_file(buf, version)
}

impl TimelinePersistentState {
    pub(crate) fn write_to_buf(&self) -> Result<Vec<u8>> {
        let mut buf: Vec<u8> = Vec::new();
        WriteBytesExt::write_u32::<LittleEndian>(&mut buf, SK_MAGIC)?;

        if self.mconf.generation == INVALID_GENERATION {
            // Temp hack for forward compatibility test: in case of none
            // configuration save cfile in previous v9 format.
            const PREV_FORMAT_VERSION: u32 = 9;
            let prev = downgrade_v10_to_v9(self);
            WriteBytesExt::write_u32::<LittleEndian>(&mut buf, PREV_FORMAT_VERSION)?;
            prev.ser_into(&mut buf)?;
        } else {
            // otherwise, we write the current format version
            WriteBytesExt::write_u32::<LittleEndian>(&mut buf, SK_FORMAT_VERSION)?;
            self.ser_into(&mut buf)?;
        }

        // calculate checksum before resize
        let checksum = crc32c::crc32c(&buf);
        buf.extend_from_slice(&checksum.to_le_bytes());
        Ok(buf)
    }
}

/// Control file backed by a `GenericRemoteStorage` (the `YbStore`
/// backend in production). The on-the-wire format is bit-identical to
/// the historical local-FS layout so blobs written before the cleanup
/// keep deserializing.
pub struct YbControlFileStorage {
    storage: Arc<GenericRemoteStorage>,
    /// Absolute key in the remote store. Pattern:
    /// `safekeeper-control/<tenant_id>/<timeline_id>`.
    path: RemotePath,
    state: TimelinePersistentState,
    last_persist_at: Instant,
}

impl std::fmt::Debug for YbControlFileStorage {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("YbControlFileStorage")
            .field("path", &self.path)
            .finish()
    }
}

impl YbControlFileStorage {
    fn key_for(ttid: &TenantTimelineId) -> Result<RemotePath> {
        RemotePath::from_string(&format!(
            "safekeeper-control/{}/{}",
            ttid.tenant_id, ttid.timeline_id
        ))
        .context("build safekeeper control-file remote path")
    }

    /// Load + deserialize the control blob from remote.
    pub async fn restore_new(
        storage: Arc<GenericRemoteStorage>,
        ttid: &TenantTimelineId,
    ) -> Result<YbControlFileStorage> {
        let path = Self::key_for(ttid)?;
        let cancel = CancellationToken::new();
        let mut download = match storage
            .download(&path, &DownloadOpts::default(), &cancel)
            .await
        {
            Ok(d) => d,
            Err(DownloadError::NotFound) => {
                bail!(
                    "control file not found in remote storage at {}",
                    path
                );
            }
            Err(e) => return Err(anyhow::anyhow!(e)),
        };
        let mut buf: Vec<u8> = Vec::new();
        while let Some(chunk) = download.download_stream.next().await {
            let chunk: Bytes = chunk.context("read control file chunk")?;
            buf.extend_from_slice(&chunk);
        }
        if buf.len() < CHECKSUM_SIZE {
            bail!(
                "control file in remote storage is truncated ({} bytes)",
                buf.len()
            );
        }
        let calculated = crc32c::crc32c(&buf[..buf.len() - CHECKSUM_SIZE]);
        let expected_bytes: &[u8; CHECKSUM_SIZE] =
            buf[buf.len() - CHECKSUM_SIZE..].try_into()?;
        let expected = u32::from_le_bytes(*expected_bytes);
        ensure!(
            calculated == expected,
            "safekeeper control file checksum mismatch: expected {expected} got {calculated}"
        );
        let state = deser_sk_state(&mut &buf[..buf.len() - CHECKSUM_SIZE])
            .with_context(|| format!("reading remote control file {path}"))?;
        Ok(YbControlFileStorage {
            storage,
            path,
            state,
            last_persist_at: Instant::now(),
        })
    }

    /// Create + persist a fresh control file in remote storage.
    pub async fn create_new(
        storage: Arc<GenericRemoteStorage>,
        ttid: &TenantTimelineId,
        state: TimelinePersistentState,
    ) -> Result<YbControlFileStorage> {
        assert!(matches!(state.eviction_state, EvictionState::Present));
        let mut store = YbControlFileStorage {
            storage,
            path: Self::key_for(ttid)?,
            state: state.clone(),
            last_persist_at: Instant::now(),
        };
        store.persist(&state).await?;
        Ok(store)
    }
}

impl Deref for YbControlFileStorage {
    type Target = TimelinePersistentState;
    fn deref(&self) -> &Self::Target {
        &self.state
    }
}

impl Storage for YbControlFileStorage {
    async fn persist(&mut self, s: &TimelinePersistentState) -> Result<()> {
        let _timer = PERSIST_CONTROL_FILE_SECONDS.start_timer();
        let buf: Vec<u8> = s.write_to_buf()?;
        let size = buf.len();
        let bytes = Bytes::from(buf);
        let stream = futures::stream::once(async move {
            Ok::<_, std::io::Error>(bytes)
        });
        let cancel = CancellationToken::new();
        self.storage
            .upload(stream, size, &self.path, None, &cancel)
            .await
            .with_context(|| format!("upload safekeeper control file to {}", self.path))?;
        self.state = s.clone();
        Ok(())
    }

    fn last_persist_at(&self) -> Instant {
        self.last_persist_at
    }
}

/// Control-file storage. Only the YB-backed implementation exists;
/// kept as a type alias so call sites read uniformly with the rest of
/// the code that holds a `Storage`-trait value generically.
pub type ControlFileStorage = YbControlFileStorage;

