//! Canonical Go schedule identity and interval arithmetic, not a scheduler.
use sha2::{Digest, Sha256};

/// Existing schedule.ChildID: first sixteen digest bytes, lowercase hex.
pub fn child_id(parent: &str, index: i32) -> String {
    let sum = Sha256::digest(format!("{parent}:{index}").as_bytes());
    let mut result = String::with_capacity(32);
    const HEX: &[u8; 16] = b"0123456789abcdef";
    for byte in &sum[..16] {
        result.push(HEX[(byte >> 4) as usize] as char);
        result.push(HEX[(byte & 15) as usize] as char);
    }
    result
}

/// Epoch nanoseconds permit canonical Timestamp endpoints without truncation.
/// Go time.Sub saturates to Duration's positive maximum before the floor division.
pub fn interval(start: i128, end: i128, slices: i32, index: i32) -> Option<(i128, i128)> {
    if slices <= 0 || index < 0 || index >= slices || end <= start {
        return None;
    }
    let span = end.checked_sub(start)?.min(i64::MAX as i128) as u128;
    let boundary = |i: i32| start.checked_add((span * i as u128 / slices as u128) as i128);
    Some((boundary(index)?, boundary(index + 1)?))
}
