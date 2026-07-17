use std::num::NonZero;
use std::pin::pin;
use std::sync::Arc;
use std::time::{Duration, Instant};

use anyhow::anyhow;
use arc_swap::ArcSwap;
use tonic::codec::CompressionEncoding;
use tracing::{debug, instrument};
use utils::logging::warn_slow;

use crate::pool::{ChannelPool, ClientGuard, ClientPool, StreamGuard, StreamPool};
use crate::retry::Retry;
use compute_api::spec::PageserverProtocol;
use pageserver_page_api as page_api;
use utils::id::BranchId;

/// Max number of concurrent clients per channel (i.e. TCP connection). New channels will be spun up
/// when full.
///
/// Normal requests are small, and we don't pipeline them, so we can afford a large number of
/// streams per connection.
///
/// TODO: tune all of these constants, and consider making them configurable.
const MAX_CLIENTS_PER_CHANNEL: NonZero<usize> = NonZero::new(64).unwrap();

/// Max number of concurrent bulk GetPage streams per channel (i.e. TCP connection). These use a
/// dedicated channel pool with a lower client limit, to avoid TCP-level head-of-line blocking and
/// transmission delays. This also concentrates large window sizes on a smaller set of
/// streams/connections, presumably reducing memory use.
const MAX_BULK_CLIENTS_PER_CHANNEL: NonZero<usize> = NonZero::new(16).unwrap();

/// The batch size threshold at which a GetPage request will use the bulk stream pool.
///
/// The gRPC initial window size is 64 KB. Each page is 8 KB, so let's avoid increasing the window
/// size for the normal stream pool, and route requests for >= 5 pages (>32 KB) to the bulk pool.
const BULK_THRESHOLD_BATCH_SIZE: usize = 5;

/// The overall request call timeout, including retries and pool acquisition.
/// TODO: should we retry forever? Should the caller decide?
const CALL_TIMEOUT: Duration = Duration::from_secs(60);

/// The per-request (retry attempt) timeout, including any lazy connection establishment.
const REQUEST_TIMEOUT: Duration = Duration::from_secs(10);

/// The initial request retry backoff duration. The first retry does not back off.
/// TODO: use a different backoff for ResourceExhausted (rate limiting)? Needs server support.
const BASE_BACKOFF: Duration = Duration::from_millis(5);

/// The maximum request retry backoff duration.
const MAX_BACKOFF: Duration = Duration::from_secs(5);

/// Threshold and interval for warning about slow operation.
const SLOW_THRESHOLD: Duration = Duration::from_secs(3);

/// A rich Pageserver gRPC client for a single branch. This client is more capable than the
/// basic `page_api::Client` gRPC client, and supports:
///
/// * Pooling of connections, clients, and streams for efficient resource use.
/// * Concurrent use by many callers.
/// * Internal handling of GetPage bidirectional streams.
/// * Automatic retries.
/// * Observability.
///
/// The client has dedicated connection/client/stream pools, for resource reuse. These pools are
/// unbounded: we allow scaling out as many concurrent streams as needed to serve all concurrent
/// callers, which mostly eliminates head-of-line blocking. Idle streams are fairly cheap: the
/// server task currently uses 26 KB of memory, so we can comfortably fit 100,000 concurrent idle
/// streams (2.5 GB memory). The worst case degenerates to the old libpq case with one stream per
/// backend, but without the TCP connection overhead. In the common case we expect significantly
/// lower stream counts due to stream sharing, driven e.g. by idle backends, LFC hits, read
/// coalescing, etc.
///
/// TODO: this client does not support base backups or LSN leases, as these are only used by
/// compute_ctl. Consider adding this.
pub struct PageserverClient {
    /// The branch ID.
    branch_id: BranchId,
    /// The JWT auth token for this branch, if any.
    auth_token: Option<String>,
    /// The compression to use, if any.
    compression: Option<CompressionEncoding>,
    /// The pageserver for this branch.
    shard: ArcSwap<Shard>,
}

impl PageserverClient {
    /// Creates a new Pageserver client for a given branch. Connects to the given gRPC
    /// URL.
    pub fn new(
        branch_id: BranchId,
        url: String,
        auth_token: Option<String>,
        compression: Option<CompressionEncoding>,
    ) -> anyhow::Result<Self> {
        let shard = Shard::new(url, branch_id, auth_token.clone(), compression)?;
        Ok(Self {
            branch_id,
            auth_token,
            compression,
            shard: ArcSwap::new(Arc::new(shard)),
        })
    }

    /// Updates the pageserver to the given gRPC URL. In-flight requests will complete using the
    /// existing pageserver, but may retry with the new one if they fail.
    ///
    /// TODO: verify that in-flight requests are allowed to complete, and that the old pools are
    /// properly spun down and dropped afterwards.
    pub fn update_shards(&self, url: String) -> anyhow::Result<()> {
        let shard = Shard::new(
            url,
            self.branch_id,
            self.auth_token.clone(),
            self.compression,
        )?;
        self.shard.store(Arc::new(shard));
        Ok(())
    }

