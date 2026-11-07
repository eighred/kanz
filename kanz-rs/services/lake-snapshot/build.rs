fn main() -> Result<(), Box<dyn std::error::Error>> {
    println!("cargo:rerun-if-env-changed=KANZ_BUILD_VERSION");
    let root = "../../../kanz-schemas/proto";
    for file in [
        "lake/worker/v1/worker.proto",
        "lake/v1/snapshot.proto",
        "envelope/v1/envelope.proto",
        "envelope/v1/event_class.proto",
    ] {
        println!("cargo:rerun-if-changed={root}/{file}");
    }
    let mut config = prost_build::Config::new();
    config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);
    config.compile_protos(
        &[
            format!("{root}/lake/worker/v1/worker.proto"),
            format!("{root}/envelope/v1/envelope.proto"),
        ],
        &[root],
    )?;
    Ok(())
}
