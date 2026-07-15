//! Compile the vendored YugabyteDB protos (see `protos/README.md`) with
//! prost. Uses `protoc-bin-vendored` because the system protoc on this
//! distro (3.5) predates the proto3 `optional` label used in
//! `pg_client.proto`.

use std::env;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let protoc = protoc_bin_vendored::protoc_bin_path()?;
    // SAFETY: build.rs runs single-threaded before any other code.
    unsafe {
        env::set_var("PROTOC", &protoc);
    }
    prost_build::Config::new().compile_protos(
        &[
            "protos/yb/tserver/pg_client.proto",
            "protos/yb/rpc/rpc_header.proto",
        ],
        &[
            "protos",
            protoc_bin_vendored::include_path()?.to_str().unwrap(),
        ],
    )?;
    Ok(())
}
