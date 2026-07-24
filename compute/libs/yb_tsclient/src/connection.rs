//! One multiplexed YB RPC connection.
//!
//! A [`Connection`] owns a TCP stream split into a writer task (frames
//! queued through an mpsc channel, so concurrent callers never interleave
//! partial writes) and a reader/demux task (decodes each response frame
//! and routes it to the in-flight call's oneshot by `call_id`).
//!
//! Any transport failure — connect/read/write error, EOF, corrupt frame —
//! closes the connection: every in-flight and future call on it fails
//! with an [`ErrorClass::Network`](crate::ErrorClass::Network) error, and
//! the session layer above reconnects. Per-call timeouts remove the waiter
//! and leave the connection open (a late response is dropped by the demux
//! when no waiter is registered).
//!
//! Queued-but-unwritten frames are bounded by a per-connection byte budget
//! ([`WRITE_BUDGET_BYTES`]): a stalled tserver blocks the writer task, and
//! without the bound frames (each carrying row payloads) would pile up in
//! the writer channel without limit.

use std::collections::HashMap;
use std::sync::Mutex;
use std::sync::atomic::{AtomicI32, Ordering};
use std::time::Duration;

use bytes::Bytes;
use prost::Message;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;
use tokio::net::tcp::OwnedReadHalf;
use tokio::sync::{OwnedSemaphorePermit, Semaphore, mpsc, oneshot};

use crate::error::YbError;
use crate::framing::{
    CONNECTION_PREAMBLE, MAX_FRAME_LEN, RpcResponse, decode_response_frame, encode_call,
};

/// Per-connection ceiling on queued-but-unwritten frame bytes. A slow or
/// stalled tserver blocks the writer task on `write_all`; without a bound,
/// frames — each carrying the request's row payloads — pile up in the
/// writer channel without limit (the pageserver-side unbounded heap growth
/// observed under YB write-path stalls). When the budget is exhausted,
/// [`Connection::start_call`] fails fast with a `Network` error so the
/// retry driver sheds to another session instead of buffering. Sized above
/// [`MAX_FRAME_LEN`] so a single maximum-size frame can always be queued.
const WRITE_BUDGET_BYTES: usize = 128 << 20;

type CallResult = Result<RpcResponse, YbError>;

/// Registry of in-flight calls; `closed` is set (with the reason) when
/// the transport dies so late registrations fail fast.
#[derive(Default)]
struct Pending {
    closed: Option<String>,
    waiters: HashMap<i32, oneshot::Sender<CallResult>>,
}

pub(crate) struct Connection {
    peer: String,
    /// Each queued frame carries the byte-budget permit it acquired in
    /// `start_call`; the writer task drops the permit once the frame is on
    /// the socket, releasing the budget (RAII — no manual balance to drift).
    writer_tx: mpsc::UnboundedSender<(Bytes, OwnedSemaphorePermit)>,
    /// Byte budget for queued-but-unwritten frames. See [`WRITE_BUDGET_BYTES`].
    write_budget: std::sync::Arc<Semaphore>,
    pending: std::sync::Arc<Mutex<Pending>>,
    next_call_id: AtomicI32,
    reader_task: tokio::task::JoinHandle<()>,
    writer_task: tokio::task::JoinHandle<()>,
}

impl Drop for Connection {
    fn drop(&mut self) {
        self.reader_task.abort();
        self.writer_task.abort();
    }
}

impl Connection {
    /// Connects, sends the `"YB\x01"` preamble, and spawns the demux
    /// tasks.
    pub(crate) async fn connect(addr: &str, timeout: Duration) -> Result<Self, YbError> {
        Self::connect_with_budget(addr, timeout, WRITE_BUDGET_BYTES).await
    }

