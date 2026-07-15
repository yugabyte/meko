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
//! the session layer above reconnects. Per-call timeouts abandon the
//! waiter but leave the connection open (the late response is dropped by
//! the demux when no waiter is registered).

use std::collections::HashMap;
use std::sync::Mutex;
use std::sync::atomic::{AtomicI32, Ordering};
use std::time::Duration;

use bytes::Bytes;
use prost::Message;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;
use tokio::net::tcp::OwnedReadHalf;
use tokio::sync::{mpsc, oneshot};

use crate::error::YbError;
use crate::framing::{
    CONNECTION_PREAMBLE, MAX_FRAME_LEN, RpcResponse, decode_response_frame, encode_call,
};

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
    writer_tx: mpsc::UnboundedSender<Bytes>,
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
        let (writer_tx, mut writer_rx) = mpsc::unbounded_channel::<Bytes>();

        let writer_pending = pending.clone();
        let writer_peer = addr.to_string();
        let writer_task = tokio::spawn(async move {
            while let Some(frame) = writer_rx.recv().await {
                let n = frame.len();
                if let Err(e) = write_half.write_all(&frame).await {
                    close_all(&writer_pending, format!("write to {writer_peer}: {e}"));
                    return;
                }
                tracing::trace!(target: "yb_pgclient::wire", peer = %writer_peer, bytes = n, "frame written to socket");
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
    ) -> Result<oneshot::Receiver<CallResult>, YbError> {
        let call_id = self.next_call_id.fetch_add(1, Ordering::Relaxed);
        let frame = encode_call(call_id, service, method, timeout_millis, request)?;
        let (tx, rx) = oneshot::channel();
        {
            let mut pending = self.pending.lock().expect("pending lock poisoned");
            if let Some(reason) = &pending.closed {
                return Err(YbError::network(format!(
                    "connection to {} closed: {reason}",
                    self.peer
                )));
            }
            pending.waiters.insert(call_id, tx);
        }
        if self.writer_tx.send(frame).is_err() {
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
        tracing::trace!(target: "yb_pgclient::wire", peer = %self.peer, call_id, method, "call queued to writer");
        Ok(rx)
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
        let rx = self.start_call(service, method, timeout_millis, request)?;
        self.await_response(rx, timeout, service, method).await
    }

    /// Waits on a receiver from [`Self::start_call`] with a deadline.
    pub(crate) async fn await_response(
        &self,
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
            Err(_) => Err(YbError::network(format!(
                "{service}.{method} to {} timed out after {timeout:?}",
                self.peer
            ))),
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
            tracing::trace!(target: "yb_pgclient::wire", %peer, "skipped zero-length keepalive frame");
            continue;
        }
        if total > MAX_FRAME_LEN {
            return format!("implausible frame length {total} from {peer}");
        }
        tracing::trace!(target: "yb_pgclient::wire", %peer, bytes = total, "reading response frame body");
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
                tracing::trace!(target: "yb_pgclient::wire", %peer, call_id = decoded.call_id, matched = waiter.is_some(), "response frame decoded");
                match waiter {
                    // Send failure: the caller gave up (timeout); drop.
                    Some(tx) => drop(tx.send(decoded.result)),
                    None => tracing::debug!(
                        "yb_pgclient: dropping response for unknown call_id {} from {peer} \
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
            "yb_pgclient: connection closed ({reason}); failing {} in-flight calls",
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
}
