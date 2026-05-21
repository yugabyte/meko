//! Control file serialization, deserialization and persistence.

use std::future::Future;
use std::io::Read;
use std::ops::Deref;
use std::path::Path;
use std::sync::Arc;
use std::time::Instant;

use anyhow::{Context, Result, bail, ensure};
use byteorder::{LittleEndian, ReadBytesExt, WriteBytesExt};
use bytes::Bytes;
use camino::{Utf8Path, Utf8PathBuf};
use futures::StreamExt;
use remote_storage::{DownloadError, DownloadOpts, GenericRemoteStorage, RemotePath};
use safekeeper_api::membership::INVALID_GENERATION;
use tokio::fs::File;
use tokio::io::AsyncWriteExt;
use tokio_util::sync::CancellationToken;
use utils::bin_ser::LeSer;
use utils::crashsafe::durable_rename;
use utils::id::TenantTimelineId;

use crate::control_file_upgrade::{downgrade_v10_to_v9, upgrade_control_file};
use crate::metrics::PERSIST_CONTROL_FILE_SECONDS;
use crate::metrics::WAL_DISK_IO_ERRORS;
use crate::state::{EvictionState, TimelinePersistentState};

pub const SK_MAGIC: u32 = 0xcafeceefu32;
pub const SK_FORMAT_VERSION: u32 = 10;

// contains persistent metadata for safekeeper
pub const CONTROL_FILE_NAME: &str = "safekeeper.control";
// needed to atomically update the state using `rename`
const CONTROL_FILE_NAME_PARTIAL: &str = "safekeeper.control.partial";
pub const CHECKSUM_SIZE: usize = size_of::<u32>();

/// Storage should keep actual state inside of it. It should implement Deref
/// trait to access state fields and have persist method for updating that state.
pub trait Storage: Deref<Target = TimelinePersistentState> {
    /// Persist safekeeper state on disk and update internal state.
    fn persist(&mut self, s: &TimelinePersistentState) -> impl Future<Output = Result<()>> + Send;

    /// Timestamp of last persist.
    fn last_persist_at(&self) -> Instant;
}

#[derive(Debug)]
pub struct FileStorage {
    // save timeline dir to avoid reconstructing it every time
    timeline_dir: Utf8PathBuf,
    no_sync: bool,

    /// Last state persisted to disk.
    state: TimelinePersistentState,
    /// Not preserved across restarts.
    last_persist_at: Instant,
}

impl FileStorage {
    /// Initialize storage by loading state from disk.
    pub fn restore_new(timeline_dir: &Utf8Path, no_sync: bool) -> Result<FileStorage> {
        let state = Self::load_control_file_from_dir(timeline_dir)?;

        Ok(FileStorage {
            timeline_dir: timeline_dir.to_path_buf(),
            no_sync,
            state,
            last_persist_at: Instant::now(),
        })
    }

    /// Create and reliably persist new control file at given location.
    ///
    /// Note: we normally call this in temp directory for atomic init, so
    /// interested in FileStorage as a result only in tests.
    pub async fn create_new(
        timeline_dir: &Utf8Path,
        state: TimelinePersistentState,
        no_sync: bool,
    ) -> Result<FileStorage> {
        // we don't support creating new timelines in offloaded state
        assert!(matches!(state.eviction_state, EvictionState::Present));

        let mut store = FileStorage {
            timeline_dir: timeline_dir.to_path_buf(),
            no_sync,
            state: state.clone(),
            last_persist_at: Instant::now(),
        };
        store.persist(&state).await?;
        Ok(store)
    }

    /// Check the magic/version in the on-disk data and deserialize it, if possible.
    fn deser_sk_state(buf: &mut &[u8]) -> Result<TimelinePersistentState> {
        // Read the version independent part
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
        // try to upgrade
        upgrade_control_file(buf, version)
    }

    /// Load control file from given directory.
    fn load_control_file_from_dir(timeline_dir: &Utf8Path) -> Result<TimelinePersistentState> {
        let path = timeline_dir.join(CONTROL_FILE_NAME);
        Self::load_control_file(path)
    }

