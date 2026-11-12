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
/// Checked existing Go ToProtoScaled conversion for derived rounded outputs.
pub fn decimal(value: BigRational) -> Option<Decimal> {
    decimal_fit(value, 64)
}

/// Go dec.ToProtoExact's bounded exact contract: a normalized i64
/// coefficient, exponent [-60,60], and no rounding or implicit zero. Keep this
/// distinct from the established average-price output conversion.
pub fn exact_decimal(value: BigRational) -> Option<Decimal> {
    if value.numer().bits() > 512 || value.denom().bits() > 512 {
        return None;
    }
    if value.is_zero() {
        return Some(Decimal {
            coefficient: 0,
            exponent: 0,
        });
    }
    let (twos, fives) = decimal_factors(&value)?;
    let places = twos.max(fives);
    if places > 128 {
        return None;
    }
    let mut coefficient =
        value.numer() * BigInt::from(2).pow(places - twos) * BigInt::from(5).pow(places - fives);
    let mut exponent = -(places as i32);
    while (&coefficient % 10_u32).is_zero() {
        coefficient /= 10_u32;
        exponent += 1;
    }
    if !(-60..=60).contains(&exponent) {
        return None;
    }
    Some(Decimal {
        coefficient: coefficient.to_i64()?,
        exponent,
    })
}

/// Distinguishes a terminating observation from derived repeating arithmetic.
/// This is not representability: an oversized finite value must still refuse
/// exact conversion rather than fall back to a rounded success.
pub fn has_terminating_decimal(value: &BigRational) -> bool {
    decimal_factors(value).is_some()
}

fn decimal_factors(value: &BigRational) -> Option<(u32, u32)> {
    let mut denominator = value.denom().clone();
    let mut twos = 0;
    let mut fives = 0;
    while (&denominator % 2_u32).is_zero() {
        denominator /= 2_u32;
        twos += 1;
    }
    while (&denominator % 5_u32).is_zero() {
        denominator /= 5_u32;
        fives += 1;
    }
    (denominator == BigInt::from(1)).then_some((twos, fives))
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
    fn exact_quantity_domain_preserves_or_refuses() {
        for (n, d, coefficient, exponent) in [
            (1_i64, 1_000_000_000_i64, 1, -9),
            (-9, 1_000_000_000, -9, -9),
            (10, 1, 1, 1),
            (0, 1, 0, 0),
            (i64::MAX, 1, i64::MAX, 0),
            (i64::MIN, 1, i64::MIN, 0),
        ] {
            assert_eq!(
                exact_decimal(BigRational::new(n.into(), d.into())),
                Some(Decimal {
                    coefficient,
                    exponent
                })
            );
        }
        for exponent in [-60_i32, 60] {
            let value = rational(Some(&Decimal {
                coefficient: 1,
                exponent,
            }))
            .unwrap();
            assert_eq!(
                exact_decimal(value),
                Some(Decimal {
                    coefficient: 1,
                    exponent
                })
            );
        }
        for exponent in [-61_i32, 61] {
            assert!(exact_decimal(
                rational(Some(&Decimal {
                    coefficient: 1,
                    exponent
                }))
                .unwrap()
            )
            .is_none());
        }
        for value in [
            BigRational::new(1.into(), 3.into()),
            BigRational::from_integer(BigInt::from(i64::MAX) + BigInt::from(1)),
            BigRational::from_integer(BigInt::from(1) << 512_usize),
            BigRational::new(1.into(), BigInt::from(1) << 129_usize),
        ] {
            assert!(exact_decimal(value).is_none());
        }
    }
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
