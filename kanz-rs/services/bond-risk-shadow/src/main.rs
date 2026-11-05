#![forbid(unsafe_code)]
use kanz_bond_risk_shadow::{evaluate, proto::quant::v1::BondRiskRequest};
use prost::Message;
use std::io::{Read, Write};
const LIMIT: usize = 1 << 20;
fn run() -> Result<(), Box<dyn std::error::Error>> {
    let version = option_env!("KANZ_BUILD_VERSION")
        .filter(|v| !v.trim().is_empty())
        .ok_or("unstamped binary")?;
    let mut bytes = Vec::new();
    std::io::stdin()
        .take((LIMIT + 1) as u64)
        .read_to_end(&mut bytes)?;
    if bytes.is_empty() || bytes.len() > LIMIT {
        return Err("invalid input size".into());
    }
    let request = BondRiskRequest::decode(bytes.as_slice())?;
    let output = evaluate(&request, version).encode_to_vec();
    if output.len() > LIMIT {
        return Err("invalid output size".into());
    }
    std::io::stdout().lock().write_all(&output)?;
    Ok(())
}
fn main() {
    if run().is_err() {
        eprintln!("bond shadow worker refused request");
        std::process::exit(1);
    }
}