    /// Read in the control file.
    pub fn load_control_file<P: AsRef<Path>>(
        control_file_path: P,
    ) -> Result<TimelinePersistentState> {
        let mut control_file = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(&control_file_path)
            .with_context(|| {
                format!(
                    "failed to open control file at {}",
                    control_file_path.as_ref().display(),
                )
            })?;

        let mut buf = Vec::new();
        control_file
            .read_to_end(&mut buf)
            .context("failed to read control file")?;

        let calculated_checksum = crc32c::crc32c(&buf[..buf.len() - CHECKSUM_SIZE]);

        let expected_checksum_bytes: &[u8; CHECKSUM_SIZE] =
            buf[buf.len() - CHECKSUM_SIZE..].try_into()?;
        let expected_checksum = u32::from_le_bytes(*expected_checksum_bytes);

        ensure!(
            calculated_checksum == expected_checksum,
            format!(
                "safekeeper control file checksum mismatch: expected {} got {}",
                expected_checksum, calculated_checksum
            )
        );

        let state = FileStorage::deser_sk_state(&mut &buf[..buf.len() - CHECKSUM_SIZE])
            .with_context(|| {
                format!(
                    "while reading control file {}",
                    control_file_path.as_ref().display(),
                )
            })?;
        Ok(state)
    }
}

impl Deref for FileStorage {
    type Target = TimelinePersistentState;

    fn deref(&self) -> &Self::Target {
        &self.state
    }
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

impl Storage for FileStorage {
    /// Persists state durably to the underlying storage.
    async fn persist(&mut self, s: &TimelinePersistentState) -> Result<()> {
        // start timer for metrics
        let _timer = PERSIST_CONTROL_FILE_SECONDS.start_timer();
        // write data to safekeeper.control.partial
        let control_partial_path = self.timeline_dir.join(CONTROL_FILE_NAME_PARTIAL);
        let mut control_partial = File::create(&control_partial_path).await.with_context(|| {
            /* BEGIN_HADRON */
            WAL_DISK_IO_ERRORS.inc();
            /*END_HADRON */
            format!(
                "failed to create partial control file at: {}",
                &control_partial_path
            )
        })?;

        let buf: Vec<u8> = s.write_to_buf()?;

        control_partial.write_all(&buf).await.with_context(|| {
            /* BEGIN_HADRON */
            WAL_DISK_IO_ERRORS.inc();
            /*END_HADRON */
            format!("failed to write safekeeper state into control file at: {control_partial_path}")
        })?;
        control_partial.flush().await.with_context(|| {
            /* BEGIN_HADRON */
            WAL_DISK_IO_ERRORS.inc();
            /*END_HADRON */
            format!("failed to flush safekeeper state into control file at: {control_partial_path}")
        })?;

        let control_path = self.timeline_dir.join(CONTROL_FILE_NAME);
        durable_rename(&control_partial_path, &control_path, !self.no_sync)
            .await
            /* BEGIN_HADRON */
            .inspect_err(|_| WAL_DISK_IO_ERRORS.inc())?;
        /* END_HADRON */

        // update internal state
        self.state = s.clone();
        Ok(())
    }

    fn last_persist_at(&self) -> Instant {
        self.last_persist_at
    }
}

/// Which backend stores the control file. See [`ControlFileStorage`].
#[derive(Copy, Clone, Debug, Default, PartialEq, Eq, clap::ValueEnum, serde::Deserialize, serde::Serialize)]
#[serde(rename_all = "kebab-case")]
pub enum ControlFileBackend {
    /// Local filesystem (`safekeeper.control` under the timeline dir).
    #[default]
    File,
    /// Reuse the safekeeper's `--remote-storage` to persist the control
    /// blob in a `GenericRemoteStorage` (intended for the `YbStore`
    /// variant, but any backend works).
    RemoteStorage,
}

/// Control file backed by a `GenericRemoteStorage`. The on-the-wire bytes
/// are bit-identical to what `FileStorage` would write to disk, so the
/// backends are interchangeable on a per-timeline basis (migrate by
/// copying the blob).
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
        let state = FileStorage::deser_sk_state(&mut &buf[..buf.len() - CHECKSUM_SIZE])
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
            .with_context(|| format!("upload safekeeper control file to {}", self.path))
            .inspect_err(|_| WAL_DISK_IO_ERRORS.inc())?;
        self.state = s.clone();
        Ok(())
    }

    fn last_persist_at(&self) -> Instant {
        self.last_persist_at
    }
}

/// Dispatcher across backend implementations. Pick a variant per timeline at
/// load time; the rest of the safekeeper holds this type generically as
/// `<CTRL: Storage>`, so dispatch is monomorphized.
#[derive(Debug)]
pub enum ControlFileStorage {
    File(FileStorage),
    Yb(YbControlFileStorage),
}

