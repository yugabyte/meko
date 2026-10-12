use crate::compute::ComputeNode;
use crate::lfc_storage::LfcStorage;
use anyhow::{Context, Result, bail};
use async_compression::tokio::bufread::{ZstdDecoder, ZstdEncoder};
use bytes::Bytes;
use compute_api::responses::LfcOffloadState;
use compute_api::responses::LfcPrewarmState;
use remote_storage::RemotePath;
use std::mem::replace;
use std::sync::Arc;
use std::time::Instant;
use tokio::{io::AsyncReadExt, select, spawn};
use tokio_util::sync::CancellationToken;
use tracing::{error, info};

/// The blob key for an endpoint's LFC state.
///
/// endpoint_id is set to None while prewarming from other endpoint, see compute_promote.rs
/// If not None, takes precedence over pspec.spec.endpoint_id
fn lfc_state_key(
    pspec: &crate::compute::ParsedSpec,
    endpoint_id: Option<String>,
) -> Result<RemotePath> {
    let endpoint_id = endpoint_id.as_ref().or(pspec.spec.endpoint_id.as_ref());
    let Some(endpoint_id) = endpoint_id else {
        bail!("pspec.endpoint_id missing, other endpoint_id not provided")
    };
    LfcStorage::key(&pspec.branch_id.to_string(), endpoint_id)
}

/// In-process only: compared against the last upload by this same compute_ctl,
/// so the hash need not be stable across builds.
fn lfc_state_digest(state: &[u8]) -> u64 {
    use std::hash::{DefaultHasher, Hasher};
    let mut h = DefaultHasher::new();
    h.write(state);
    h.finish()
}

impl ComputeNode {
    pub async fn lfc_prewarm_state(&self) -> LfcPrewarmState {
        self.state.lock().unwrap().lfc_prewarm_state.clone()
    }

    pub fn lfc_offload_state(&self) -> LfcOffloadState {
        self.state.lock().unwrap().lfc_offload_state.clone()
    }

    /// If there is a prewarm request ongoing, return `false`, `true` otherwise.
    /// Has a failpoint "compute-prewarm"
    pub fn prewarm_lfc(self: &Arc<Self>, from_endpoint: Option<String>) -> bool {
        let token: CancellationToken;
        {
            let state = &mut self.state.lock().unwrap();
            token = state.lfc_prewarm_token.clone();
            if let LfcPrewarmState::Prewarming =
                replace(&mut state.lfc_prewarm_state, LfcPrewarmState::Prewarming)
            {
                return false;
            }
        }
        crate::metrics::LFC_PREWARMS.inc();

        let this = self.clone();
        spawn(async move {
            let prewarm_state = match this.prewarm_impl(from_endpoint, token).await {
                Ok(state) => state,
                Err(err) => {
                    crate::metrics::LFC_PREWARM_ERRORS.inc();
                    error!(%err, "could not prewarm LFC");
                    let error = format!("{err:#}");
                    LfcPrewarmState::Failed { error }
                }
            };

            let state = &mut this.state.lock().unwrap();
            if let LfcPrewarmState::Cancelled = prewarm_state {
                state.lfc_prewarm_token = CancellationToken::new();
            }
            state.lfc_prewarm_state = prewarm_state;
        });
        true
    }

    /// from_endpoint: None for endpoint managed by this compute_ctl
    fn lfc_state_key(&self, from_endpoint: Option<String>) -> Result<RemotePath> {
        let state = self.state.lock().unwrap();
        lfc_state_key(state.pspec.as_ref().unwrap(), from_endpoint)
    }

