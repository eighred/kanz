//! Compatibility foundation only: no runtime caller or admission authority.
//! Missing fields follow Go internal/dec predicates; requiredness belongs to callers.
pub mod common {
    pub mod v1 {
        include!(concat!(env!("OUT_DIR"), "/common.v1.rs"));
    }
}
use common::v1::Decimal;
use std::cmp::Ordering;

pub fn in_domain(value: Option<&Decimal>) -> bool {
    value.is_none_or(|d| (-64..=64).contains(&d.exponent))
}
pub fn is_positive(value: Option<&Decimal>) -> bool {
    value.is_some_and(|d| d.coefficient > 0)
}
pub fn is_zero(value: Option<&Decimal>) -> bool {
    value.is_none_or(|d| d.coefficient == 0)
}
/// Exact and total over every signed wire exponent, including out-of-domain inputs.
/// Comparison does not authorize conversion or establish field presence.
pub fn compare(a: Option<&Decimal>, b: Option<&Decimal>) -> Ordering {
    let (ca, ea) = a.map_or((0, 0), |d| (d.coefficient, d.exponent));
    let (cb, eb) = b.map_or((0, 0), |d| (d.coefficient, d.exponent));
    match ca.signum().cmp(&cb.signum()) {
        Ordering::Equal => {}
        other => return other,
    }
    if ca == 0 {
        return Ordering::Equal;
    }
    let mut ma = u128::from(ca.unsigned_abs());
    let mut mb = u128::from(cb.unsigned_abs());
    let oa = i64::from(ma.ilog10()) + 1 + i64::from(ea);
    let ob = i64::from(mb.ilog10()) + 1 + i64::from(eb);
    let magnitude = if oa != ob {
        oa.cmp(&ob)
    } else {
        // Equal decimal orders imply exponent gap <=18. Aligned magnitude has
        // at most 19 digits, so neither powers nor multiplication can overflow.
        let gap = i64::from(ea) - i64::from(eb);
        if gap > 0 {
            ma *= 10_u128.pow(gap as u32);
        }
        if gap < 0 {
            mb *= 10_u128.pow((-gap) as u32);
        }
        ma.cmp(&mb)
    };
    if ca < 0 {
        magnitude.reverse()
    } else {
        magnitude
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    fn d(coefficient: i64, exponent: i32) -> Decimal {
        Decimal {
            coefficient,
            exponent,
        }
    }
    #[test]
    fn missing_is_not_requiredness() {
        assert!(in_domain(None));
        assert!(is_zero(None));
        assert!(!is_positive(None));
        assert_eq!(compare(None, Some(&d(0, i32::MAX))), Ordering::Equal);
    }
    #[test]
    fn domain_bound_includes_zero() {
        for e in [-64, 64] {
            assert!(in_domain(Some(&d(1, e))));
        }
        for e in [i32::MIN, -65, 65, i32::MAX] {
            assert!(!in_domain(Some(&d(0, e))));
        }
    }
    #[test]
    fn equivalent_representations_compare_equal() {
        for c in [-123, 1, 123] {
            assert_eq!(
                compare(Some(&d(c, 0)), Some(&d(c * 100, -2))),
                Ordering::Equal
            );
        }
    }
    #[test]
    fn extreme_exponents_do_not_expand() {
        assert_eq!(
            compare(Some(&d(i64::MAX, i32::MIN)), Some(&d(1, i32::MAX))),
            Ordering::Less
        );
        assert_eq!(
            compare(Some(&d(i64::MIN, i32::MAX)), Some(&d(-1, i32::MIN))),
            Ordering::Less
        );
        assert_eq!(
            compare(Some(&d(i64::MIN, 0)), Some(&d(-922337203685477580, 1))),
            Ordering::Less
        );
    }
    #[test]
    fn comparison_is_antisymmetric() {
        for c in [i64::MIN, -10, -1, 0, 1, 10, i64::MAX] {
            for e in [i32::MIN, -64, 0, 64, i32::MAX] {
                let a = d(c, e);
                for b in [d(1, 0), d(-1, 0), d(c, 0)] {
                    assert_eq!(
                        compare(Some(&a), Some(&b)),
                        compare(Some(&b), Some(&a)).reverse()
                    );
                }
            }
        }
    }
}