    /// [`Self::connect`] with an explicit write budget. Split out so tests
    /// can drive the saturation path with a small, deterministic budget.
    async fn connect_with_budget(
        addr: &str,
        timeout: Duration,
        write_budget_bytes: usize,
    ) -> Result<Self, YbError> {
        let mut stream = tokio::time::timeout(timeout, TcpStream::connect(addr))
            .await
            .map_err(|_| YbError::network(format!("connect {addr}: timed out")))?
            .map_err(|e| YbError::network(format!("connect {addr}: {e}")))?;
        stream
            .set_nodelay(true)
            .map_err(|e| YbError::network(format!("set_nodelay {addr}: {e}")))?;
        stream
            .write_all(CONNECTION_PREAMBLE)
            .await
            .map_err(|e| YbError::network(format!("send preamble to {addr}: {e}")))?;

        let (read_half, mut write_half) = stream.into_split();
        let pending = std::sync::Arc::new(Mutex::new(Pending::default()));
        let (writer_tx, mut writer_rx) = mpsc::unbounded_channel::<(Bytes, OwnedSemaphorePermit)>();
        let write_budget = std::sync::Arc::new(Semaphore::new(write_budget_bytes));

        let writer_pending = pending.clone();
        let writer_peer = addr.to_string();
        let writer_task = tokio::spawn(async move {
            while let Some((frame, permit)) = writer_rx.recv().await {
                let n = frame.len();
                if let Err(e) = write_half.write_all(&frame).await {
                    close_all(&writer_pending, format!("write to {writer_peer}: {e}"));
                    return;
                }
                // Frame is on the socket — release its budget so a new call
                // can queue. Dropping the permit is the release.
                drop(permit);
                tracing::trace!(target: "yb_tsclient::wire", peer = %writer_peer, bytes = n, "frame written to socket");
            }
            // Connection dropped; nothing to fail — Drop aborts us anyway.
        });

        let reader_pending = pending.clone();
        let reader_peer = addr.to_string();
        let reader_task = tokio::spawn(async move {
            let reason = read_loop(read_half, &reader_pending, &reader_peer).await;
            close_all(&reader_pending, reason);
        });

        Ok(Connection {
            peer: addr.to_string(),
            writer_tx,
            write_budget,
            pending,
            next_call_id: AtomicI32::new(1),
            reader_task,
            writer_task,
        })
    }

    /// Registers a call and queues its frame, without waiting for the
    /// response. Fully synchronous: a caller that must serialize the
    /// on-wire order of two calls (Perform `serial_no` ordering) can hold
    /// its own lock across this and be certain the frames hit the socket
    /// in that order.
    pub(crate) fn start_call(
        &self,
        service: &str,
        method: &str,
        timeout_millis: u32,
        request: &impl Message,
    ) -> Result<(i32, oneshot::Receiver<CallResult>), YbError> {
        let call_id = self.next_call_id.fetch_add(1, Ordering::Relaxed);
        let frame = encode_call(call_id, service, method, timeout_millis, request)?;
        // Byte-budget backpressure (see WRITE_BUDGET_BYTES). `try_acquire`
        // is synchronous, so start_call stays sync — callers can still hold
        // a lock across it to serialize on-wire order. The permit rides the
        // channel and the writer task releases it once the frame is on the
        // socket; on a stalled connection the budget fills and we fail fast
        // here instead of buffering payloads without bound.
        let permit = match std::sync::Arc::clone(&self.write_budget)
            .try_acquire_many_owned(frame.len() as u32)
        {
            Ok(permit) => permit,
            Err(_) => {
                return Err(YbError::network(format!(
                    "connection to {} write queue saturated ({WRITE_BUDGET_BYTES} B budget); \
                     shedding to another session",
                    self.peer
                )));
            }
        };
        let (tx, rx) = oneshot::channel();
        {
            let mut pending = self.pending.lock().expect("pending lock poisoned");
            if let Some(reason) = &pending.closed {
                // `permit` drops here, releasing the budget it reserved.
                return Err(YbError::network(format!(
                    "connection to {} closed: {reason}",
                    self.peer
                )));
            }
            pending.waiters.insert(call_id, tx);
        }
        if self.writer_tx.send((frame, permit)).is_err() {
            // Send failed: the (frame, permit) is returned in the error and
            // dropped, releasing the budget. Undo the waiter registration.
            self.pending
                .lock()
                .expect("pending lock poisoned")
                .waiters
                .remove(&call_id);
            return Err(YbError::network(format!(
                "connection to {} closed: writer gone",
                self.peer
            )));
        }
        tracing::trace!(target: "yb_tsclient::wire", peer = %self.peer, call_id, method, "call queued to writer");
        Ok((call_id, rx))
    }

    /// Issues one call and waits for its response (or the timeout).
    pub(crate) async fn call(
        &self,
        service: &str,
        method: &str,
        timeout: Duration,
        request: &impl Message,
    ) -> CallResult {
        let timeout_millis = u32::try_from(timeout.as_millis()).unwrap_or(u32::MAX);
        let (call_id, rx) = self.start_call(service, method, timeout_millis, request)?;
        self.await_response(call_id, rx, timeout, service, method)
            .await
    }

