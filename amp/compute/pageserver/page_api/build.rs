use std::env;
use std::path::PathBuf;

/// Generates Rust code from .proto Protobuf schemas, along with a binary file
/// descriptor set for Protobuf schema reflection.
///
/// Uses `protoc-bin-vendored` so the build doesn't depend on a system protoc
/// of any particular version. The vendored bundle also ships the well-known
/// proto types (e.g. `google/protobuf/timestamp.proto`) on its include path,
/// so imports of those resolve without any extra files in the source tree.
fn main() -> Result<(), Box<dyn std::error::Error>> {
    let out_dir = PathBuf::from(env::var("OUT_DIR")?);

    let protoc = protoc_bin_vendored::protoc_bin_path()?;
    let protoc_include = protoc_bin_vendored::include_path()?;
    // SAFETY: build.rs is single-threaded and runs before any user code.
    unsafe {
        env::set_var("PROTOC", &protoc);
    }

    tonic_build::configure()
        .bytes(["."])
        .file_descriptor_set_path(out_dir.join("page_api_descriptor.bin"))
        .compile_protos(
            &["proto/page_service.proto"],
            &["proto", protoc_include.to_str().expect("UTF-8 path")],
        )
        .map_err(|err| err.into())
}
