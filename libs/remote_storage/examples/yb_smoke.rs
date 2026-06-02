//! Smoke test for `YbStore`. Run with:
//!
//!   cargo run -p remote_storage --example yb_smoke -- \
//!       --host 127.0.0.1 --port 5433 --user yugabyte \
//!       --dbname neon_storage --table pageserver_objects
//!
//! Exercises upload -> head -> download -> list -> copy -> delete against a
//! live YB/Postgres instance. Exits 0 on success, 1 on failure.

use std::num::NonZeroUsize;
use std::time::Duration;

use bytes::Bytes;
use remote_storage::{
    DownloadOpts, GenericRemoteStorage, ListingMode, RemotePath, RemoteStorageConfig,
    RemoteStorageKind, YbStoreConfig,
};
use tokio_util::sync::CancellationToken;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let mut args = std::env::args().skip(1);
    let mut host = "127.0.0.1".to_string();
    let mut port: u16 = 5433;
    let mut user = "yugabyte".to_string();
    let mut dbname = "neon_storage".to_string();
    let mut table = "pageserver_objects".to_string();
    let mut prefix: Option<String> = Some(format!("smoke-{}/", uuid::Uuid::new_v4()));

    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--host" => host = args.next().unwrap(),
            "--port" => port = args.next().unwrap().parse().unwrap(),
            "--user" => user = args.next().unwrap(),
            "--dbname" => dbname = args.next().unwrap(),
            "--table" => table = args.next().unwrap(),
            "--prefix" => prefix = Some(args.next().unwrap()),
            other => anyhow::bail!("unknown arg: {other}"),
        }
    }

    let config = RemoteStorageConfig {
        storage: RemoteStorageKind::YbStore(YbStoreConfig {
            host,
            port,
            user,
            password_env: None,
            dbname,
            table_name: table,
            prefix,
            concurrency_limit: NonZeroUsize::new(16).unwrap(),
            sslmode: None,
            sslrootcert: None,
        }),
        timeout: Duration::from_secs(60),
        small_timeout: Duration::from_secs(10),
    };
    let storage = GenericRemoteStorage::from_config(&config).await?;
    let cancel = CancellationToken::new();

    let key_a = RemotePath::from_string("a/foo.bin").unwrap();
    let key_b = RemotePath::from_string("a/bar.bin").unwrap();
    let key_c = RemotePath::from_string("b/baz.bin").unwrap();

    // Upload three keys.
    let payload_a: Vec<u8> = (0u8..=255).cycle().take(64 * 1024).collect();
    let payload_b: Vec<u8> = b"hello from yb_smoke".to_vec();
    let payload_c: Vec<u8> = vec![0xAB; 1024];

    for (k, v) in [(&key_a, &payload_a), (&key_b, &payload_b), (&key_c, &payload_c)] {
        let size = v.len();
        let bytes = Bytes::copy_from_slice(v);
        let stream =
            futures::stream::once(async move { Ok::<_, std::io::Error>(bytes) });
        storage
            .upload(stream, size, k, None, &cancel)
            .await
            .map_err(|e| anyhow::anyhow!("upload {k} failed: {e:#}"))?;
        println!("uploaded {k} ({size} bytes)");
    }

    // Head + download payload_a, verify round-trip.
    let head = storage.head_object(&key_a, &cancel).await?;
    println!("head {key_a}: size={} last_modified={:?}", head.size, head.last_modified);
    anyhow::ensure!(head.size as usize == payload_a.len(), "head size mismatch");

    let mut dl = storage.download(&key_a, &DownloadOpts::default(), &cancel).await?;
    let mut got: Vec<u8> = Vec::new();
    use futures::StreamExt;
    while let Some(chunk) = dl.download_stream.next().await {
        got.extend_from_slice(&chunk?);
    }
    anyhow::ensure!(got == payload_a, "round-trip mismatch on {key_a}");
    println!("round-trip OK for {key_a}");

    // List with delimiter — expect `a/` and `b/` as prefixes when listing root.
    let root_listing = storage
        .list(None, ListingMode::WithDelimiter, None, &cancel)
        .await?;
    println!(
        "list root WithDelimiter: prefixes={:?} keys={:?}",
        root_listing.prefixes.iter().map(|p| p.to_string()).collect::<Vec<_>>(),
        root_listing.keys.iter().map(|o| o.key.to_string()).collect::<Vec<_>>()
    );

    // List under a/ — expect the two keys.
    let prefix_a = RemotePath::from_string("a/").unwrap();
    let a_listing = storage
        .list(Some(&prefix_a), ListingMode::NoDelimiter, None, &cancel)
        .await?;
    println!(
        "list a/ NoDelimiter: {} keys",
        a_listing.keys.len()
    );
    anyhow::ensure!(a_listing.keys.len() == 2, "expected 2 keys under a/");

    // Copy + verify.
    let key_b_copy = RemotePath::from_string("a/bar-copy.bin").unwrap();
    storage.copy_object(&key_b, &key_b_copy, &cancel).await?;
    let mut dl2 = storage
        .download(&key_b_copy, &DownloadOpts::default(), &cancel)
        .await?;
    let mut got2: Vec<u8> = Vec::new();
    while let Some(chunk) = dl2.download_stream.next().await {
        got2.extend_from_slice(&chunk?);
    }
    anyhow::ensure!(got2 == payload_b, "copy contents mismatch");
    println!("copy OK");

    // Range download.
    let mut opts = DownloadOpts::default();
    opts.byte_start = std::ops::Bound::Included(100);
    opts.byte_end = std::ops::Bound::Excluded(200);
    let mut dl3 = storage.download(&key_a, &opts, &cancel).await?;
    let mut got3: Vec<u8> = Vec::new();
    while let Some(chunk) = dl3.download_stream.next().await {
        got3.extend_from_slice(&chunk?);
    }
    anyhow::ensure!(got3 == payload_a[100..200], "range slice mismatch");
    println!("range download OK");

    // Delete one + delete_objects for the rest.
    storage.delete(&key_c, &cancel).await?;
    storage
        .delete_objects(&[key_a.clone(), key_b.clone(), key_b_copy.clone()], &cancel)
        .await?;
    let after = storage
        .list(None, ListingMode::NoDelimiter, None, &cancel)
        .await?;
    anyhow::ensure!(after.keys.is_empty(), "expected empty after delete, got {} keys", after.keys.len());
    println!("delete + delete_objects OK");

    println!("ALL OK");
    Ok(())
}
