//! YB RPC wire codec (Kudu-derived), verified against
//! `yugabyte-db/src/yb/rpc/{yb_rpc.cc,serialization.cc}` @ 2025.2.3.0.
//!
//! Connection setup: the client sends a 3-byte preamble `"YB\x01"`
//! (`kConnectionHeaderBytes` in `yb_rpc.cc`); no connection-context
//! call or negotiation follows — the server flips straight from
//! NEGOTIATING to OPEN and parses regular calls
//! (`YBInboundConnectionContext::ProcessCalls`).
//!
//! Per-call framing (`serialization.cc`):
//!
//! ```text
//! [u32 BE total_len]                        // excludes these 4 bytes
//! [varint header_len][RequestHeader pb]     // call_id, remote_method, timeout
//! [varint msg_len][msg pb][sidecar bytes]   // msg_len = pb + sidecars
//! ```
//!
//! Responses are framed identically with `ResponseHeader`. The last
//! `msg_len` bytes of the frame are message-pb + sidecars;
//! `sidecar_offsets[i]` in the header are relative to the start of the
//! message pb (offset 0 = first byte after the msg_len varint), so with
//! sidecars present the response pb is `entire[..offsets[0]]` and
//! sidecar `i` spans `offsets[i]..offsets[i+1]` (last: to end).
//! `is_error` responses carry an `ErrorStatusPB` as the message body.
//! CRC fields exist in both headers but are optional; the server never
//! requires them.
//!
//! This module is pure encode/decode; the socket + demultiplexing logic
//! lives in [`crate::connection`].

use bytes::{BufMut, Bytes, BytesMut};
use prost::Message;

use crate::error::{YbError, classify_rpc_error};
use crate::pb::yb::rpc::{ErrorStatusPb, RemoteMethodPb, RequestHeader, ResponseHeader};

/// `kConnectionHeaderBytes` from `yb/rpc/yb_rpc.cc`.
pub(crate) const CONNECTION_PREAMBLE: &[u8] = b"YB\x01";

/// Upper bound on accepted frame sizes; anything larger means we lost
/// framing sync (or got a corrupt/hostile length prefix) and the
/// connection must be dropped. Kept well above any legitimate hot-path
/// response (page chains and WAL batches are a few MiB at most) but low
/// enough that a bogus prefix can't drive a huge allocation.
pub(crate) const MAX_FRAME_LEN: usize = 64 << 20;

/// Decoded response payload: the message protobuf bytes plus any sidecars.
#[derive(Debug)]
pub struct RpcResponse {
    pub body: Bytes,
    pub sidecars: Vec<Bytes>,
}

/// One decoded response frame, routed by `call_id`.
pub(crate) struct DecodedFrame {
    pub call_id: i32,
    /// Per-call outcome: an `is_error` frame becomes a classified error.
    pub result: Result<RpcResponse, YbError>,
}

/// Encodes one call frame (including the length prefix).
pub(crate) fn encode_call(
    call_id: i32,
    service: &str,
    method: &str,
    timeout_millis: u32,
    request: &impl Message,
) -> Result<Bytes, YbError> {
    let header = RequestHeader {
        call_id: Some(call_id),
        remote_method: Some(RemoteMethodPb {
            service_name: service.to_string(),
            method_name: method.to_string(),
        }),
        timeout_millis: Some(timeout_millis),
        ..Default::default()
    };

    let header_len = header.encoded_len();
    let body_len = request.encoded_len();
    let total =
        varint_len(header_len as u64) + header_len + varint_len(body_len as u64) + body_len;

    let mut buf = BytesMut::with_capacity(4 + total);
    buf.put_u32(
        u32::try_from(total).map_err(|_| YbError::invalid(format!("frame too large: {total}")))?,
    );
    prost::encoding::encode_varint(header_len as u64, &mut buf);
    header
        .encode(&mut buf)
        .map_err(|e| YbError::invalid(format!("encode RequestHeader: {e}")))?;
    prost::encoding::encode_varint(body_len as u64, &mut buf);
    request
        .encode(&mut buf)
        .map_err(|e| YbError::invalid(format!("encode {service}.{method} request: {e}")))?;
    Ok(buf.freeze())
}