    /// The LFC state blob store, from the connection the spec carries.
    ///
    /// Built per call rather than held: prewarm and offload are rare, and a
    /// long-lived connection to YB for a blob written every few minutes is a
    /// connection to keep alive for nothing.
    async fn lfc_storage(&self) -> Result<LfcStorage> {
        let (proxy, kind) = {
            let state = self.state.lock().unwrap();
            let pspec = state.pspec.as_ref().context("spec not set")?;
            (
                pspec.spec.lfc_state_url.clone(),
                pspec.spec.yb_storage_conn.clone(),
            )
        };
        if let Some(url) = proxy {
            return LfcStorage::proxy(&url);
        }
        LfcStorage::new(
            kind.context("spec.yb_storage_conn missing; LFC prewarm needs the blob store")?,
        )
        .await
    }

    /// Request LFC state from endpoint storage and load corresponding pages into Postgres.
    async fn prewarm_impl(
        &self,
        from_endpoint: Option<String>,
        token: CancellationToken,
    ) -> Result<LfcPrewarmState> {
        let key = self.lfc_state_key(from_endpoint)?;

        #[cfg(feature = "testing")]
        fail::fail_point!("compute-prewarm", |_| bail!("compute-prewarm failpoint"));

        info!(%key, "reading LFC state");
        let mut now = Instant::now();
        let storage = self.lfc_storage().await?;

        // The store's own retries honour this token, so a cancel during a retry
        // sleep is not held until the budget runs out.
        let downloaded = select! {
            _ = token.cancelled() => return Ok(LfcPrewarmState::Cancelled),
            res = storage.download(&key, &token) => res
        }
        .context("reading LFC state")?;

        // Nothing stored: this endpoint has never offloaded, which is the normal
        // first-start case rather than a failure.
        let Some(lfc_state) = downloaded else {
            return Ok(LfcPrewarmState::Skipped);
        };
        let state_download_time_ms = now.elapsed().as_millis() as u32;
        now = Instant::now();

        let mut uncompressed = Vec::new();
        let mut decoder = ZstdDecoder::new(lfc_state.iter().as_slice());
        select! {
            _ = token.cancelled() => return Ok(LfcPrewarmState::Cancelled),
            read = decoder.read_to_end(&mut uncompressed) => read
        }
        .context("decoding LFC state")?;
        let uncompress_time_ms = now.elapsed().as_millis() as u32;
        now = Instant::now();

        let uncompressed_len = uncompressed.len();
        info!(%key, "read LFC state, uncompressed size {uncompressed_len}");

        // Client connection and prewarm info querying are fast and therefore don't need
        // cancellation
        let client = ComputeNode::get_maintenance_client(&self.tokio_conn_conf)
            .await
            .context("connecting to postgres")?;
        let pg_token = client.cancel_token();

        let params: Vec<&(dyn postgres_types::ToSql + Sync)> = vec![&uncompressed];
        select! {
            res = client.query_one("select yb_amp.prewarm_local_cache($1)", &params) => res,
            _ = token.cancelled() => {
                pg_token.cancel_query(postgres::NoTls).await
                    .context("cancelling yb_amp.prewarm_local_cache()")?;
                return Ok(LfcPrewarmState::Cancelled)
            }
        }
        .context("loading LFC state into postgres")
        .map(|_| ())?;
        let prewarm_time_ms = now.elapsed().as_millis() as u32;

        let row = client
            .query_one("select * from yb_amp.get_prewarm_info()", &[])
            .await
            .context("querying prewarm info")?;
        let total = row.try_get(0).unwrap_or_default();
        let prewarmed = row.try_get(1).unwrap_or_default();
        let skipped = row.try_get(2).unwrap_or_default();

        Ok(LfcPrewarmState::Completed {
            total,
            prewarmed,
            skipped,
            state_download_time_ms,
            uncompress_time_ms,
            prewarm_time_ms,
        })
    }

    /// If offload request is ongoing, return false, true otherwise
    pub fn offload_lfc(self: &Arc<Self>) -> bool {
        {
            let state = &mut self.state.lock().unwrap().lfc_offload_state;
            if matches!(
                replace(state, LfcOffloadState::Offloading),
                LfcOffloadState::Offloading
            ) {
                return false;
            }
        }
        let cloned = self.clone();
        spawn(async move { cloned.offload_lfc_with_state_update(false).await });
        true
    }