impl Deref for ControlFileStorage {
    type Target = TimelinePersistentState;
    fn deref(&self) -> &Self::Target {
        match self {
            ControlFileStorage::File(s) => s,
            ControlFileStorage::Yb(s) => s,
        }
    }
}

impl Storage for ControlFileStorage {
    async fn persist(&mut self, s: &TimelinePersistentState) -> Result<()> {
        match self {
            ControlFileStorage::File(f) => f.persist(s).await,
            ControlFileStorage::Yb(y) => y.persist(s).await,
        }
    }

    fn last_persist_at(&self) -> Instant {
        match self {
            ControlFileStorage::File(f) => f.last_persist_at(),
            ControlFileStorage::Yb(y) => y.last_persist_at(),
        }
    }
}

impl ControlFileStorage {
    /// Load an existing control file from whichever backend is configured.
    pub async fn restore_new(
        backend: ControlFileBackend,
        timeline_dir: &Utf8Path,
        ttid: &TenantTimelineId,
        no_sync: bool,
        remote_storage: Option<&Arc<GenericRemoteStorage>>,
    ) -> Result<ControlFileStorage> {
        match backend {
            ControlFileBackend::File => {
                FileStorage::restore_new(timeline_dir, no_sync).map(ControlFileStorage::File)
            }
            ControlFileBackend::RemoteStorage => {
                let storage = remote_storage
                    .context("--control-file-backend=remote-storage requires --remote-storage")?
                    .clone();
                YbControlFileStorage::restore_new(storage, ttid)
                    .await
                    .map(ControlFileStorage::Yb)
            }
        }
    }

    /// Create + persist a new control file in whichever backend is configured.
    pub async fn create_new(
        backend: ControlFileBackend,
        timeline_dir: &Utf8Path,
        ttid: &TenantTimelineId,
        state: TimelinePersistentState,
        no_sync: bool,
        remote_storage: Option<&Arc<GenericRemoteStorage>>,
    ) -> Result<ControlFileStorage> {
        match backend {
            ControlFileBackend::File => {
                FileStorage::create_new(timeline_dir, state, no_sync)
                    .await
                    .map(ControlFileStorage::File)
            }
            ControlFileBackend::RemoteStorage => {
                let storage = remote_storage
                    .context("--control-file-backend=remote-storage requires --remote-storage")?
                    .clone();
                YbControlFileStorage::create_new(storage, ttid, state)
                    .await
                    .map(ControlFileStorage::Yb)
            }
        }
    }
}

#[cfg(test)]
mod test {
    use safekeeper_api::membership::{Configuration, MemberSet, SafekeeperGeneration};
    use tokio::fs;
    use utils::lsn::Lsn;

    use super::*;

    const NO_SYNC: bool = true;

    #[tokio::test]
    async fn test_read_write_safekeeper_state() -> anyhow::Result<()> {
        let tempdir = camino_tempfile::tempdir()?;
        let mut state = TimelinePersistentState::empty();
        state.mconf = Configuration {
            generation: SafekeeperGeneration::new(42),
            members: MemberSet::empty(),
            new_members: None,
        };
        let mut storage = FileStorage::create_new(tempdir.path(), state.clone(), NO_SYNC).await?;

        // Make a change.
        state.commit_lsn = Lsn(42);
        storage.persist(&state).await?;

        // Reload the state. It should match the previously persisted state.
        let loaded_state = FileStorage::load_control_file_from_dir(tempdir.path())?;
        assert_eq!(loaded_state, state);
        Ok(())
    }

    #[tokio::test]
    async fn test_safekeeper_state_checksum_mismatch() -> anyhow::Result<()> {
        let tempdir = camino_tempfile::tempdir()?;
        let mut state = TimelinePersistentState::empty();
        let mut storage = FileStorage::create_new(tempdir.path(), state.clone(), NO_SYNC).await?;

        // Make a change.
        state.commit_lsn = Lsn(42);
        storage.persist(&state).await?;

        // Change the first byte to fail checksum validation.
        let ctrl_path = tempdir.path().join(CONTROL_FILE_NAME);
        let mut data = fs::read(&ctrl_path).await?;
        data[0] += 1;
        fs::write(&ctrl_path, &data).await?;

        // Loading the file should fail checksum validation.
        if let Err(err) = FileStorage::load_control_file_from_dir(tempdir.path()) {
            assert!(err.to_string().contains("control file checksum mismatch"))
        } else {
            panic!("expected checksum error")
        }
        Ok(())
    }
}
