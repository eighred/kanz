mod support;
use serde::Deserialize;
use std::{
    collections::{BTreeMap, BTreeSet},
    fs::File,
    io::Read,
};

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Corpus {
    schema: u32,
    method: String,
    source_sha: String,
    case_count: usize,
    cases: Vec<Case>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Case {
    id: String,
    mode: u8,
    comp: u8,
    interp: u8,
    tenors: Vec<u64>,
    rates: Vec<u64>,
    queries: Vec<u64>,
    forwards: Vec<[u64; 2]>,
    coupons: Vec<u64>,
    outcome: String,
    values: Option<Vec<u64>>,
}
fn floats(xs: Vec<u64>) -> Vec<f64> {
    xs.into_iter().map(f64::from_bits).collect()
}

fn verify(raw: &[u8], source: &str) -> Result<BTreeMap<String, usize>, String> {
    if raw.len() > 8 * 1024 * 1024
        || source.len() != 40
        || !source
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
    {
        return Err("invalid capture bounds/source".into());
    }
    let data: Corpus = serde_json::from_slice(raw).map_err(|_| "malformed capture")?;
    if data.schema != 1
        || data.method != "go_zero_par_v2"
        || data.source_sha != source
        || data.case_count != 876
        || data.cases.len() != 876
    {
        return Err("capture identity/inventory mismatch".into());
    }
    let mut ids = BTreeSet::new();
    let mut counts = BTreeMap::new();
    for row in data.cases {
        if row.id.is_empty()
            || row.id.len() > 128
            || !ids.insert(row.id.clone())
            || row.tenors.len() > 256
            || row.rates.len() > 256
            || row.queries.len() > 32
            || row.forwards.len() > 16
            || row.coupons.len() > 4096
        {
            return Err("case identity/bounds refused".into());
        }
        let ts = floats(row.tenors);
        let rs = floats(row.rates);
        let qs = floats(row.queries);
        let fs: Vec<_> = row
            .forwards
            .into_iter()
            .map(|p| p.map(f64::from_bits))
            .collect();
        let cp = floats(row.coupons);
        let built = support::build(row.mode, row.comp, row.interp, &ts, &rs);
        match (row.outcome.as_str(), row.values, built) {
            ("refused", None, Err(_)) => (),
            ("unavailable", None, Ok(c)) => {
                if support::evaluate(&c, &qs, &fs, &cp).is_ok() {
                    return Err(format!("{}: unavailable became a candidate", row.id));
                }
            }
            ("value", Some(bits), Ok(c)) => {
                if bits.len() != 2 * ts.len() + 3 * qs.len() + fs.len() + 1 {
                    return Err("result field inventory differs".into());
                }
                let actual = support::evaluate(&c, &qs, &fs, &cp)
                    .map_err(|e| format!("{}: {e:?}", row.id))?;
                support::compare(&actual, &floats(bits), ts.len(), qs.len())
                    .map_err(|e| format!("{}: {e}", row.id))?;
            }
            _ => return Err(format!("{}: outcome/presence differs", row.id)),
        }
        *counts.entry(row.outcome).or_insert(0) += 1;
    }
    if counts
        != BTreeMap::from([
            ("value".into(), 832),
            ("refused".into(), 29),
            ("unavailable".into(), 15),
        ])
    {
        return Err("outcome inventory differs".into());
    }
    Ok(counts)
}

#[test]
#[ignore = "requires a fresh Go capture and exact source SHA"]
fn fresh_go_oracle_is_required() {
    let path = std::env::var("KANZ_CURVE_CORPUS").expect("fresh capture required");
    let source = std::env::var("KANZ_CURVE_SOURCE").expect("exact source required");
    let mut raw = Vec::new();
    File::open(path)
        .unwrap()
        .take(8 * 1024 * 1024 + 1)
        .read_to_end(&mut raw)
        .unwrap();
    let counts = verify(&raw, &source).unwrap();
    let original: serde_json::Value = serde_json::from_slice(&raw).unwrap();
    let mut changed = original.clone();
    changed["source_sha"] = serde_json::json!("0".repeat(40));
    assert!(verify(&serde_json::to_vec(&changed).unwrap(), &source).is_err());
    changed = original.clone();
    let n = changed["cases"][0]["tenors"].as_array().unwrap().len();
    changed["cases"][0]["values"][n] = serde_json::json!(0);
    assert!(verify(&serde_json::to_vec(&changed).unwrap(), &source).is_err());
    changed = original.clone();
    changed["cases"][1]["id"] = changed["cases"][0]["id"].clone();
    assert!(verify(&serde_json::to_vec(&changed).unwrap(), &source).is_err());
    changed = original.clone();
    changed["cases"].as_array_mut().unwrap().pop();
    changed["case_count"] = serde_json::json!(875);
    assert!(verify(&serde_json::to_vec(&changed).unwrap(), &source).is_err());
    changed = original;
    changed["method"] = "go_zero_par_v1".into();
    assert!(verify(&serde_json::to_vec(&changed).unwrap(), &source).is_err());
    println!("fresh Go curve source={source} cases=876 outcomes={counts:?} negative_controls=5");
}

#[test]
fn missing_or_malformed_capture_refuses() {
    for bytes in [b"".as_slice(), b"{}", b"{\"schema\":1}"] {
        assert!(verify(bytes, &"a".repeat(40)).is_err());
    }
    assert!(verify(&[], "not-a-revision").is_err());
}
