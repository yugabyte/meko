//! Adapter that lets `tokio-postgres-rustls` plug into the
//! `yb-tokio-postgres` smart driver.
//!
//! The YB rust-postgres fork (`yb-tokio-postgres`) defines its own
//! copy of the `MakeTlsConnect` / `TlsConnect` / `TlsStream` traits
//! in its own crate. `tokio-postgres-rustls`'s `MakeRustlsConnect`
//! implements the **upstream** `tokio_postgres::tls::MakeTlsConnect`
//! — same trait shape, different crate, so the impl doesn't carry
//! across the boundary. This module bridges that gap with three
//! thin newtypes that forward to the upstream impls.
//!
//! Why we need this at all: the YB-shipped TLS connector
//! (`yb-postgres-openssl 0.5.0-yb-1-beta.3`) fails the TLS handshake
//! against YB tservers in both dynamic-openssl-1.1 and
//! statically-vendored-openssl-3.6.2 builds, while
//! `tokio-postgres-rustls` (pure-Rust / ring) connects fine to the
//! same target. We swapped back to rustls but kept the YB smart
//! driver — this adapter is the bridge.

use std::future::Future;
use std::io;
use std::pin::Pin;
use std::task::{Context, Poll};

use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};

/// Wraps [`tokio_postgres_rustls::MakeRustlsConnect`] so it
/// satisfies `yb_tokio_postgres::tls::MakeTlsConnect`.
#[derive(Clone)]
pub struct YbMakeRustlsConnect(pub tokio_postgres_rustls::MakeRustlsConnect);

impl YbMakeRustlsConnect {
    pub fn new(inner: tokio_postgres_rustls::MakeRustlsConnect) -> Self {
        Self(inner)
    }
}

impl<S> yb_tokio_postgres::tls::MakeTlsConnect<S> for YbMakeRustlsConnect
where
    S: AsyncRead + AsyncWrite + Unpin + Send + 'static,
{
    type Stream = YbRustlsStream<S>;
    type TlsConnect = YbRustlsConnect;
    type Error = rustls::pki_types::InvalidDnsNameError;

    fn make_tls_connect(&mut self, hostname: &str) -> Result<Self::TlsConnect, Self::Error> {
        let inner = <tokio_postgres_rustls::MakeRustlsConnect as tokio_postgres::tls::MakeTlsConnect<S>>::make_tls_connect(
            &mut self.0,
            hostname,
        )?;
        Ok(YbRustlsConnect(inner))
    }
}

pub struct YbRustlsConnect(tokio_postgres_rustls::RustlsConnect);

impl<S> yb_tokio_postgres::tls::TlsConnect<S> for YbRustlsConnect
where
    S: AsyncRead + AsyncWrite + Unpin + Send + 'static,
{
    type Stream = YbRustlsStream<S>;
    type Error = io::Error;
    type Future = Pin<Box<dyn Future<Output = io::Result<YbRustlsStream<S>>> + Send>>;

    fn connect(self, stream: S) -> Self::Future {
        let fut = <tokio_postgres_rustls::RustlsConnect as tokio_postgres::tls::TlsConnect<S>>::connect(
            self.0,
            stream,
        );
        Box::pin(async move { fut.await.map(YbRustlsStream) })
    }
}

pub struct YbRustlsStream<S>(tokio_postgres_rustls::RustlsStream<S>);

impl<S> AsyncRead for YbRustlsStream<S>
where
    S: AsyncRead + AsyncWrite + Unpin,
{
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        Pin::new(&mut self.0).poll_read(cx, buf)
    }
}

impl<S> AsyncWrite for YbRustlsStream<S>
where
    S: AsyncRead + AsyncWrite + Unpin,
{
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<io::Result<usize>> {
        Pin::new(&mut self.0).poll_write(cx, buf)
    }

    fn poll_flush(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.0).poll_flush(cx)
    }

    fn poll_shutdown(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.0).poll_shutdown(cx)
    }
}

impl<S> yb_tokio_postgres::tls::TlsStream for YbRustlsStream<S>
where
    S: AsyncRead + AsyncWrite + Unpin,
{
    fn channel_binding(&self) -> yb_tokio_postgres::tls::ChannelBinding {
        // The upstream `tokio_postgres::tls::ChannelBinding` has
        // private fields with no extractor, so we can't forward the
        // server-end-point bytes from the upstream stream. Returning
        // `none()` makes SCRAM negotiation degrade gracefully from
        // SCRAM-SHA-256-PLUS to SCRAM-SHA-256 — fine for YB password
        // auth.
        yb_tokio_postgres::tls::ChannelBinding::none()
    }
}
