use std::collections::BTreeMap;
use std::sync::{Arc, RwLock};

use itertools::Itertools;
use tokio_epoll_uring::{BoundedBuf, IoBufMut, Slice};

use crate::assert_u64_eq_usize::{U64IsUsize, UsizeIsU64};
use crate::context::RequestContext;
use crate::virtual_file::IoBufferMut;
use crate::virtual_file::owned_buffers_io::io_buf_aligned::IoBufAlignedMut;

/// The file interface we require. At runtime, this is a [`crate::tenant::ephemeral_file::EphemeralFile`].
pub trait File: Send {
    /// Attempt to read the bytes in `self` in range `[start,start+dst.bytes_total())`
    /// and return the number of bytes read (let's call it `nread`).
    /// The bytes read are placed in `dst`, i.e., `&dst[..nread]` will contain the read bytes.
    ///
    /// The only reason why the read may be short (i.e., `nread != dst.bytes_total()`)
    /// is if the file is shorter than `start+dst.len()`.
    ///
    /// This is unlike [`std::os::unix::fs::FileExt::read_exact_at`] which returns an
    /// [`std::io::ErrorKind::UnexpectedEof`] error if the file is shorter than `start+dst.len()`.
    ///
    /// No guarantees are made about the remaining bytes in `dst` in case of a short read.
    async fn read_exact_at_eof_ok<B: IoBufAlignedMut + Send>(
        &self,
        start: u64,
        dst: Slice<B>,
        ctx: &RequestContext,
    ) -> std::io::Result<(Slice<B>, usize)>;
}

/// A logical read from [`File`]. See [`Self::new`].
pub struct LogicalRead<B: Buffer> {
    pos: u64,
    state: RwLockRefCell<LogicalReadState<B>>,
}

enum LogicalReadState<B: Buffer> {
    NotStarted(B),
    Ongoing(B),
    Ok(B),
    Error(Arc<std::io::Error>),
    Undefined,
}

impl<B: Buffer> LogicalRead<B> {
    /// Create a new [`LogicalRead`] from [`File`] of the data in the file in range `[ pos, pos + buf.cap() )`.
    pub fn new(pos: u64, buf: B) -> Self {
        Self {
            pos,
            state: RwLockRefCell::new(LogicalReadState::NotStarted(buf)),
        }
    }
    pub fn into_result(self) -> Option<Result<B, Arc<std::io::Error>>> {
        match self.state.into_inner() {
            LogicalReadState::Ok(buf) => Some(Ok(buf)),
            LogicalReadState::Error(e) => Some(Err(e)),
            LogicalReadState::NotStarted(_) | LogicalReadState::Ongoing(_) => None,
            LogicalReadState::Undefined => unreachable!(),
        }
    }
}

/// The buffer into which a [`LogicalRead`] result is placed.
pub trait Buffer: std::ops::Deref<Target = [u8]> {
    /// Immutable.
    fn cap(&self) -> usize;
    /// Changes only through [`Self::extend_from_slice`].
    fn len(&self) -> usize;
    /// Panics if the total length would exceed the initialized capacity.
    fn extend_from_slice(&mut self, src: &[u8]);
}

/// The minimum alignment and size requirement for disk offsets and memory buffer size for direct IO.
const DIO_CHUNK_SIZE: usize = crate::virtual_file::get_io_buffer_alignment();

/// If multiple chunks need to be read, merge adjacent chunk reads into batches of max size `MAX_CHUNK_BATCH_SIZE`.
/// (The unit is the number of chunks.)
const MAX_CHUNK_BATCH_SIZE: usize = {
    let desired = 128 * 1024; // 128k
    if desired % DIO_CHUNK_SIZE != 0 {
        panic!("MAX_CHUNK_BATCH_SIZE must be a multiple of DIO_CHUNK_SIZE")
        // compile-time error
    }
    desired / DIO_CHUNK_SIZE
};

