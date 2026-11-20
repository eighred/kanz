//! Canonical Go schedule identity and interval arithmetic, not a scheduler.
use sha2::{Digest, Sha256};

/// Existing schedule.ChildID: first sixteen digest bytes, lowercase hex.
pub fn child_id(parent: &str, index: i64) -> String {
    ChildIds::new(parent).at(index)
}

/// Hash the potentially large parent exactly once; each index clones only the
/// fixed-size SHA256 state. This changes no bytes in Go's parent:decimal-index.
pub struct ChildIds(Sha256);
impl ChildIds {
    pub fn new(parent: &str) -> Self {
        let mut prefix = Sha256::new();
        prefix.update(parent.as_bytes());
        prefix.update(b":");
        Self(prefix)
    }
    pub fn at(&self, index: i64) -> String {
        let mut state = self.0.clone();
        state.update(index.to_string().as_bytes());
        let sum = state.finalize();
        crate::lowercase_hex(&sum[..16])
    }
}

/// Epoch nanoseconds permit canonical Timestamp endpoints without truncation.
/// Match Go Plan.Interval: reject a span that cannot fit time.Duration rather
/// than measuring a shorter window after time.Sub saturates.
pub fn interval(start: i128, end: i128, slices: i64, index: i64) -> Option<(i128, i128)> {
    if slices <= 0 || index < 0 || index >= slices || end <= start {
        return None;
    }
    let span = end.checked_sub(start)?;
    if span > i64::MAX as i128 {
        return None;
    }
    let span = span as u128;
    let boundary = |i: i64| start.checked_add((span * i as u128 / slices as u128) as i128);
    Some((boundary(index)?, boundary(index + 1)?))
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn full_and_truncated_digests_preserve_leading_zero_bytes() {
        let digest = Sha256::digest(b"fixture:5476");
        assert_eq!(
            crate::lowercase_hex(&digest),
            "00078eb3c539b05ee4452da1634368bb2d554e5237944a566bc6c82896e16d45"
        );
        assert_eq!(
            child_id("fixture", 5476),
            "00078eb3c539b05ee4452da1634368bb"
        );
    }
    #[test]
    fn prefix_clone_preserves_long_parent_and_signed_index_bytes() {
        let parent = "α:".repeat(140_000);
        let ids = ChildIds::new(&parent);
        for index in [i64::MIN, -1, 0, 1, 99_999, i64::MAX] {
            let direct = Sha256::digest(format!("{parent}:{index}").as_bytes());
            assert_eq!(ids.at(index), crate::lowercase_hex(&direct[..16]));
        }
        // Exercise the supported index boundary using a single prefix owner.
        // The source guard rejects moving parent hashing into this iteration.
        for index in 0..100_000 {
            assert_eq!(ids.at(index).len(), 32);
        }
    }
    #[test]
    fn boundaries_floor_once_and_refuse_unrepresentable_duration() {
        assert_eq!(interval(0, 10, 3, 0), Some((0, 3)));
        assert_eq!(interval(0, 10, 3, 1), Some((3, 6)));
        assert_eq!(interval(0, 10, 3, 2), Some((6, 10)));
        assert_eq!(
            interval(-1, i128::from(i64::MAX) - 1, 2, 1),
            Some((4_611_686_018_427_387_902, 9_223_372_036_854_775_806))
        );
        assert_eq!(interval(-1, i128::from(i64::MAX), 2, 1), None);
        assert_eq!(interval(-1, i128::from(i64::MAX) + 20, 2, 1), None);
        assert_eq!(interval(i128::MIN, i128::MAX, 2, 1), None);
        assert_eq!(interval(0, 10, 3, 3), None);
        assert_eq!(interval(0, 0, 3, 0), None);
    }
}