/// Decodes one response frame (without the u32 length prefix).
///
/// The outer `Err` means the frame itself is corrupt (framing
/// desynchronized) and the connection must be torn down; a per-call
/// failure (`is_error` frame) comes back as `Ok` with an `Err` result so
/// it can be routed to its waiter.
pub(crate) fn decode_response_frame(mut frame: Bytes) -> Result<DecodedFrame, YbError> {
    let header_len = prost::encoding::decode_varint(&mut frame)
        .map_err(|e| YbError::decode(format!("response header length varint: {e}")))?
        as usize;
    if frame.len() < header_len {
        return Err(YbError::decode(format!(
            "truncated response header: {} bytes, header_len {header_len}",
            frame.len()
        )));
    }
    let header = ResponseHeader::decode(frame.split_to(header_len))
        .map_err(|e| YbError::decode(format!("decode ResponseHeader: {e}")))?;
    let call_id = header.call_id;

    let msg_len = prost::encoding::decode_varint(&mut frame)
        .map_err(|e| YbError::decode(format!("response msg length varint: {e}")))?
        as usize;
    if frame.len() != msg_len {
        return Err(YbError::decode(format!(
            "frame has {} bytes after headers, msg_len says {msg_len}",
            frame.len()
        )));
    }

    // `sidecar_offsets` are relative to the start of the message pb.
    let (body, sidecars) = if header.sidecar_offsets.is_empty() {
        (frame, Vec::new())
    } else {
        let offsets = &header.sidecar_offsets;
        let mut sidecars = Vec::with_capacity(offsets.len());
        for (i, &start) in offsets.iter().enumerate() {
            let start = start as usize;
            let end = offsets.get(i + 1).map_or(frame.len(), |&e| e as usize);
            if start > end || end > frame.len() {
                return Err(YbError::decode(format!(
                    "sidecar {i} range {start}..{end} outside message of {} bytes",
                    frame.len()
                )));
            }
            sidecars.push(frame.slice(start..end));
        }
        (frame.slice(..offsets[0] as usize), sidecars)
    };

    let result = if header.is_error() {
        match ErrorStatusPb::decode(body) {
            Ok(err) => Err(classify_rpc_error(err.code(), &err.message)),
            Err(e) => Err(YbError::decode(format!("decode ErrorStatusPB: {e}"))),
        }
    } else {
        Ok(RpcResponse { body, sidecars })
    };
    Ok(DecodedFrame { call_id, result })
}

