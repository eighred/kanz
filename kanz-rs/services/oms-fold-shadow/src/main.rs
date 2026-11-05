#![forbid(unsafe_code)]
use kanz_oms_fold_shadow::{evaluate, proto::oms::v1::FillFoldRequest};
use prost::Message;
use std::io::{Read, Write};
const LIMIT: usize = 1 << 20;
fn run() -> Result<(), Box<dyn std::error::Error>> {
    let version = option_env!("KANZ_BUILD_VERSION")
        .filter(|v| !v.trim().is_empty())
        .ok_or("unstamped")?;
    let mut wire = Vec::new();
    std::io::stdin()
        .take((LIMIT + 1) as u64)
        .read_to_end(&mut wire)?;
    if wire.is_empty() || wire.len() > LIMIT {
        return Err("input bound".into());
    }
    let request = FillFoldRequest::decode(wire.as_slice())?;
    // Prost drops unknown fields: refuse any non-canonical input instead of
    // silently dropping metadata from candidate state. Go sends deterministic wire.
    if request.encode_to_vec() != wire {
        return Err("unsupported wire metadata".into());
    }
    let output = evaluate(&request, version).encode_to_vec();
    if output.len() > LIMIT {
        return Err("output bound".into());
    }
    std::io::stdout().lock().write_all(&output)?;
    Ok(())
}
fn main() {
    if run().is_err() {
        eprintln!("OMS fold shadow refused request");
        std::process::exit(1)
    }
}
