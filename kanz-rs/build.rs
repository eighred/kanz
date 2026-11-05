fn main() -> Result<(), Box<dyn std::error::Error>> {
    let schema = "../kanz-schemas/proto/common/v1/decimal.proto";
    println!("cargo:rerun-if-changed={schema}");
    let mut config = prost_build::Config::new();
    config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);
    let money = "../kanz-schemas/proto/common/v1/money.proto";
    println!("cargo:rerun-if-changed={money}");
    config.compile_protos(&[schema, money], &["../kanz-schemas/proto"])?;
    Ok(())
}
