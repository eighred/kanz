#![forbid(unsafe_code)]
use kanz_oms_fold_shadow::{
    evaluate, evaluate_terminal,
    proto::oms::v1::{FillFoldRequest, TerminalFoldRequest},
};
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
    // Prost drops unknown fields: refuse any non-canonical input instead of
    // silently dropping metadata from candidate state. Go sends deterministic wire.
    let args: Vec<String> = std::env::args().skip(1).collect();
    let output = match args.as_slice() {
        [] => {
            let request = FillFoldRequest::decode(wire.as_slice())?;
            if request.encode_to_vec() != wire {
                return Err("unsupported wire metadata".into());
            }
            evaluate(&request, version).encode_to_vec()
        }
        [mode] if mode == "--terminal" => {
            let request = TerminalFoldRequest::decode(wire.as_slice())?;
            if request.encode_to_vec() != wire {
                return Err("unsupported wire metadata".into());
            }
            evaluate_terminal(&request, version).encode_to_vec()
        }
        _ => return Err("unsupported operation mode".into()),
    };
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