    /// Returns the total size of a database, as # of bytes.
    #[instrument(skip_all, fields(db_oid=%req.db_oid, lsn=%req.read_lsn))]
    pub async fn get_db_size(
        &self,
        req: page_api::GetDbSizeRequest,
    ) -> tonic::Result<page_api::GetDbSizeResponse> {
        debug!("sending request: {req:?}");
        let resp = Self::with_retries(CALL_TIMEOUT, async |_| {
            let mut client = self.shard.load_full().client().await?;
            Self::with_timeout(REQUEST_TIMEOUT, client.get_db_size(req)).await
        })
        .await?;
        debug!("received response: {resp:?}");
        Ok(resp)
    }

    /// Fetches pages. The `request_id` must be unique across all in-flight requests, and the
    /// `attempt` must be 0 (incremented on retry).
    ///
    /// Unlike `page_api::Client`, this automatically converts `status_code` into `tonic::Status`
    /// errors. All responses will have `GetPageStatusCode::Ok`.
    #[instrument(skip_all, fields(
        req_id = %req.request_id,
        class = %req.request_class,
        rel = %req.rel,
        blkno = %req.block_numbers[0],
        blks = %req.block_numbers.len(),
        lsn = %req.read_lsn,
    ))]
    pub async fn get_page(
        &self,
        req: page_api::GetPageRequest,
    ) -> tonic::Result<page_api::GetPageResponse> {
        // Make sure we have at least one page.
        if req.block_numbers.is_empty() {
            return Err(tonic::Status::invalid_argument("no block number"));
        }
        // The request attempt must be 0. The client will increment it internally.
        if req.request_id.attempt != 0 {
            return Err(tonic::Status::invalid_argument("request attempt must be 0"));
        }

        debug!("sending request: {req:?}");

        // The pageserver may change while we're fetching pages. We execute the request using a
        // stable view of the pageserver, but retry the top-level request to pick up changes.
        let resp = Self::with_retries(CALL_TIMEOUT, async |attempt| {
            let mut req = req.clone();
            req.request_id.attempt = attempt as u32;
            let shard = self.shard.load_full();
            Self::with_timeout(REQUEST_TIMEOUT, Self::get_page_with_shard(req, &shard)).await
        })
        .await?;

        debug!("received response: {resp:?}");
        Ok(resp)
    }

    /// Fetches pages on the given shard. Does not retry internally, but is retried by `get_page()`.
    async fn get_page_with_shard(
        req: page_api::GetPageRequest,
        shard: &Shard,
    ) -> tonic::Result<page_api::GetPageResponse> {
        let mut stream = shard.stream(Self::is_bulk(&req)).await?;
        let resp = stream.send(req.clone()).await?;

        // Convert per-request errors into a tonic::Status.
        if resp.status_code != page_api::GetPageStatusCode::Ok {
            return Err(tonic::Status::new(
                resp.status_code.into(),
                resp.reason.unwrap_or_else(|| String::from("unknown error")),
            ));
        }

        // Check that we received the expected pages.
        if req.rel != resp.rel {
            return Err(tonic::Status::internal(format!(
                "pageserver returned wrong relation, expected {} got {}",
                req.rel, resp.rel
            )));
        }
        if !req
            .block_numbers
            .iter()
            .copied()
            .eq(resp.pages.iter().map(|p| p.block_number))
        {
            return Err(tonic::Status::internal(format!(
                "pageserver returned wrong pages, expected {:?} got {:?}",
                req.block_numbers,
                resp.pages
                    .iter()
                    .map(|page| page.block_number)
                    .collect::<Vec<_>>()
            )));
        }

        Ok(resp)
    }

    /// Returns the size of a relation, as # of blocks.
    #[instrument(skip_all, fields(rel=%req.rel, lsn=%req.read_lsn))]
    pub async fn get_rel_size(
        &self,
        req: page_api::GetRelSizeRequest,
    ) -> tonic::Result<page_api::GetRelSizeResponse> {
        debug!("sending request: {req:?}");
        let resp = Self::with_retries(CALL_TIMEOUT, async |_| {
            let mut client = self.shard.load_full().client().await?;
            Self::with_timeout(REQUEST_TIMEOUT, client.get_rel_size(req)).await
        })
        .await?;
        debug!("received response: {resp:?}");
        Ok(resp)
    }

    /// Fetches an SLRU segment.
    #[instrument(skip_all, fields(kind=%req.kind, segno=%req.segno, lsn=%req.read_lsn))]
    pub async fn get_slru_segment(
        &self,
        req: page_api::GetSlruSegmentRequest,
    ) -> tonic::Result<page_api::GetSlruSegmentResponse> {
        debug!("sending request: {req:?}");
        let resp = Self::with_retries(CALL_TIMEOUT, async |_| {
            let mut client = self.shard.load_full().client().await?;
            Self::with_timeout(REQUEST_TIMEOUT, client.get_slru_segment(req)).await
        })
        .await?;
        debug!("received response: {resp:?}");
        Ok(resp)
    }

    /// Runs the given async closure with retries up to the given timeout. Only certain gRPC status
    /// codes are retried, see [`Retry::should_retry`]. Returns `DeadlineExceeded` on timeout.
    async fn with_retries<T, F, O>(timeout: Duration, f: F) -> tonic::Result<T>
    where
        F: FnMut(usize) -> O, // pass attempt number, starting at 0
        O: Future<Output = tonic::Result<T>>,
    {
        Retry {
            timeout: Some(timeout),
            base_backoff: BASE_BACKOFF,
            max_backoff: MAX_BACKOFF,
        }
        .with(f)
        .await
    }

    /// Runs the given future with a timeout. Returns `DeadlineExceeded` on timeout.
    async fn with_timeout<T>(
        timeout: Duration,
        f: impl Future<Output = tonic::Result<T>>,
    ) -> tonic::Result<T> {
        let started = Instant::now();
        tokio::time::timeout(timeout, f).await.map_err(|_| {
            tonic::Status::deadline_exceeded(format!(
                "request timed out after {:.3}s",
                started.elapsed().as_secs_f64()
            ))
        })?
    }

    /// Returns true if the request is considered a bulk request and should use the bulk pool.
    fn is_bulk(req: &page_api::GetPageRequest) -> bool {
        req.block_numbers.len() >= BULK_THRESHOLD_BATCH_SIZE
    }
}