fn varint_len(value: u64) -> usize {
    prost::encoding::encoded_len_varint(value)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::error::ErrorClass;
    use crate::pb::yb::rpc::error_status_pb::RpcErrorCodePb;
    use crate::pb::yb::tserver::PgHeartbeatRequestPb;

    /// Builds a response frame the way the server does
    /// (`serialization.cc` DoSerializeResponse): header, then a msg_len
    /// covering pb + sidecars, with sidecar offsets in the header.
    fn encode_response_frame(
        call_id: i32,
        is_error: bool,
        body: &impl Message,
        sidecars: &[&[u8]],
    ) -> Bytes {
        let body_len = body.encoded_len();
        let mut offsets = Vec::new();
        let mut pos = body_len as u32;
        for s in sidecars {
            offsets.push(pos);
            pos += s.len() as u32;
        }
        let header = ResponseHeader {
            call_id,
            is_error: Some(is_error),
            sidecar_offsets: offsets,
            crc: None,
        };
        let header_len = header.encoded_len();
        let msg_len = body_len + sidecars.iter().map(|s| s.len()).sum::<usize>();
        let mut buf = BytesMut::new();
        prost::encoding::encode_varint(header_len as u64, &mut buf);
        header.encode(&mut buf).unwrap();
        prost::encoding::encode_varint(msg_len as u64, &mut buf);
        body.encode(&mut buf).unwrap();
        for s in sidecars {
            buf.put_slice(s);
        }
        buf.freeze()
    }

    #[test]
    fn call_frame_round_trip() {
        let req = PgHeartbeatRequestPb {
            session_id: 7,
            pid: 42,
            cluster_config_version: 0,
        };
        let frame = encode_call(3, "yb.tserver.PgClientService", "Heartbeat", 5000, &req).unwrap();

        // Outer length prefix covers the rest exactly.
        let total = u32::from_be_bytes(frame[..4].try_into().unwrap()) as usize;
        assert_eq!(total, frame.len() - 4);

        let mut rest = frame.slice(4..);
        let header_len = prost::encoding::decode_varint(&mut rest).unwrap() as usize;
        let header = RequestHeader::decode(rest.split_to(header_len)).unwrap();
        assert_eq!(header.call_id, Some(3));
        assert_eq!(header.timeout_millis, Some(5000));
        let rm = header.remote_method.unwrap();
        assert_eq!(rm.service_name, "yb.tserver.PgClientService");
        assert_eq!(rm.method_name, "Heartbeat");

        let msg_len = prost::encoding::decode_varint(&mut rest).unwrap() as usize;
        assert_eq!(msg_len, rest.len());
        let decoded = PgHeartbeatRequestPb::decode(rest).unwrap();
        assert_eq!(decoded.session_id, 7);
        assert_eq!(decoded.pid, 42);
    }

    #[test]
    fn response_frame_round_trip_no_sidecars() {
        let body = PgHeartbeatRequestPb {
            session_id: 99,
            pid: 1,
            cluster_config_version: 0,
        };
        let frame = encode_response_frame(11, false, &body, &[]);
        let decoded = decode_response_frame(frame).unwrap();
        assert_eq!(decoded.call_id, 11);
        let resp = decoded.result.unwrap();
        assert!(resp.sidecars.is_empty());
        let round = PgHeartbeatRequestPb::decode(resp.body).unwrap();
        assert_eq!(round.session_id, 99);
    }

    #[test]
    fn response_frame_sidecar_slicing() {
        let body = PgHeartbeatRequestPb {
            session_id: 1,
            pid: 2,
            cluster_config_version: 3,
        };
        let s0: &[u8] = b"first-sidecar";
        let s1: &[u8] = b"";
        let s2: &[u8] = &[0xde, 0xad, 0xbe, 0xef];
        let frame = encode_response_frame(5, false, &body, &[s0, s1, s2]);
        let decoded = decode_response_frame(frame).unwrap();
        assert_eq!(decoded.call_id, 5);
        let resp = decoded.result.unwrap();
        assert_eq!(resp.sidecars.len(), 3);
        assert_eq!(resp.sidecars[0].as_ref(), s0);
        assert_eq!(resp.sidecars[1].as_ref(), s1);
        assert_eq!(resp.sidecars[2].as_ref(), s2);
        // The body pb still decodes: it ends at the first sidecar offset.
        let round = PgHeartbeatRequestPb::decode(resp.body).unwrap();
        assert_eq!(round.cluster_config_version, 3);
    }

    #[test]
    fn error_frame_becomes_classified_error() {
        let err = ErrorStatusPb {
            message: "server is shutting down".to_string(),
            code: Some(RpcErrorCodePb::FatalServerShuttingDown as i32),
        };
        let frame = encode_response_frame(8, true, &err, &[]);
        let decoded = decode_response_frame(frame).unwrap();
        assert_eq!(decoded.call_id, 8);
        let e = decoded.result.unwrap_err();
        assert_eq!(e.class(), ErrorClass::Network);
        assert!(e.to_string().contains("shutting down"));

        let busy = ErrorStatusPb {
            message: "busy".to_string(),
            code: Some(RpcErrorCodePb::ErrorServerTooBusy as i32),
        };
        let frame = encode_response_frame(9, true, &busy, &[]);
        let e = decode_response_frame(frame).unwrap().result.unwrap_err();
        assert_eq!(e.class(), ErrorClass::TryAgain);
    }

    #[test]
    fn corrupt_frames_are_connection_fatal() {
        // Truncated header.
        let mut buf = BytesMut::new();
        prost::encoding::encode_varint(100, &mut buf);
        buf.put_slice(&[1, 2, 3]);
        assert!(decode_response_frame(buf.freeze()).is_err());

        // msg_len mismatch.
        let body = PgHeartbeatRequestPb::default();
        let good = encode_response_frame(1, false, &body, &[]);
        let mut bad = BytesMut::from(&good[..]);
        bad.put_u8(0xff); // trailing garbage the msg_len doesn't cover
        assert!(decode_response_frame(bad.freeze()).is_err());

        // Sidecar offset outside the message.
        let header = ResponseHeader {
            call_id: 2,
            is_error: Some(false),
            sidecar_offsets: vec![50],
            crc: None,
        };
        let mut buf = BytesMut::new();
        prost::encoding::encode_varint(header.encoded_len() as u64, &mut buf);
        header.encode(&mut buf).unwrap();
        prost::encoding::encode_varint(4, &mut buf);
        buf.put_slice(&[0, 0, 0, 0]);
        assert!(decode_response_frame(buf.freeze()).is_err());
    }
}
