//! Exact pinned Go strings.ToLower(strings.TrimSpace(...)) compatibility.
//! This is normalization only, not authentication or approval authority.
use crate::go_unicode::{LOWER, SPACE};
use sha2::{Digest, Sha256};

pub fn normalize(subject: &str) -> String {
    trim(subject)
        .chars()
        .map(
            |c| match LOWER.binary_search_by_key(&(c as u32), |&(key, _)| key) {
                Ok(i) => char::from_u32(LOWER[i].1).expect("generated valid scalar"),
                Err(_) => c,
            },
        )
        .collect()
}
/// Pinned Go strings.TrimSpace, shared by command and maker/checker validation.
pub fn trim(subject: &str) -> &str {
    subject.trim_matches(|c: char| SPACE.binary_search(&(c as u32)).is_ok())
}
/// Test evidence over every Unicode scalar; no subject values are logged.
pub fn scalar_digest() -> String {
    let mut hash = Sha256::new();
    for scalar in 0..=0x10ffff {
        if let Some(c) = char::from_u32(scalar) {
            let value = normalize(&c.to_string());
            hash.update(scalar.to_be_bytes());
            hash.update((value.len() as u64).to_be_bytes());
            hash.update(value.as_bytes());
        }
    }
    format!("{:x}", hash.finalize())
}