/// A single pageserver. Has dedicated resource pools with the following structure:
///
/// * Channel pool: MAX_CLIENTS_PER_CHANNEL.
///   * Client pool: unbounded.
///     * Stream pool: unbounded.
/// * Bulk channel pool: MAX_BULK_CLIENTS_PER_CHANNEL.
///   * Bulk client pool: unbounded.
///     * Bulk stream pool: unbounded.
///
/// We use a separate bulk channel pool with a lower concurrency limit for large batch requests.
/// This avoids TCP-level head-of-line blocking, and also concentrates large window sizes on a
/// smaller set of streams/connections, which presumably reduces memory use. Neither of these pools
/// are bounded, nor do they pipeline requests, so the latency characteristics should be mostly
/// similar (except for TCP transmission time).
///
/// TODO: since we never use bounded pools, we could consider removing the pool limiters. However,
/// the code is fairly trivial, so we may as well keep them around for now in case we need them.
struct Shard {
    /// Unary gRPC client pool.
    client_pool: Arc<ClientPool>,
    /// GetPage stream pool.
    stream_pool: Arc<StreamPool>,
    /// GetPage stream pool for bulk requests.
    bulk_stream_pool: Arc<StreamPool>,
}

impl Shard {
    /// Creates a new pageserver connection. It has its own dedicated resource pools. The URL must
    /// use the gRPC scheme.
    fn new(
        url: String,
        branch_id: BranchId,
        auth_token: Option<String>,
        compression: Option<CompressionEncoding>,
    ) -> anyhow::Result<Self> {
        if PageserverProtocol::from_connstring(&url)? != PageserverProtocol::Grpc {
            return Err(anyhow!("invalid pageserver URL {url}: must use gRPC"));
        }

        // Pools for unary requests and non-bulk GetPage requests.
        let client_pool = ClientPool::new(
            ChannelPool::new(url.clone(), MAX_CLIENTS_PER_CHANNEL)?,
            branch_id,
            auth_token.clone(),
            compression,
            None, // unbounded
        );
        let stream_pool = StreamPool::new(client_pool.clone(), None); // unbounded

        // Bulk GetPage stream pool for large batches (prefetches, sequential scans, vacuum, etc.).
        let bulk_stream_pool = StreamPool::new(
            ClientPool::new(
                ChannelPool::new(url, MAX_BULK_CLIENTS_PER_CHANNEL)?,
                branch_id,
                auth_token,
                compression,
                None, // unbounded,
            ),
            None, // unbounded
        );

        Ok(Self {
            client_pool,
            stream_pool,
            bulk_stream_pool,
        })
    }

    /// Returns a pooled client for this pageserver.
    #[instrument(skip_all)]
    async fn client(&self) -> tonic::Result<ClientGuard> {
        warn_slow(
            "client pool acquisition",
            SLOW_THRESHOLD,
            pin!(self.client_pool.get()),
        )
        .await
    }

    /// Returns a pooled stream for this shard. If `bulk` is `true`, uses the dedicated bulk pool.
    #[instrument(skip_all, fields(bulk))]
    async fn stream(&self, bulk: bool) -> tonic::Result<StreamGuard> {
        let pool = match bulk {
            false => &self.stream_pool,
            true => &self.bulk_stream_pool,
        };
        warn_slow("stream pool acquisition", SLOW_THRESHOLD, pin!(pool.get())).await
    }
}
