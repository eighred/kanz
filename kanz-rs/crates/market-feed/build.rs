fn main() -> Result<(), Box<dyn std::error::Error>> {
    let root = "../../../kanz-schemas/proto";
    let schemas = [
        "market/v1/order_book.proto",
        "market/v1/market_data.proto",
        "envelope/v1/event_frame.proto",
    ];
    for schema in schemas {
        println!("cargo:rerun-if-changed={root}/{schema}");
    }
    println!("cargo:rerun-if-changed={root}/envelope/v1/envelope.proto");
    println!("cargo:rerun-if-changed={root}/envelope/v1/event_class.proto");
    let inputs: Vec<String> = schemas.iter().map(|s| format!("{root}/{s}")).collect();
    let mut config = prost_build::Config::new();
    config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);
    config.extern_path(".common.v1", "::kanz_decimal_compat::common::v1");
    config.compile_protos(&inputs, &[root])?;
    Ok(())
}