/// Execute the given logical `reads` against `file`.
/// The results are placed in the buffers of the [`LogicalRead`]s.
/// Retrieve the results by calling [`LogicalRead::into_result`] on each [`LogicalRead`].
///
/// The [`LogicalRead`]s must be freshly created using [`LogicalRead::new`] when calling this function.
/// Otherwise, this function panics.
pub async fn execute<'a, I, F, B>(file: &F, reads: I, ctx: &RequestContext)
where
    I: IntoIterator<Item = &'a LogicalRead<B>>,
    F: File,
    B: Buffer + IoBufMut + Send,
{
    // Terminology:
    // logical read = a request to read an arbitrary range of bytes from `file`; byte-level granularity
    // chunk = we conceptually divide up the byte range of `file` into DIO_CHUNK_SIZEs ranges
    // interest = a range within a chunk that a logical read is interested in; one logical read gets turned into many interests
    // physical read = the read request we're going to issue to the OS; covers a range of chunks; chunk-level granularity

    // Preserve a copy of the logical reads for debug assertions at the end
    #[cfg(debug_assertions)]
    let (reads, assert_logical_reads) = {
        let (reads, assert) = reads.into_iter().tee();
        (reads, Some(Vec::from_iter(assert)))
    };
    #[cfg(not(debug_assertions))]
    let (reads, assert_logical_reads): (_, Option<Vec<&'a LogicalRead<B>>>) = (reads, None);

    // Plan which parts of which chunks need to be appended to which buffer
    let mut by_chunk: BTreeMap<u64, Vec<Interest<B>>> = BTreeMap::new();
    struct Interest<'a, B: Buffer> {
        logical_read: &'a LogicalRead<B>,
        offset_in_chunk: u64,
        len: u64,
    }
    for logical_read in reads {
        let LogicalRead { pos, state } = logical_read;
        let mut state = state.borrow_mut();

        // transition from NotStarted to Ongoing
        let cur = std::mem::replace(&mut *state, LogicalReadState::Undefined);
        let req_len = match cur {
            LogicalReadState::NotStarted(buf) => {
                if buf.len() != 0 {
                    panic!(
                        "The `LogicalRead`s that are passed in must be freshly created using `LogicalRead::new`"
                    );
                }
                // buf.cap() == 0 is ok

                // transition into Ongoing state
                let req_len = buf.cap();
                *state = LogicalReadState::Ongoing(buf);
                req_len
            }
            x => panic!(
                "must only call with fresh LogicalReads, got another state, leaving Undefined state behind state={x:?}"
            ),
        };

        // plan which chunks we need to read from
        let mut remaining = req_len;
        let mut chunk_no = *pos / (DIO_CHUNK_SIZE.into_u64());
        let mut offset_in_chunk = pos.into_usize() % DIO_CHUNK_SIZE;
        while remaining > 0 {
            let remaining_in_chunk = std::cmp::min(remaining, DIO_CHUNK_SIZE - offset_in_chunk);
            by_chunk.entry(chunk_no).or_default().push(Interest {
                logical_read,
                offset_in_chunk: offset_in_chunk.into_u64(),
                len: remaining_in_chunk.into_u64(),
            });
            offset_in_chunk = 0;
            chunk_no += 1;
            remaining -= remaining_in_chunk;
        }
    }

    // At this point, we could iterate over by_chunk, in chunk order,
    // read each chunk from disk, and fill the buffers.
    // However, we can merge adjacent chunks into batches of MAX_CHUNK_BATCH_SIZE
    // so we issue fewer IOs = fewer roundtrips = lower overall latency.
    struct PhysicalRead<'a, B: Buffer> {
        start_chunk_no: u64,
        nchunks: usize,
        dsts: Vec<PhysicalInterest<'a, B>>,
    }
    struct PhysicalInterest<'a, B: Buffer> {
        logical_read: &'a LogicalRead<B>,
        offset_in_physical_read: u64,
        len: u64,
    }
    let mut physical_reads: Vec<PhysicalRead<B>> = Vec::new();
    let mut by_chunk = by_chunk.into_iter().peekable();
    loop {
        let mut last_chunk_no = None;
        let to_merge: Vec<(u64, Vec<Interest<B>>)> = by_chunk
            .peeking_take_while(|(chunk_no, _)| {
                if let Some(last_chunk_no) = last_chunk_no {
                    if *chunk_no != last_chunk_no + 1 {
                        return false;
                    }
                }
                last_chunk_no = Some(*chunk_no);
                true
            })
            .take(MAX_CHUNK_BATCH_SIZE)
            .collect(); // TODO: avoid this .collect()
        let Some(start_chunk_no) = to_merge.first().map(|(chunk_no, _)| *chunk_no) else {
            break;
        };
        let nchunks = to_merge.len();
        let dsts = to_merge
            .into_iter()
            .enumerate()
            .flat_map(|(i, (_, dsts))| {
                dsts.into_iter().map(
                    move |Interest {
                              logical_read,
                              offset_in_chunk,
                              len,
                          }| {
                        PhysicalInterest {
                            logical_read,
                            offset_in_physical_read: i
                                .checked_mul(DIO_CHUNK_SIZE)
                                .unwrap()
                                .into_u64()
                                + offset_in_chunk,
                            len,
                        }
                    },
                )
            })
            .collect();
        physical_reads.push(PhysicalRead {
            start_chunk_no,
            nchunks,
            dsts,
        });
    }
    drop(by_chunk);

    // Execute physical reads and fill the logical read buffers
    // TODO: pipelined reads; prefetch;
    let get_io_buffer = |nchunks| IoBufferMut::with_capacity(nchunks * DIO_CHUNK_SIZE);
    for PhysicalRead {
        start_chunk_no,
        nchunks,
        dsts,
    } in physical_reads
    {
        let all_done = dsts
            .iter()
            .all(|PhysicalInterest { logical_read, .. }| logical_read.state.borrow().is_terminal());
        if all_done {
            continue;
        }
        let read_offset = start_chunk_no
            .checked_mul(DIO_CHUNK_SIZE.into_u64())
            .expect("we produce chunk_nos by dividing by DIO_CHUNK_SIZE earlier");
        let io_buf = get_io_buffer(nchunks).slice_full();
        let req_len = io_buf.len();
        let (io_buf_slice, nread) = match file.read_exact_at_eof_ok(read_offset, io_buf, ctx).await
        {
            Ok(t) => t,
            Err(e) => {
                let e = Arc::new(e);
                for PhysicalInterest { logical_read, .. } in dsts {
                    *logical_read.state.borrow_mut() = LogicalReadState::Error(Arc::clone(&e));
                    // this will make later reads for the given LogicalRead short-circuit, see top of loop body
                }
                continue;
            }
        };
        let io_buf = io_buf_slice.into_inner();
        assert!(
            nread <= io_buf.len(),
            "the last chunk in the file can be a short read, so, no =="
        );
        let io_buf = &io_buf[..nread];
        for PhysicalInterest {
            logical_read,
            offset_in_physical_read,
            len,
        } in dsts
        {
            let mut logical_read_state_borrow = logical_read.state.borrow_mut();
            let logical_read_buf = match &mut *logical_read_state_borrow {
                LogicalReadState::NotStarted(_) => {
                    unreachable!("we transition it into Ongoing at function entry")
                }
                LogicalReadState::Ongoing(buf) => buf,
                LogicalReadState::Ok(_) | LogicalReadState::Error(_) => {
                    continue;
                }
                LogicalReadState::Undefined => unreachable!(),
            };
            let range_in_io_buf = std::ops::Range {
                start: offset_in_physical_read as usize,
                end: offset_in_physical_read as usize + len as usize,
            };
            assert!(range_in_io_buf.end >= range_in_io_buf.start);
            if range_in_io_buf.end > nread {
                let msg = format!(
                    "physical read returned EOF where this logical read expected more data in the file: offset=0x{read_offset:x} req_len=0x{req_len:x} nread=0x{nread:x} {:?}",
                    &*logical_read_state_borrow
                );
                logical_read_state_borrow.transition_to_terminal(Err(std::io::Error::new(
                    std::io::ErrorKind::UnexpectedEof,
                    msg,
                )));
                continue;
            }
            let data = &io_buf[range_in_io_buf];

            // Copy data from io buffer into the logical read buffer.
            // (And in debug mode, validate that the buffer impl adheres to the Buffer trait spec.)
            let pre = if cfg!(debug_assertions) {
                Some((logical_read_buf.len(), logical_read_buf.cap()))
            } else {
                None
            };
            logical_read_buf.extend_from_slice(data);
            let post = if cfg!(debug_assertions) {
                Some((logical_read_buf.len(), logical_read_buf.cap()))
            } else {
                None
            };
            match (pre, post) {
                (None, None) => {}
                (Some(_), None) | (None, Some(_)) => unreachable!(),
                (Some((pre_len, pre_cap)), Some((post_len, post_cap))) => {
                    assert_eq!(pre_len + len as usize, post_len);
                    assert_eq!(pre_cap, post_cap);
                }
            }

            if logical_read_buf.len() == logical_read_buf.cap() {
                logical_read_state_borrow.transition_to_terminal(Ok(()));
            }
        }
    }

    if let Some(assert_logical_reads) = assert_logical_reads {
        for logical_read in assert_logical_reads {
            assert!(logical_read.state.borrow().is_terminal());
        }
    }
}