    /// Waits on a receiver from [`Self::start_call`] with a deadline.
    pub(crate) async fn await_response(
        &self,
        call_id: i32,
        rx: oneshot::Receiver<CallResult>,
        timeout: Duration,
        service: &str,
        method: &str,
    ) -> CallResult {
        match tokio::time::timeout(timeout, rx).await {
            Ok(Ok(result)) => result,
            // Sender dropped: the connection closed and close_all already
            // fired the waiters — but there's a benign race where the
            // waiter map entry was consumed without a send.
            Ok(Err(_)) => Err(YbError::network(format!(
                "connection to {} closed during {service}.{method}",
                self.peer
            ))),
            Err(_) => {
                // Timed out. Remove our waiter so a response that never
                // arrives (server silent, connection stays open) doesn't leak
                // the oneshot entry in `pending` for the life of the
                // connection. A racing late response either already removed it
                // (read_loop) or now finds it gone — both drop the response,
                // which is correct once we've given up on it.
                self.pending
                    .lock()
                    .expect("pending lock poisoned")
                    .waiters
                    .remove(&call_id);
                Err(YbError::network(format!(
                    "{service}.{method} to {} timed out after {timeout:?}",
                    self.peer
                )))
            }
        }
    }
}

/// Reads and dispatches frames until the transport fails; returns the
/// close reason.
async fn read_loop(mut read_half: OwnedReadHalf, pending: &Mutex<Pending>, peer: &str) -> String {
    loop {
        let total = match read_half.read_u32().await {
            Ok(n) => n as usize,
            Err(e) => return format!("read from {peer}: {e}"),
        };
        // A zero-length frame is YB's RPC-level keepalive on idle
        // connections ("we use them as low level heartbeats for
        // inter-YB RPC connections", binary_call_parser.cc; the
        // server's own parser runs with SkipEmptyMessages::kTrue and
        // sends one every timeout/3). Skip it — treating it as
        // corruption dropped the connection on every idle heartbeat.
        if total == 0 {
            tracing::trace!(target: "yb_tsclient::wire", %peer, "skipped zero-length keepalive frame");
            continue;
        }
        if total > MAX_FRAME_LEN {
            return format!("implausible frame length {total} from {peer}");
        }
        tracing::trace!(target: "yb_tsclient::wire", %peer, bytes = total, "reading response frame body");
        let mut frame = vec![0u8; total];
        if let Err(e) = read_half.read_exact(&mut frame).await {
            return format!("read frame from {peer}: {e}");
        }
        match decode_response_frame(Bytes::from(frame)) {
            Ok(decoded) => {
                let waiter = pending
                    .lock()
                    .expect("pending lock poisoned")
                    .waiters
                    .remove(&decoded.call_id);
                tracing::trace!(target: "yb_tsclient::wire", %peer, call_id = decoded.call_id, matched = waiter.is_some(), "response frame decoded");
                match waiter {
                    // Send failure: the caller gave up (timeout); drop.
                    Some(tx) => drop(tx.send(decoded.result)),
                    None => tracing::debug!(
                        "yb_tsclient: dropping response for unknown call_id {} from {peer} \
                         (caller timed out?)",
                        decoded.call_id
                    ),
                }
            }
            // Framing is byte-synchronous: a corrupt frame means we can
            // no longer trust stream offsets. Kill the connection.
            Err(e) => return format!("corrupt frame from {peer}: {e}"),
        }
    }
}

