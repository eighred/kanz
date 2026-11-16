use serde::Deserialize;
use std::collections::BTreeSet;

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Corpus {
    schema: u32,
    method: String,
    source_sha: String,
    case_count: usize,
    pub cases: Vec<Case>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Case {
    pub id: String,
    pub right: u8,
    pub exercise: u8,
    pub input_bits: [u64; 6],
    pub outcome: String,
    pub values_bits: Option<[u64; 6]>,
}

pub fn capture(raw: &[u8], source: &str, method: &str, count: usize) -> Result<Corpus, String> {
    if raw.len() > 8 * 1024 * 1024
        || source.len() != 40
        || !source
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
    {
        return Err("invalid capture/source bounds".into());
    }
    let c: Corpus = serde_json::from_slice(raw).map_err(|_| "malformed capture")?;
    if c.schema != 1
        || c.method != method
        || c.source_sha != source
        || c.case_count != count
        || c.cases.len() != count
    {
        return Err("capture identity/inventory mismatch".into());
    }
    let mut ids = BTreeSet::new();
    for row in &c.cases {
        if row.id.is_empty() || row.id.len() > 128 || !ids.insert(&row.id) {
            return Err("missing/duplicate case identity".into());
        }
    }
    Ok(c)
}
