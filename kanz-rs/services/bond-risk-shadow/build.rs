fn main() -> Result<(), Box<dyn std::error::Error>> {
    let root = "../../../kanz-schemas/proto";
    for file in [
        "quant/v1/bond_risk.proto",
        "reference/v1/fixed_income.proto",
        "reference/v1/conventions.proto",
        "domain/v1/calibration.proto",
    ] {
        println!("cargo:rerun-if-changed={root}/{file}");
    }
    println!("cargo:rerun-if-env-changed=KANZ_BUILD_VERSION");
    let mut config = prost_build::Config::new();
    config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);
    config.extern_path(".common.v1", "::kanz_decimal_compat::common::v1");
    config.compile_protos(&[format!("{root}/quant/v1/bond_risk.proto")], &[root])?;
    Ok(())
}