/// Marks the connection closed and fails every in-flight call.
fn close_all(pending: &Mutex<Pending>, reason: String) {
    let waiters = {
        let mut pending = pending.lock().expect("pending lock poisoned");
        if pending.closed.is_none() {
            pending.closed = Some(reason.clone());
        }
        std::mem::take(&mut pending.waiters)
    };
    if !waiters.is_empty() {
        tracing::warn!(
            "yb_tsclient: connection closed ({reason}); failing {} in-flight calls",
            waiters.len()
        );
    }
    for (_, tx) in waiters {
        let _ = tx.send(Err(YbError::network(format!(
            "connection closed: {reason}"
        ))));
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    /// A zero-length frame is YB's RPC-level idle keepalive
    /// (binary_call_parser.cc, SkipEmptyMessages::kTrue); the reader
    /// must skip it, not treat it as corruption. Regression: the
    /// perf-host tserver sent one every idle heartbeat period and the
    /// client dropped + re-established the connection each time.
    #[tokio::test]
    async fn zero_length_keepalive_frame_is_skipped() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap().to_string();

        let server = tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.unwrap();
            // Absorb the preamble + the call frame (length-prefixed).
            let mut preamble = [0u8; 3];
            stream.read_exact(&mut preamble).await.unwrap();
            assert_eq!(&preamble, CONNECTION_PREAMBLE);
            let call_len = stream.read_u32().await.unwrap() as usize;
            let mut call = vec![0u8; call_len];
            stream.read_exact(&mut call).await.unwrap();
            // Recover the call id from the request header.
            let mut rest = bytes::Bytes::from(call);
            let header_len = prost::encoding::decode_varint(&mut rest).unwrap() as usize;
            let header =
                crate::pb::yb::rpc::RequestHeader::decode(rest.split_to(header_len)).unwrap();
            let call_id = header.call_id.unwrap();

            // Keepalive FIRST: a bare zero length prefix.
            stream.write_u32(0).await.unwrap();

            // Then the real response: [len][varint hdr][ResponseHeader][varint msg][msg].
            let resp_header = crate::pb::yb::rpc::ResponseHeader {
                call_id,
                is_error: Some(false),
                sidecar_offsets: vec![],
                crc: None,
            };
            let body = crate::pb::yb::tserver::PgHeartbeatResponsePb::default();
            let mut inner = bytes::BytesMut::new();
            let mut hdr = bytes::BytesMut::new();
            resp_header.encode(&mut hdr).unwrap();
            prost::encoding::encode_varint(hdr.len() as u64, &mut inner);
            inner.extend_from_slice(&hdr);
            let mut msg = bytes::BytesMut::new();
            body.encode(&mut msg).unwrap();
            prost::encoding::encode_varint(msg.len() as u64, &mut inner);
            inner.extend_from_slice(&msg);
            stream.write_u32(inner.len() as u32).await.unwrap();
            stream.write_all(&inner).await.unwrap();
            // Keep the socket open so the reader doesn't see EOF early.
            tokio::time::sleep(Duration::from_secs(5)).await;
        });

        let conn = Connection::connect(&addr, Duration::from_secs(5))
            .await
            .unwrap();
        let req = crate::pb::yb::tserver::PgHeartbeatRequestPb::default();
        let resp = conn
            .call(
                "yb.tserver.PgClientService",
                "Heartbeat",
                Duration::from_secs(5),
                &req,
            )
            .await
            .expect("call must survive an interleaved zero-length keepalive frame");
        let _ = resp.body; // reaching here (vs connection-drop error) is the assertion
        server.abort();
    }

    /// A stalled tserver blocks the writer task; without a bound, frames
    /// (with payloads) queue without limit. `start_call` must instead fail
    /// fast once the per-connection byte budget is exhausted so the caller
    /// sheds to another session — and recover once the budget is released.
    #[tokio::test]
    async fn start_call_sheds_when_write_budget_exhausted() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap().to_string();

        // Server reads the preamble, then stalls — never reads frames, never
        // responds — holding the socket open so the connection stays warm.
        let server = tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.unwrap();
            let mut preamble = [0u8; 3];
            stream.read_exact(&mut preamble).await.unwrap();
            tokio::time::sleep(Duration::from_secs(5)).await;
        });

        let conn = Connection::connect_with_budget(&addr, Duration::from_secs(5), 4096)
            .await
            .unwrap();

        // Exhaust the whole budget (same-module access to the private field),
        // simulating a writer blocked on a stalled socket with frames queued.
        let hog = std::sync::Arc::clone(&conn.write_budget)
            .try_acquire_many_owned(4096)
            .expect("fresh budget must grant its full allotment");

        let req = crate::pb::yb::tserver::PgHeartbeatRequestPb::default();
        let err = conn
            .start_call("yb.tserver.PgClientService", "Heartbeat", 5000, &req)
            .expect_err("start_call must shed when the write budget is exhausted");
        assert!(
            err.to_string().contains("saturated"),
            "expected a write-queue-saturated error, got: {err}"
        );

        // Releasing the budget lets calls proceed again.
        drop(hog);
        conn.start_call("yb.tserver.PgClientService", "Heartbeat", 5000, &req)
            .expect("start_call must succeed once the budget is released");

        server.abort();
    }
}
