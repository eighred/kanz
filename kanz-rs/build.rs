fn main() -> Result<(), Box<dyn std::error::Error>> {
    let schema = "../kanz-schemas/proto/common/v1/decimal.proto";
    println!("cargo:rerun-if-changed={schema}");
    let mut config = prost_build::Config::new();
    config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);
    config.compile_protos(&[schema], &["../kanz-schemas/proto"])?;
    Ok(())
}
