fn main() -> Result<(), Box<dyn std::error::Error>> {
    let root = "../../../kanz-schemas/proto";
    println!("cargo:rerun-if-changed={root}");
    let mut config = prost_build::Config::new();
    config.file_descriptor_set_path(
        std::path::PathBuf::from(std::env::var("OUT_DIR")?).join("venue-descriptors.bin"),
    );
    config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);
    config.extern_path(".common.v1", "::kanz_decimal_compat::common::v1");
    config.extern_path(".order.v1", "::kanz_order_core::proto::order::v1");
    config.compile_protos(&[format!("{root}/venue/v1/venue.proto")], &[root])?;
    Ok(())
}