    pub async fn offload_lfc_async(self: &Arc<Self>) {
        {
            let state = &mut self.state.lock().unwrap().lfc_offload_state;
            if matches!(
                replace(state, LfcOffloadState::Offloading),
                LfcOffloadState::Offloading
            ) {
                return;
            }
        }
        self.offload_lfc_with_state_update(true).await
    }

    /// `skip_unchanged` is for the periodic loop; an explicit offload request
    /// always writes.
    async fn offload_lfc_with_state_update(&self, skip_unchanged: bool) {
        crate::metrics::LFC_OFFLOADS.inc();
        let state = match self.offload_lfc_impl(skip_unchanged).await {
            Ok(state) => state,
            Err(err) => {
                crate::metrics::LFC_OFFLOAD_ERRORS.inc();
                error!(%err, "could not offload LFC");
                let error = format!("{err:#}");
                LfcOffloadState::Failed { error }
            }
        };
        self.state.lock().unwrap().lfc_offload_state = state;
    }

    async fn offload_lfc_impl(&self, skip_unchanged: bool) -> Result<LfcOffloadState> {
        let key = self.lfc_state_key(None)?;
        info!(%key, "requesting LFC state from Postgres");

        let mut now = Instant::now();
        let row = ComputeNode::get_maintenance_client(&self.tokio_conn_conf)
            .await
            .context("connecting to postgres")?
            .query_one("select yb_amp.get_local_cache_state()", &[])
            .await
            .context("querying LFC state")?;
        let state = row
            .try_get::<usize, Option<&[u8]>>(0)
            .context("deserializing LFC state")?;
        let Some(state) = state else {
            info!(%key, "empty LFC state, not exporting");
            return Ok(LfcOffloadState::Skipped);
        };
        let offloaded = Some((key.clone(), lfc_state_digest(state)));
        if skip_unchanged && self.state.lock().unwrap().lfc_offloaded_digest == offloaded {
            info!(%key, "LFC state unchanged since the last offload, not exporting");
            return Ok(LfcOffloadState::Skipped);
        }
        let state_query_time_ms = now.elapsed().as_millis() as u32;
        now = Instant::now();

        let mut compressed = Vec::new();
        ZstdEncoder::new(state)
            .read_to_end(&mut compressed)
            .await
            .context("compressing LFC state")?;
        let compress_time_ms = now.elapsed().as_millis() as u32;
        now = Instant::now();

        let compressed_len = compressed.len();
        info!(%key, "read LFC state from Postgres, compressed size {compressed_len}");

        // Offload is not user-facing, so it is not cancellable: a token here would
        // only ever be cancelled by shutdown, at which point the upload is moot.
        let cancel = CancellationToken::new();
        self.lfc_storage()
            .await?
            .upload(&key, Bytes::from(compressed), &cancel)
            .await?;
        self.state.lock().unwrap().lfc_offloaded_digest = offloaded;
        let state_upload_time_ms = now.elapsed().as_millis() as u32;

        Ok(LfcOffloadState::Completed {
            compress_time_ms,
            state_query_time_ms,
            state_upload_time_ms,
        })
    }

    pub fn cancel_prewarm(self: &Arc<Self>) {
        self.state.lock().unwrap().lfc_prewarm_token.cancel();
    }
}

#[cfg(test)]
mod tests {
    use super::lfc_state_digest;

    #[test]
    fn digest_tracks_state_bytes() {
        let state = vec![7u8; 4096];
        assert_eq!(lfc_state_digest(&state), lfc_state_digest(&state));
        let mut changed = state.clone();
        changed[4095] ^= 1;
        assert_ne!(lfc_state_digest(&state), lfc_state_digest(&changed));
    }
}
