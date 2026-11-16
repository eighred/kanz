use crate::proto::common::v1::Decimal;
use num_bigint::BigInt;
use num_rational::BigRational;
use num_traits::{Signed, ToPrimitive, Zero};

/// Absent means legacy aggregate zero, never an admission-state default.
pub fn rational(value: Option<&Decimal>) -> Option<BigRational> {
    let Some(d) = value else {
        return Some(BigRational::zero());
    };
    if !(-64..=64).contains(&d.exponent) {
        return None;
    }
    let pow = BigInt::from(10).pow(d.exponent.unsigned_abs());
    Some(if d.exponent >= 0 {
        BigRational::from_integer(BigInt::from(d.coefficient) * pow)
    } else {
        BigRational::new(BigInt::from(d.coefficient), pow)
    })
}
fn rounded(n: BigInt, d: &BigInt) -> BigInt {
    let mut q = &n / d;
    let rem = &n % d;
    if rem.abs() * 2 >= d.abs() {
        if n.is_negative() {
            q -= 1
        } else {
            q += 1
        }
    }
    q
}
// Go dec.ToProtoScaled: first round at 1e-8, then repeated half-away /10.
/// Checked existing Go ToProtoScaled conversion shared with position outputs.
pub fn decimal(value: BigRational) -> Option<Decimal> {
    decimal_fit(value, 64)
}

/// Diagnostic Go-wire candidate only. The caller bounds rational inputs and
/// separately classifies financial-domain refusal; no existing caller uses this.
pub(crate) fn attribution_go_wire_decimal(value: BigRational) -> Option<Decimal> {
    decimal_fit(value, i32::MAX)
}

fn decimal_fit(value: BigRational, maximum_exponent: i32) -> Option<Decimal> {
    let mut q = rounded(value.numer() * BigInt::from(100_000_000), value.denom());
    let mut exponent: i32 = -8;
    loop {
        if let Some(coefficient) = q.to_i64() {
            return Some(Decimal {
                coefficient,
                exponent,
            });
        }
        q = rounded(q, &BigInt::from(10));
        exponent = exponent.checked_add(1)?;
        if exponent > maximum_exponent {
            return None;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn go_wire_candidate_does_not_expand_existing_financial_output_domain() {
        let cost = BigRational::from_integer(BigInt::from(10).pow(132) - BigInt::from(10_000));
        assert!(decimal(cost.clone()).is_none());
        let candidate = attribution_go_wire_decimal(cost).unwrap();
        assert_eq!(
            (candidate.coefficient, candidate.exponent),
            (1_000_000_000_000_000_000, 114)
        );
        assert!(!kanz_decimal_compat::in_domain(Some(&candidate)));
        for (numerator, expected) in [(1, 33_333_333), (-1, -33_333_333)] {
            let value = BigRational::new(numerator.into(), 3.into());
            assert_eq!(decimal(value.clone()), attribution_go_wire_decimal(value));
            assert_eq!(
                decimal(BigRational::new(numerator.into(), 3.into()))
                    .unwrap()
                    .coefficient,
                expected
            );
        }
    }
}
