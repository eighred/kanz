fn main() -> Result<(), Box<dyn std::error::Error>> {
    let root = "../../../kanz-schemas/proto";
    println!("cargo:rerun-if-changed={root}");
    println!("cargo:rerun-if-env-changed=KANZ_BUILD_VERSION");
    let mut config = prost_build::Config::new();
    config.file_descriptor_set_path(
        std::path::PathBuf::from(std::env::var("OUT_DIR")?).join("oms-descriptors.bin"),
    );
    config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);
    config.extern_path(".common.v1", "::kanz_decimal_compat::common::v1");
    config.compile_protos(
        &[
            format!("{root}/oms/v1/fold_shadow.proto"),
            format!("{root}/envelope/v1/event_frame.proto"),
            format!("{root}/domain/v1/portfolio.proto"),
            format!("{root}/command/v1/command_outcome.proto"),
        ],
        &[root],
    )?;
    Ok(())
}
