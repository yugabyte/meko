use std::env;

/// Generate Rust code from .proto Protobuf schemas. Uses
/// `protoc-bin-vendored` so the build doesn't depend on the system
/// protoc (which on RHEL/AlmaLinux is too old to parse proto3
/// `optional` labels).
fn main() -> Result<(), Box<dyn std::error::Error>> {
    let protoc = protoc_bin_vendored::protoc_bin_path()?;
    // SAFETY: build.rs is single-threaded and runs before any user code.
    unsafe {
        env::set_var("PROTOC", &protoc);
    }
    tonic_build::compile_protos("proto/broker.proto")
        .unwrap_or_else(|e| panic!("failed to compile protos {e:?}"));
    Ok(())
}