impl<B: Buffer> LogicalReadState<B> {
    fn is_terminal(&self) -> bool {
        match self {
            LogicalReadState::NotStarted(_) | LogicalReadState::Ongoing(_) => false,
            LogicalReadState::Ok(_) | LogicalReadState::Error(_) => true,
            LogicalReadState::Undefined => unreachable!(),
        }
    }
    fn transition_to_terminal(&mut self, err: std::io::Result<()>) {
        let cur = std::mem::replace(self, LogicalReadState::Undefined);
        let buf = match cur {
            LogicalReadState::Ongoing(buf) => buf,
            x => panic!("must only call in state Ongoing, got {x:?}"),
        };
        *self = match err {
            Ok(()) => LogicalReadState::Ok(buf),
            Err(e) => LogicalReadState::Error(Arc::new(e)),
        };
    }
}

impl<B: Buffer> std::fmt::Debug for LogicalReadState<B> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        #[derive(Debug)]
        #[allow(unused)]
        struct BufferDebug {
            len: usize,
            cap: usize,
        }
        impl<'a> From<&'a dyn Buffer> for BufferDebug {
            fn from(buf: &'a dyn Buffer) -> Self {
                Self {
                    len: buf.len(),
                    cap: buf.cap(),
                }
            }
        }
        match self {
            LogicalReadState::NotStarted(b) => {
                write!(f, "NotStarted({:?})", BufferDebug::from(b as &dyn Buffer))
            }
            LogicalReadState::Ongoing(b) => {
                write!(f, "Ongoing({:?})", BufferDebug::from(b as &dyn Buffer))
            }
            LogicalReadState::Ok(b) => write!(f, "Ok({:?})", BufferDebug::from(b as &dyn Buffer)),
            LogicalReadState::Error(e) => write!(f, "Error({e:?})"),
            LogicalReadState::Undefined => write!(f, "Undefined"),
        }
    }
}

#[derive(Debug)]
struct RwLockRefCell<T>(RwLock<T>);
impl<T> RwLockRefCell<T> {
    fn new(value: T) -> Self {
        Self(RwLock::new(value))
    }
    fn borrow(&self) -> impl std::ops::Deref<Target = T> + '_ {
        self.0.try_read().unwrap()
    }
    fn borrow_mut(&self) -> impl std::ops::DerefMut<Target = T> + '_ {
        self.0.try_write().unwrap()
    }
    fn into_inner(self) -> T {
        self.0.into_inner().unwrap()
    }
}

impl Buffer for Vec<u8> {
    fn cap(&self) -> usize {
        self.capacity()
    }

    fn len(&self) -> usize {
        self.len()
    }

    fn extend_from_slice(&mut self, src: &[u8]) {
        if self.len() + src.len() > self.cap() {
            panic!("Buffer capacity exceeded");
        }
        Vec::extend_from_slice(self, src);
    }
}

